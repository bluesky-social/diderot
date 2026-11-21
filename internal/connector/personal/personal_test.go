package personal

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestMailTextPrefersPlainTextAndSkipsAttachments(t *testing.T) {
	raw := strings.ReplaceAll(`MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="nested"

--nested
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Delivery arrives at caf=C3=A9 on Friday.
--nested
Content-Type: text/html

<p>HTML ALTERNATIVE</p>
--nested--
--outer
Content-Type: text/plain
Content-Disposition: attachment; filename="secret.txt"

ATTACHMENT CONTENT
--outer--
`, "\n", "\r\n")
	require.Equal(t, "Delivery arrives at café on Friday.", messageText([]byte(raw)))
	require.Equal(t, "Hello & goodbye\nNext", messageText([]byte(
		"Content-Type: text/html\r\n\r\n<head><title>ignore</title></head><script>ignore</script><p>Hello &amp; goodbye<br>Next</p>")))
}

func TestIMAPSyncReadsAllMailboxesWithoutMarkingSeenAndReconciles(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "personal_imap"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())

	memory := imapmemserver.New()
	user := imapmemserver.NewUser("mailuser", "app-password")
	for _, name := range []string{"INBOX", "Sent", "Archive/2026"} {
		require.NoError(t, user.Create(name, nil))
	}
	memory.AddUser(user)

	certificateServer := httptest.NewTLSServer(nil)
	t.Cleanup(certificateServer.Close)
	roots := x509.NewCertPool()
	roots.AddCert(certificateServer.Certificate())
	var failArchive atomic.Bool
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return imapFixture{Session: memory.NewSession(), failArchive: &failArchive}, nil, nil
		},
	})
	listener, err := tls.Listen("tcp", "127.0.0.1:0", certificateServer.TLS)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close(); _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	dial := func() net.Conn {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp",
			listener.Addr().String(), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
		require.NoError(t, err)
		return conn
	}
	upstream := imapclient.New(dial(), nil)
	t.Cleanup(func() { _ = upstream.Close() })
	require.NoError(t, upstream.Login("mailuser", "app-password").Wait())
	_, err = upstream.Select("INBOX", nil).Wait()
	require.NoError(t, err)
	saved := appendMessage(t, upstream, "INBOX", "Lunch plans", "Dinner on the roof.")
	require.Equal(t, saved, appendMessage(t, upstream, "Sent", "Sent reply", "Confirmed lunch."))
	appendMessage(t, upstream, "Archive/2026", "Old draft", "Archived project text.")
	cfg := settings.NewResolved(imapName, settings.Values{
		fieldOwner: " ALICE@example.com ", fieldUsername: "mailuser",
		fieldIMAPServer: listener.Addr().String(),
	}, map[string]string{fieldPassword: "app-password"})

	sync := func() error {
		t.Helper()
		account, err := loginIMAP(ctx, dial(), cfg)
		require.NoError(t, err)
		defer account.close()
		return account.sync(ctx, sink, cfg)
	}
	require.NoError(t, sync())
	first := readRows(t, pool, imapName)
	require.Len(t, first, 3)
	before := make(map[string]storedRow, len(first))
	for _, row := range first {
		require.Equal(t, "owner:alice@example.com", row.scope)
		before[row.title] = row
	}
	require.Contains(t, before["Lunch plans"].body, "Dinner on the roof.")
	require.Contains(t, before["Sent reply"].body, "Mailbox: Sent")
	require.Contains(t, before["Old draft"].body, "Mailbox: Archive/2026")
	flags, err := upstream.Fetch(imap.UIDSetNum(saved), &imap.FetchOptions{Flags: true}).Collect()
	require.NoError(t, err)
	require.Len(t, flags, 1)
	require.NotContains(t, flags[0].Flags, imap.FlagSeen)

	require.NoError(t, sync())
	require.Equal(t, first, readRows(t, pool, imapName), "repeat polls should leave the event and its embeddings alone")
	appendMessage(t, upstream, "INBOX", "Second message", "New message text.")
	require.NoError(t, upstream.Store(imap.UIDSetNum(saved), &imap.StoreFlags{
		Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close())
	require.NoError(t, upstream.Expunge().Close())
	failArchive.Store(true)
	require.ErrorContains(t, sync(), "Archive/2026")
	require.Len(t, readRows(t, pool, imapName), 4, "a partial poll can ingest updates but must not remove rows")

	failArchive.Store(false)
	require.NoError(t, upstream.Delete("Archive/2026").Wait())
	require.NoError(t, sync())
	rows := readRows(t, pool, imapName)
	require.Len(t, rows, 2)
	require.Equal(t, before["Sent reply"], rows[0])
	require.Equal(t, "Second message", rows[1].title)
	require.Contains(t, rows[1].body, "New message text.")
}

type imapFixture struct {
	imapserver.Session
	failArchive *atomic.Bool
}

func (s imapFixture) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	if err := w.WriteList(&imap.ListData{Mailbox: "Archive", Delim: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}}); err != nil {
		return err
	}
	return s.Session.List(w, ref, patterns, options)
}

func (s imapFixture) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	if name == "Archive/2026" && s.failArchive.Load() {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "temporary failure"}
	}
	return s.Session.Select(name, options)
}

func appendMessage(t *testing.T, c *imapclient.Client, mailbox, subject, body string) imap.UID {
	t.Helper()
	raw := "Date: Fri, 20 Nov 2026 12:00:00 +0000\r\nFrom: Taylor <taylor@example.com>\r\n" +
		"To: Alice <alice@example.com>\r\nSubject: " + subject + "\r\n\r\n" + body + "\r\n"
	append := c.Append(mailbox, int64(len(raw)), nil)
	_, err := io.WriteString(append, raw)
	require.NoError(t, err)
	require.NoError(t, append.Close())
	data, err := append.Wait()
	require.NoError(t, err)
	require.NotZero(t, data.UID)
	return data.UID
}

