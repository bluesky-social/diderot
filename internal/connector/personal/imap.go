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
	maxFetchBytes   = 512 << 10
	maxFetchPerPoll = 300
	fetchBatchSize  = 20
)

// IMAP reads all selectable mailboxes in an account over TLS.
type IMAP struct{}

func (IMAP) Name() string { return imapName }

func (IMAP) Info() source.Info {
	return source.Info{
		EventSource: imapName, Title: "IMAP mail", ResultNoun: "Email",
		Summary:         "Email messages from a private IMAP account.",
		ToolDescription: "Email messages from an owner's IMAP mailboxes.",
		ActorRole:       "sender", IdentityNamespace: func(string) string { return "email" },
	}
}

func (IMAP) Fields() []settings.Field {
	return append(accountFields(),
		settings.Field{Key: fieldIMAPServer, Label: "IMAP TLS server", Kind: settings.KindText, Required: true,
			Help: "The server and port, such as imap.example.com:993. All selectable mailboxes are indexed."})
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
	account, err := openIMAP(ctx, cfg)
	check := settings.Check{Name: "Discover mailboxes", Detail: "Could not authenticate and list mailboxes over TLS.",
		Remedy: "Check the server, credentials, and TLS certificate."}
	if err == nil {
		defer account.close()
		mailboxes, err := account.mailboxes()
		if err == nil {
			check = settings.Check{Name: check.Name, Passed: true,
				Detail: fmt.Sprintf("Found %d selectable mailboxes.", len(mailboxes))}
		}
	}
	return settings.TestReport{Checks: []settings.Check{check}}
}

func (IMAP) Run(ctx context.Context, deps source.Deps, cfg settings.Resolved) error {
	return run(ctx, deps, imapName, cfg, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		account, err := openIMAP(ctx, cfg)
		if err != nil {
			return err
		}
		defer account.close()
		return account.sync(ctx, deps.Sink, cfg)
	})
}

func (IMAP) Expand(ctx context.Context, deps source.Deps, id int64, _ []string) (source.Expanded, error) {
	return expand(ctx, deps, id, imapName)
}

type imapAccount struct {
	client *imapclient.Client
	close  func()
}

func openIMAP(ctx context.Context, cfg settings.Resolved) (imapAccount, error) {
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: &tls.Config{MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.String(fieldIMAPServer))
	if err != nil {
		return imapAccount{}, fmt.Errorf("dial IMAP TLS: %w", err)
	}
	// Closing the socket cancels a pending command on leadership or deadline
	// loss, independent of the IMAP client's per-command timeouts.
	return loginIMAP(ctx, conn, cfg)
}

func loginIMAP(ctx context.Context, conn net.Conn, cfg settings.Resolved) (imapAccount, error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	client := imapclient.New(conn, &imapclient.Options{WordDecoder: &mime.WordDecoder{CharsetReader: charset.Reader}})
	closeClient := func() {
		stop()
		_ = client.Close()
	}
	if err := client.Login(cfg.String(fieldUsername), cfg.Secret(fieldPassword)).Wait(); err != nil {
		closeClient()
		return imapAccount{}, errors.New("IMAP login failed")
	}
	return imapAccount{client: client, close: closeClient}, nil
}

func (a imapAccount) mailboxes() ([]string, error) {
	mailboxes, err := a.client.List("", "*", nil).Collect()
	if err != nil {
		return nil, errors.New("IMAP mailbox listing failed")
	}
	var names []string
	for _, box := range mailboxes {
		if !selectableMailbox(box) {
			continue
		}
		name := box.Mailbox
		if strings.EqualFold(name, "INBOX") {
			name = "INBOX"
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func selectableMailbox(box *imap.ListData) bool {
	return box.Mailbox != "" && !slices.ContainsFunc(box.Attrs, func(attr imap.MailboxAttr) bool {
		return strings.EqualFold(string(attr), string(imap.MailboxAttrNoSelect)) ||
			strings.EqualFold(string(attr), string(imap.MailboxAttrNonExistent))
	})
}

func (a imapAccount) sync(ctx context.Context, sink *events.DBSink, cfg settings.Resolved) error {
	names, err := a.mailboxes()
	if err != nil {
		return err
	}
	existing, err := sink.ExternalIDs(ctx, imapName)
	if err != nil {
		return err
	}
	byPrefix := make(map[string][]string)
	for _, id := range existing {
		if prefix, _, ok := strings.Cut(id, ":"); ok {
			byPrefix[prefix+":"] = append(byPrefix[prefix+":"], id)
		}
	}
	seen := make(map[string]bool, len(existing))
	var syncErr error
	for _, name := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := a.syncMailbox(ctx, sink, cfg, name, byPrefix, seen); err != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("sync IMAP mailbox %q: %w", name, err))
		}
	}
	if syncErr != nil {
		return syncErr
	}
	return removeMissing(ctx, sink, imapName, existing, seen)
}

func (a imapAccount) syncMailbox(ctx context.Context, sink *events.DBSink, cfg settings.Resolved, name string, existing map[string][]string, seen map[string]bool) error {
	selected, err := a.client.Select(name, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil || selected.UIDValidity == 0 {
		return errors.New("could not examine mailbox with a valid UID generation")
	}
	found, err := a.client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return errors.New("IMAP UID search failed")
	}
	uids, ok := found.All.(imap.UIDSet)
	if !ok || uids.Dynamic() {
		return errors.New("IMAP returned an invalid UID list")
	}
	owner := ownerEmail(cfg.String(fieldOwner))
	prefix := namespace(owner, cfg.String(fieldIMAPServer), cfg.String(fieldUsername), name,
		strconv.FormatUint(uint64(selected.UIDValidity), 10))
	idFor := func(uid imap.UID) string { return prefix + strconv.FormatUint(uint64(uid), 10) }

	indexedUIDs := make(map[imap.UID]bool, len(existing[prefix]))
	for _, id := range existing[prefix] {
		uid, err := strconv.ParseUint(strings.TrimPrefix(id, prefix), 10, 32)
		if err == nil && uids.Contains(imap.UID(uid)) {
			seen[id] = true
			indexedUIDs[imap.UID(uid)] = true
		}
	}

	missing := make([]imap.UID, 0, maxFetchPerPoll)
	for _, r := range slices.Backward(uids) {
		for uid := r.Stop; len(missing) < maxFetchPerPoll; uid-- {
			if !indexedUIDs[uid] {
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
		messages, err := a.client.Fetch(imap.UIDSetNum(batch...), &imap.FetchOptions{
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
			if name != "INBOX" {
				event.Body = "Mailbox: " + cleanText(name) + "\n" + event.Body
			}
			if _, err := sink.Upsert(ctx, event); err != nil {
				return err
			}
		}
	}
	return nil
}
