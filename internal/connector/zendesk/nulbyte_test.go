package zendesk

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNulByteTicketDoesNotStallIncrementalCursor pins that a ticket
// Postgres cannot store as given does not stall the Zendesk incremental
// cursor.
//
// Trigger: a ticket's Subject contains a literal NUL byte (U+0000).
// Postgres text columns reject that byte (SQLSTATE 22021).
//
// Contract: this package's own TestPoisonTicketDoesNotBlockLaterTicketsAndRetries
// establishes the intended shape — a ticket worker.importTicket can't
// process (there, a 403) must not block the incremental cursor from
// advancing past the page it's in, nor block tickets created after it.
// A write error on a NUL byte would not get this treatment: the error
// from worker.storeTicket's Sink.Upsert call is a plain pgx error, not an
// *APIError and not errProtocol, so deferTicketError returns false,
// syncTickets's group.Wait() fails, and the
// `INSERT INTO zendesk_sync ... cursor` that saves page.AfterCursor never
// runs. Every following sync tick would re-fetch the exact same page and
// fail on the same ticket again, forever, because the ticket's content
// never changes — stranding every ticket on every later page.
//
// The guarantee is at the sink (internal/events), not in Zendesk's
// deferTicketError classification: DBSink.Upsert sanitizes NUL bytes and
// invalid UTF-8 out of every text field before writing, so the poisoned
// ticket is stored successfully instead of erroring at all, and
// syncTickets's cursor-save logic runs normally.
func TestNulByteTicketDoesNotStallIncrementalCursor(t *testing.T) {
	ctx := context.Background()

	// Page 1 (cursor=""): ticket 12 (NUL in Subject) and ticket 13
	// (valid). Page 2 (cursor="page2"): ticket 14, valid, created after
	// both — it stands in for "everything behind the poisoned ticket in
	// the export stream."
	poisoned := fixtureTicket()
	poisoned.ID = 12
	poisoned.Subject = "bad\u0000subject"
	valid13 := fixtureTicket()
	valid13.ID = 13
	valid14 := fixtureTicket()
	valid14.ID = 14

	client := testLookupClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/incremental/tickets/cursor.json":
			if r.URL.Query().Get("cursor") == "page2" {
				respond(t, w, map[string]any{"tickets": []Ticket{valid14}, "after_cursor": "done", "end_of_stream": true})
				return
			}
			respond(t, w, map[string]any{"tickets": []Ticket{poisoned, valid13}, "after_cursor": "page2", "end_of_stream": false})
		case r.URL.Path == "/api/v2/tickets/show_many.json":
			var tickets []Ticket
			for _, raw := range strings.Split(r.URL.Query().Get("ids"), ",") {
				switch raw {
				case "12":
					tickets = append(tickets, poisoned)
				case "13":
					tickets = append(tickets, valid13)
				case "14":
					tickets = append(tickets, valid14)
				}
			}
			respond(t, w, map[string]any{"tickets": tickets})
		case strings.HasSuffix(r.URL.Path, "/comments.json"):
			respond(t, w, map[string]any{"comments": []Comment{}, "meta": map[string]any{"has_more": false}})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	worker := testWorker(t, client)

	// Tick 1: fetches page 1. Both tickets 12 and 13 are written — the
	// NUL byte in ticket 12's Subject is sanitized by the sink rather
	// than rejected, so there is nothing for deferTicketError to catch.
	_, err := worker.SyncTickets(ctx)
	require.NoError(t, err)

	// Ticket 12 is stored, Subject sanitized (NUL stripped).
	var subject string
	require.NoError(t, worker.Pool.QueryRow(ctx,
		`SELECT title FROM events WHERE thread_id = 'ticket:12' AND external_id = 'ticket:12' AND NOT deleted`).Scan(&subject))
	require.Equal(t, "badsubject", subject)

	// Tick 2: the cursor advanced past page 1, so this tick reaches page
	// 2 and ticket 14 is indexed. Before the fix this never ran: tick 1
	// already failed on ticket 12's NUL byte and withheld the cursor, so
	// a second call to SyncTickets would re-fetch page 1 and fail again.
	_, err = worker.SyncTickets(ctx)
	require.NoError(t, err)

	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE thread_id = 'ticket:14' AND NOT deleted`).Scan(&count))
	require.Equal(t, 1, count, "ticket 14, behind the poisoned ticket 12 in the export stream, must be indexed")
}

// TestNulByteAttachmentFilenameDoesNotStallIncrementalCursor pins that
// ReconcileDocuments sanitizes DocumentRef.Filename before writing
// event_documents.filename. worker.upsertWithAttachments
// calls ReconcileDocuments right after the ticket/comment
// Sink.Upsert and returns its error unchanged; that error is a plain pgx
// error, not an *APIError and not errProtocol, so deferTicketError
// returns false exactly as it would for an unsanitized Subject — an
// unsanitized attachment filename with a NUL byte produces the same
// cursor stall, just one field later in the same write.
func TestNulByteAttachmentFilenameDoesNotStallIncrementalCursor(t *testing.T) {
	ctx := context.Background()

	// Page 1 (cursor=""): ticket 12, whose only comment carries an
	// attachment with a NUL byte in its filename, and ticket 13
	// (valid, no comments). Page 2 (cursor="page2"): ticket 14, valid,
	// created after both.
	poisoned := fixtureTicket()
	poisoned.ID = 12
	poisonedComment := Comment{
		ID: 21, AuthorID: 10, Body: "see attached", Public: true, CreatedAt: poisoned.CreatedAt,
		Attachments: []Attachment{{ID: 1, Filename: "report\x00.pdf", Mimetype: "application/pdf", ContentURL: "https://synthetic.zendesk.com/attachments/1", Size: 10}},
	}
	valid13 := fixtureTicket()
	valid13.ID = 13
	valid14 := fixtureTicket()
	valid14.ID = 14

	client := testLookupClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/incremental/tickets/cursor.json":
			if r.URL.Query().Get("cursor") == "page2" {
				respond(t, w, map[string]any{"tickets": []Ticket{valid14}, "after_cursor": "done", "end_of_stream": true})
				return
			}
			respond(t, w, map[string]any{"tickets": []Ticket{poisoned, valid13}, "after_cursor": "page2", "end_of_stream": false})
		case r.URL.Path == "/api/v2/tickets/show_many.json":
			var tickets []Ticket
			for _, raw := range strings.Split(r.URL.Query().Get("ids"), ",") {
				switch raw {
				case "12":
					tickets = append(tickets, poisoned)
				case "13":
					tickets = append(tickets, valid13)
				case "14":
					tickets = append(tickets, valid14)
				}
			}
			respond(t, w, map[string]any{"tickets": tickets})
		case r.URL.Path == "/api/v2/tickets/12/comments.json":
			respond(t, w, map[string]any{"comments": []Comment{poisonedComment}, "meta": map[string]any{"has_more": false}})
		case strings.HasSuffix(r.URL.Path, "/comments.json"):
			respond(t, w, map[string]any{"comments": []Comment{}, "meta": map[string]any{"has_more": false}})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	worker := testWorker(t, client)

	// Tick 1: fetches page 1. Both tickets 12 and 13 are written —
	// ReconcileDocuments now sanitizes the attachment filename instead
	// of handing Postgres a raw NUL byte, so there is nothing for
	// deferTicketError to catch.
	_, err := worker.SyncTickets(ctx)
	require.NoError(t, err)

	var filename string
	require.NoError(t, worker.Pool.QueryRow(ctx, `
		SELECT d.filename FROM event_documents d JOIN events e ON e.id = d.event_id
		 WHERE e.external_id = 'ticket:12:comment:21'
	`).Scan(&filename))
	require.Equal(t, "report.pdf", filename, "attachment filename must be sanitized (NUL byte stripped)")

	// Tick 2: the cursor advanced past page 1, so this tick reaches
	// page 2 and ticket 14 is indexed. Before the fix this never ran:
	// tick 1 already failed on the attachment filename's NUL byte and
	// withheld the cursor.
	_, err = worker.SyncTickets(ctx)
	require.NoError(t, err)

	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE thread_id = 'ticket:14' AND NOT deleted`).Scan(&count))
	require.Equal(t, 1, count, "ticket 14, behind the ticket with the poisoned attachment, must be indexed")
}
