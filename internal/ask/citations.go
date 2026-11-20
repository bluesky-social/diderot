package ask

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/bluesky-social/diderot/internal/urlx"
)

// CitationRow is one persisted answer-time citation: the model's
// final answer contained URL, and URL belongs to an event the turn
// retrieved: the subset of retrieved events the model actually cited,
// NOT the citationAccumulator's everything-retrieved list.
type CitationRow struct {
	EventID int64
	URL     string
}

// resolveCitations joins the answer's URLs against the turn's
// retrieved hits. Exact URL match: the model copies URLs out of
// tool results verbatim, and the historical mining path uses the
// same equality, so both label sources agree on what "cited" means.
// URLs in the answer that match no retrieved hit (the model citing
// from prior-turn context, or mangling a URL) are dropped here —
// mining recovers prior-turn citations via its own session-wide
// pass if that ever matters.
//
// Both the stored URL and its padded form are indexed: the model
// never sees the raw agent_session URL — the tool layer widens
// the start/end range via padAgentSessionURL before emitting —
// so the byte-for-byte citation in the answer is the PADDED form.
// Indexing only h.URL would silently drop every agent_session
// citation.
//
// A URL is not guaranteed to identify one event: padAgentSessionURL's
// start clamp is not injective (two distinct chunks near a session's
// start can pad to the same URL), and some sources deliberately give
// several events one URL (a Zendesk ticket and its comments share the
// ticket link). byURL therefore keeps every distinct event a URL
// matches, in the order hits were accumulated, instead of letting the
// last one silently overwrite the rest: an ambiguous URL is recorded as
// a citation for each event it could name.
func resolveCitations(answer string, acc *citationAccumulator) []CitationRow {
	hits := acc.snapshot()
	if len(hits) == 0 {
		return nil
	}
	urls := urlx.ExtractURLs(answer)
	if len(urls) == 0 {
		return nil
	}
	byURL := make(map[string][]int64, len(hits)*2)
	addEvent := func(url string, eventID int64) {
		for _, id := range byURL[url] {
			if id == eventID {
				return
			}
		}
		byURL[url] = append(byURL[url], eventID)
	}
	for _, h := range hits {
		if h.URL != "" && h.EventID != 0 {
			addEvent(h.URL, h.EventID)
			addEvent(padAgentSessionURL(h.URL), h.EventID)
		}
	}
	seen := make(map[int64]bool, len(urls))
	var rows []CitationRow
	for _, u := range urls {
		for _, id := range byURL[u] {
			if seen[id] {
				continue
			}
			seen[id] = true
			rows = append(rows, CitationRow{EventID: id, URL: u})
		}
	}
	return rows
}

// InsertCitations writes answer-time citation rows. Idempotent per
// (session, turn, event): a retried turn or a URL cited twice lands
// once.
func (m *Manager) InsertCitations(ctx context.Context, sessionID uuid.UUID, turn int, rows []CitationRow) error {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		if _, err := m.pool.Exec(ctx,
			`INSERT INTO ask_citations (session_id, turn, event_id, url)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (session_id, turn, event_id) DO NOTHING`,
			sessionID, turn, row.EventID, row.URL,
		); err != nil {
			return fmt.Errorf("insert citation event=%d: %w", row.EventID, err)
		}
	}
	return nil
}