func TestCalDAVSyncReadsEditsExceptionsAndUndatedJournalNotes(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "personal_caldav"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	type response struct {
		calendar, notes string
		failNotes       bool
	}
	var fixture atomic.Pointer[response]
	calendar := "BEGIN:VEVENT\r\nUID:meeting\r\nDTSTAMP:20261001T100000Z\r\n" +
		"DTSTART;TZID=Europe/Rome:20261125T140000\r\nDTEND;TZID=Europe/Rome:20261125T150000\r\n" +
		"SUMMARY:Planning\r\nRRULE:FREQ=WEEKLY\r\nLOCATION:Main room\r\nDESCRIPTION:First line\\nSecond line\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:meeting\r\nDTSTAMP:20261001T100000Z\r\nRECURRENCE-ID;TZID=Europe/Rome:20261202T140000\r\n" +
		"DTSTART;TZID=Europe/Rome:20261202T150000\r\nSUMMARY:Planning exception\r\nEND:VEVENT\r\n"
	note := "BEGIN:VJOURNAL\r\nUID:note\r\nDTSTAMP:20261120T140000Z\r\nSUMMARY:Project notes\r\n" +
		"DESCRIPTION:Discuss the new launch.\r\nEND:VJOURNAL\r\n"
	fixture.Store(&response{calendar: calendar, notes: note})

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		body, _ := io.ReadAll(r.Body)
		if r.Method != "REPORT" || r.Header.Get("Depth") != "1" || user != "calendaruser" || pass != "app-password" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var entry string
		current := fixture.Load()
		switch r.URL.Path {
		case "/calendars/personal/":
			if strings.Contains(string(body), `name="VEVENT"`) {
				entry = current.calendar
			}
		case "/calendars/notes/":
			if current.failNotes {
				http.Error(w, "temporary failure", http.StatusServiceUnavailable)
				return
			}
			if strings.Contains(string(body), `name="VJOURNAL"`) {
				entry = current.notes
			}
		default:
			http.NotFound(w, r)
			return
		}
		writeDAVReport(w, r.URL.Path, entry)
	}))
	t.Cleanup(server.Close)
	cfg := settings.NewResolved(caldavName, settings.Values{
		fieldOwner: "alice@example.com", fieldUsername: "calendaruser",
		fieldCalendarURL: server.URL + "/calendars/personal/",
		fieldJournalURL:  server.URL + "/calendars/notes/",
	}, map[string]string{fieldPassword: "app-password"})
	dav := newDAVClient(cfg)
	dav.http.Transport = server.Client().Transport

	require.NoError(t, syncCollections(ctx, sink, dav, cfg))
	first := readRows(t, pool, caldavName)
	require.Len(t, first, 3)
	for _, row := range first {
		require.Equal(t, "owner:alice@example.com", row.scope)
	}
	byTitle := map[string]storedRow{}
	for _, row := range first {
		byTitle[row.title] = row
	}
	require.Contains(t, byTitle["Planning"].body, "Starts: 2026-11-25T14:00:00+01:00")
	require.Contains(t, byTitle["Planning"].body, "Recurrence: FREQ=WEEKLY")
	require.Contains(t, byTitle["Planning"].body, "First line\nSecond line")
	require.Contains(t, byTitle["Planning exception"].body, "Original occurrence: 2026-12-02T14:00:00+01:00")
	require.Contains(t, byTitle["Project notes"].body, "Discuss the new launch.")
	require.Equal(t, time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC), byTitle["Project notes"].occurred.UTC())

	require.NoError(t, syncCollections(ctx, sink, dav, cfg))
	require.Equal(t, first, readRows(t, pool, caldavName))
	fixture.Store(&response{failNotes: true})
	require.ErrorContains(t, syncCollections(ctx, sink, dav, cfg), "journal notes")
	require.Equal(t, first, readRows(t, pool, caldavName), "failed poll must not delete entries")

	fixture.Store(&response{calendar: strings.Replace(calendar, "SUMMARY:Planning\r\n", "SUMMARY:Planning updated\r\n", 1)})
	require.NoError(t, syncCollections(ctx, sink, dav, cfg))
	rows := readRows(t, pool, caldavName)
	require.Len(t, rows, 2)
	found := false
	for _, row := range rows {
		if row.title == "Planning updated" {
			require.Equal(t, byTitle["Planning"].id, row.id)
			found = true
		}
	}
	require.True(t, found)
}

func writeDAVReport(w http.ResponseWriter, path, entry string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprint(w, `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav">`)
	if entry != "" {
		fmt.Fprintf(w, `<D:response><D:href>%sitem.ics</D:href><D:propstat><D:prop><C:calendar-data>`, path)
		_ = xml.EscapeText(w, []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Diderot test//EN\r\n"+entry+"END:VCALENDAR\r\n"))
		fmt.Fprint(w, `</C:calendar-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	fmt.Fprint(w, `</D:multistatus>`)
}

type storedRow struct {
	id                 int64
	scope, title, body string
	occurred           time.Time
	updated            *time.Time
}

func readRows(t *testing.T, pool *pgxpool.Pool, name string) []storedRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT id, scope, title, body, occurred_at, updated_at FROM events
		WHERE source = $1 AND NOT deleted ORDER BY id`, name)
	require.NoError(t, err)
	defer rows.Close()
	var result []storedRow
	for rows.Next() {
		var r storedRow
		require.NoError(t, rows.Scan(&r.id, &r.scope, &r.title, &r.body, &r.occurred, &r.updated))
		result = append(result, r)
	}
	require.NoError(t, rows.Err())
	return result
}
