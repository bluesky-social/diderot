package slack

import (
	"context"
	"fmt"
	"strings"
)

// Document appendix for the expand path.
//
// Without this, `expand` on a document-bearing message returns the
// thread text and nothing else — so the model can see "here is the
// contract" and still not read the contract. That is the exact
// complaint that motivated document ingest: someone asked diderot to
// review an attached PDF and it answered from surrounding chat.
//
// Retrieval alone isn't sufficient because it returns the single
// best-matching chunk. A question like "review the attached PDF and
// tell me what you think" has no phrase that matches page 4, so the
// model needs a way to read the whole document once it knows one is
// there.
//
// Scoping note: this runs INSIDE the expander, which the caller has
// already gated on the parent event's scope. It selects documents by
// event_id from that already-authorized set, so it inherits the gate
// rather than re-deriving it — the same reason document chunks inherit
// scope from their parent event.

// docAppendixBudget bounds how much extracted document text one expand
// call can append, in runes.
//
// The expander's own render budget is ~64 KiB for thread text; a long
// PDF can exceed that on its own, and silently spending the whole
// window on one attachment would starve the thread context the model
// also needs. 24k runes is roughly 12-15 pages of prose — enough to
// answer questions about a typical shared document, and the truncation
// marker tells the model when there's more.
const docAppendixBudget = 24000

// perDocBudget bounds any single document when several are attached, so
// one 40-page contract can't crowd out its two siblings entirely.
const perDocBudget = 12000

// documentAppendix returns rendered text for every extracted document
// attached to the messages in the expanded set, or "" when there are
// none.
//
// eventIDs is the set of events actually rendered (thread members or
// neighbor-window members), not just the matched event: a thread where
// someone posts a contract and someone else asks about it should
// surface the contract regardless of which message matched.
//
// Documents still awaiting extraction are reported as pending rather
// than omitted. That distinction is load-bearing for the most natural
// usage there is — post a PDF and @-mention diderot in the same breath.
// Extraction takes a poll interval plus a model call (tens of seconds),
// so a fast follow-up arrives before the text exists. Silently omitting
// it would make the model answer as though the document weren't there,
// which is precisely the failure this whole feature exists to fix; it
// would just be a narrower, harder-to-notice version of it. Telling the
// model "this document exists and isn't readable yet" lets it say so
// instead of confidently answering without it.
func (e *Expander) documentAppendix(ctx context.Context, eventIDs []int64) string {
	if e.pool == nil || len(eventIDs) == 0 {
		return ""
	}
	rows, err := e.pool.Query(ctx, `
		SELECT ed.filename, COALESCE(ed.page_count, 0),
		       CASE WHEN ed.incomplete_reason IS NOT NULL THEN 'partial'
		            WHEN ed.rejected_reason IS NOT NULL THEN 'rejected'
		            ELSE 'complete' END,
		       COALESCE(ed.incomplete_reason, ''),
		       COALESCE(ed.pages_extracted, 0),
		       COALESCE(e.actor, ''),
		       ed.extracted_at IS NOT NULL AS settled,
		       COALESCE(ed.rejected_reason, ''),
		       COALESCE(string_agg(c.content, E'\n\n' ORDER BY c.chunk_idx), '')
		  FROM event_documents ed
		  JOIN events e ON e.id = ed.event_id
		  LEFT JOIN event_chunks c ON c.event_document_id = ed.id
		 WHERE ed.event_id = ANY($1::bigint[])
		   AND e.deleted = false
		 GROUP BY ed.id, ed.filename, ed.page_count,
		          ed.incomplete_reason, ed.pages_extracted, e.actor,
		          ed.extracted_at, ed.rejected_reason, ed.occurred_at
		 ORDER BY ed.occurred_at, ed.id
	`, eventIDs)
	if err != nil {
		// Best-effort: a failed appendix must not fail the expand. The
		// thread text is still useful on its own, and the caller has no
		// way to act on this error anyway — the document text either
		// makes it into the model's context or it doesn't.
		return ""
	}
	defer rows.Close()

	type doc struct {
		filename       string
		pages          int
		completeness   string
		incompleteWhy  string
		pagesExtracted int
		actor          string
		settled        bool
		rejected       string
		text           string
	}
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.filename, &d.pages, &d.completeness,
			&d.incompleteWhy, &d.pagesExtracted, &d.actor,
			&d.settled, &d.rejected, &d.text); err != nil {
			return ""
		}
		docs = append(docs, d)
	}
	if rows.Err() != nil || len(docs) == 0 {
		return ""
	}
	// Only render the block if at least one document is worth mentioning:
	// has text, or is pending (which the model should know about). A
	// document that settled with a rejection has nothing to say and
	// nothing to wait for, so it stays silent rather than advertising a
	// failure the reader can't act on.
	var worth bool
	for _, d := range docs {
		if strings.TrimSpace(d.text) != "" || !d.settled {
			worth = true
			break
		}
	}
	if !worth {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n--- Attached documents (text extracted by diderot) ---\n")
	sb.WriteString("Content below is FROM THE DOCUMENTS, not written by the people in the thread.\n")
	remaining := docAppendixBudget
	for _, d := range docs {
		// Pending: extraction hasn't finished. Say so explicitly — see
		// the doc comment on why silence here would recreate the exact
		// bug this feature fixes.
		if !d.settled {
			fmt.Fprintf(&sb,
				"\n[document: %s — text not extracted yet; it should be readable within a minute. "+
					"Say the document is still being processed rather than answering as if it were empty.]\n",
				d.filename)
			continue
		}
		if strings.TrimSpace(d.text) == "" {
			// Settled with no text (rejected as degenerate/unreadable).
			// Nothing to add and nothing to wait for.
			continue
		}
		if remaining <= 0 {
			fmt.Fprintf(&sb, "\n[%s: omitted, extracted-text budget exhausted]\n", d.filename)
			continue
		}
		budget := perDocBudget
		if budget > remaining {
			budget = remaining
		}
		text, truncated := truncateRunesMarked(d.text, budget)
		remaining -= len([]rune(text))

		fmt.Fprintf(&sb, "\n[document: %s", d.filename)
		if d.pages > 0 {
			fmt.Fprintf(&sb, ", %d %s", d.pages, pluralize("page", d.pages))
		}
		if d.actor != "" {
			fmt.Fprintf(&sb, ", shared by @%s", d.actor)
		}
		if d.completeness == "partial" {
			sb.WriteString(", PARTIAL extraction")
			if d.incompleteWhy != "" {
				fmt.Fprintf(&sb, " (%s)", d.incompleteWhy)
			}
			if d.pages > 0 {
				fmt.Fprintf(&sb, ", %d of %d pages extracted", d.pagesExtracted, d.pages)
			}
		}
		sb.WriteString("]\n")
		sb.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			sb.WriteByte('\n')
		}
		if truncated {
			fmt.Fprintf(&sb, "[...%s truncated; ask about a specific section for more]\n", d.filename)
		}
	}
	return sb.String()
}

// truncateRunesMarked cuts s to at most n runes, reporting whether it
// cut. Rune-aware so a multi-byte codepoint isn't split (which would
// produce invalid UTF-8 and get rejected by the tool layer).
func truncateRunesMarked(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n]), true
}
