package personal

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message/charset"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

const (
	imapName        = "imap"
	fieldIMAPServer = "server"
	fieldMailbox    = "mailbox"
	maxFetchBytes   = 512 << 10
	maxFetchPerPoll = 300
	fetchBatchSize  = 20
)

// IMAP reads a single mailbox with IMAP over TLS.
type IMAP struct{}

func (IMAP) Name() string { return imapName }

func (IMAP) Info() source.Info {
	return source.Info{
		EventSource: imapName, Title: "IMAP mail", ResultNoun: "Email",
		Summary:         "Email messages from a private IMAP mailbox.",
		ToolDescription: "Email messages from an owner's IMAP mailbox.",
		ActorRole:       "sender", IdentityNamespace: func(string) string { return "email" },
	}
}

func (IMAP) Fields() []settings.Field {
	return append(accountFields(),
		settings.Field{Key: fieldIMAPServer, Label: "IMAP TLS server", Kind: settings.KindText, Required: true,
			Help: "The server and port, such as imap.example.com:993."},
		settings.Field{Key: fieldMailbox, Label: "Mailbox", Kind: settings.KindText, Required: true, Default: "INBOX"})
}

func (IMAP) Validate(values settings.Values) error {
	errs := accountErrors(values)
	host, port, err := net.SplitHostPort(settingsString(values, fieldIMAPServer))
	n, numErr := strconv.Atoi(port)
	if err != nil || numErr != nil || host == "" || n < 1 || n > 65535 {
		errs[fieldIMAPServer] = "enter a hostname and port, such as imap.example.com:993"
	}
	if len(errs) != 0 {
		return errs
	}
	return nil
}

func (IMAP) Test(ctx context.Context, cfg settings.Resolved) settings.TestReport {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	mailbox, err := openMailbox(ctx, cfg)
	check := settings.Check{Name: "Read mailbox", Detail: "Could not authenticate and open the mailbox over TLS.",
		Remedy: "Check the server, mailbox name, credentials, and TLS certificate."}
	if err == nil {
		defer mailbox.close()
		check = settings.Check{Name: check.Name, Passed: true,
			Detail: fmt.Sprintf("Opened the mailbox read-only. It contains %d messages.", mailbox.NumMessages)}
	}
	return settings.TestReport{Checks: []settings.Check{check}}
}

func (IMAP) Run(ctx context.Context, deps source.Deps, cfg settings.Resolved) error {
	return run(ctx, deps, imapName, cfg, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		return syncMailbox(ctx, deps.Sink, cfg)
	})
}

func (IMAP) Expand(ctx context.Context, deps source.Deps, id int64, _ []string) (source.Expanded, error) {
	return expand(ctx, deps, id, imapName)
}

type mailboxConn struct {
	*imap.SelectData
	client *imapclient.Client
	close  func()
}

func openMailbox(ctx context.Context, cfg settings.Resolved) (mailboxConn, error) {
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.String(fieldIMAPServer))
	if err != nil {
		return mailboxConn{}, fmt.Errorf("dial IMAP TLS: %w", err)
	}
	// Closing the socket cancels a pending command on leadership or deadline
	// loss, independent of the IMAP client's per-command timeouts.
	return examineMailbox(ctx, conn, cfg)
}

func examineMailbox(ctx context.Context, conn net.Conn, cfg settings.Resolved) (mailboxConn, error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	client := imapclient.New(conn, &imapclient.Options{WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}})
	closeClient := func() {
		stop()
		_ = client.Close()
	}
	if err := client.Login(cfg.String(fieldUsername), cfg.Secret(fieldPassword)).Wait(); err != nil {
		closeClient()
		return mailboxConn{}, errors.New("IMAP login failed")
	}
	selected, err := client.Select(cfg.String(fieldMailbox), &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil || selected.UIDValidity == 0 {
		closeClient()
		return mailboxConn{}, errors.New("IMAP could not examine a mailbox with a valid UID generation")
	}
	return mailboxConn{SelectData: selected, client: client, close: closeClient}, nil
}

func syncMailbox(ctx context.Context, sink *events.DBSink, cfg settings.Resolved) error {
	mailbox, err := openMailbox(ctx, cfg)
	if err != nil {
		return err
	}
	defer mailbox.close()
	return mailbox.sync(ctx, sink, cfg)
}

func (mailbox mailboxConn) sync(ctx context.Context, sink *events.DBSink, cfg settings.Resolved) error {
	found, err := mailbox.client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return errors.New("IMAP UID search failed")
	}
	uids, ok := found.All.(imap.UIDSet)
	if !ok || uids.Dynamic() {
		return errors.New("IMAP returned an invalid UID list")
	}
	owner := ownerEmail(cfg.String(fieldOwner))
	name := cfg.String(fieldMailbox)
	if strings.EqualFold(name, "INBOX") {
		name = "INBOX"
	}
	prefix := namespace(owner, cfg.String(fieldIMAPServer), cfg.String(fieldUsername), name,
		strconv.FormatUint(uint64(mailbox.UIDValidity), 10))
	idFor := func(uid imap.UID) string { return prefix + strconv.FormatUint(uint64(uid), 10) }

	existing, err := sink.ExternalIDs(ctx, imapName)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(existing))
	for _, id := range existing {
		suffix, matches := strings.CutPrefix(id, prefix)
		uid, err := strconv.ParseUint(suffix, 10, 32)
		seen[id] = matches && err == nil && uids.Contains(imap.UID(uid))
	}

	missing := make([]imap.UID, 0, maxFetchPerPoll)
	for _, r := range slices.Backward(uids) {
		for uid := r.Stop; len(missing) < maxFetchPerPoll; uid-- {
			if !seen[idFor(uid)] {
				missing = append(missing, uid)
			}
			if uid == r.Start {
				break
			}
		}
		if len(missing) == maxFetchPerPoll {
			break
		}
	}

	for batch := range slices.Chunk(missing, fetchBatchSize) {
		section := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Size: maxFetchBytes}}
		messages, err := mailbox.client.Fetch(imap.UIDSetNum(batch...), &imap.FetchOptions{
			UID: true, InternalDate: true, Envelope: true, RFC822Size: true,
			BodySection: []*imap.FetchItemBodySection{section},
		}).Collect()
		if err != nil {
			return errors.New("IMAP message fetch failed")
		}
		for _, message := range messages {
			if !slices.Contains(batch, message.UID) || message.InternalDate.IsZero() ||
				(message.RFC822Size > 0 && len(message.FindBodySection(section)) == 0) {
				return errors.New("IMAP returned incomplete message data")
			}
			event := mailEvent(message, section, idFor(message.UID), events.ScopeOwner(owner))
			if _, err := sink.Upsert(ctx, event); err != nil {
				return err
			}
		}
	}
	return removeMissing(ctx, sink, imapName, existing, seen)
}
