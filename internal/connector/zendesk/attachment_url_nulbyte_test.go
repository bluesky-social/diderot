package zendesk

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// An attachment's content URL goes into zendesk_files inside the ticket
// comment's own write. Postgres rejects a NUL byte in it, which would fail
// the comment on every sync and hold the incremental cursor on its page.
func TestNulByteAttachmentContentURLDoesNotStallIncrementalCursor(t *testing.T) {
	ctx := context.Background()

	poisoned := fixtureTicket()
	poisoned.ID = 12
	poisonedComment := Comment{
		ID: 21, AuthorID: 10, Body: "see attached", Public: true, CreatedAt: poisoned.CreatedAt,
		Attachments: []Attachment{{ID: 1, Filename: "report.pdf", Mimetype: "application/pdf", ContentURL: "https://synthetic.zendesk.com/attachments/1?name=report\x00.pdf", Size: 10}},
	}
	valid14 := fixtureTicket()
	valid14.ID = 14

	client := testLookupClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/incremental/tickets/cursor.json":
			if r.URL.Query().Get("cursor") == "page2" {
				respond(t, w, map[string]any{"tickets": []Ticket{valid14}, "after_cursor": "done", "end_of_stream": true})
				return
			}
			respond(t, w, map[string]any{"tickets": []Ticket{poisoned}, "after_cursor": "page2", "end_of_stream": false})
		case r.URL.Path == "/api/v2/tickets/show_many.json":
			var tickets []Ticket
			for _, raw := range strings.Split(r.URL.Query().Get("ids"), ",") {
				switch raw {
				case "12":
					tickets = append(tickets, poisoned)
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

	_, err := worker.SyncTickets(ctx)
	require.NoError(t, err)

	var contentURL string
	require.NoError(t, worker.Pool.QueryRow(ctx, `
		SELECT f.content_url FROM zendesk_files f JOIN events e ON e.id = f.event_id
		 WHERE e.external_id = 'ticket:12:comment:21'
	`).Scan(&contentURL))
	require.Equal(t, "https://synthetic.zendesk.com/attachments/1?name=report.pdf", contentURL)

	// The cursor moved past the first page, so the second sync reaches ticket 14.
	_, err = worker.SyncTickets(ctx)
	require.NoError(t, err)
	var count int
	require.NoError(t, worker.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE thread_id = 'ticket:14' AND NOT deleted`).Scan(&count))
	require.Equal(t, 1, count)
}
