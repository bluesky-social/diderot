package slack

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/askchat"
)

// The thread context's escaping and caps are the engine's (askchat), which
// renders it.
var promptTagReplacer = askchat.PromptTagEscaper

const (
	threadContextMsgCap     = askchat.ThreadContextMax
	threadContextCaptionMax = askchat.ContextCaptionMax
)

// hydrateThreadBotNames resolves classic bot ids that arrived without inline
// username/bot_profile metadata. It runs immediately before transcript fetch,
// so historical actorless roots become readable on the very ask that needs
// them. LookupBot supplies persistent TTL caching, singleflight, and negative
// caching; this query only identifies the distinct unresolved ids in scope.
func hydrateThreadBotNames(
	ctx context.Context,
	pool *pgxpool.Pool,
	dir *Directory,
	channelID, threadTS string,
	since, before time.Time,
) error {
	if dir == nil {
		return nil
	}
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT e.actor_id
		  FROM events e
		  JOIN slack_message_details d ON d.event_id = e.id
		  LEFT JOIN slack_bots b ON b.bot_id = e.actor_id
		 WHERE e.source = 'slack'
		   AND d.channel_id = $1
		   AND (d.thread_ts = $2 OR e.external_id = $1 || ':' || $2)
		   AND e.deleted = false
		   AND e.occurred_at > $3
		   AND e.occurred_at < $4
		   AND COALESCE(e.actor, '') = ''
		   AND COALESCE(e.actor_id, '') <> ''
		   AND e.actor_canonical = 'slack-bot:' || e.actor_id
		   AND COALESCE(b.name, '') = ''
		 LIMIT $5
	`, channelID, threadTS, since, before, threadContextMsgCap)
	if err != nil {
		return fmt.Errorf("find unresolved thread bots: %w", err)
	}
	defer rows.Close()
	var botIDs []string
	for rows.Next() {
		var botID string
		if err := rows.Scan(&botID); err != nil {
			return fmt.Errorf("scan unresolved thread bot: %w", err)
		}
		botIDs = append(botIDs, botID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate unresolved thread bots: %w", err)
	}
	for _, botID := range botIDs {
		_, _ = dir.LookupBot(ctx, botID)
	}
	return nil
}

// fetchThreadContextBetween returns a rendered transcript of the
// thread's messages strictly after `since` and strictly before
// `before`, oldest-first. It injects thread context into an ask turn
// when diderot is @-mentioned in a thread it has not yet fully seen.
//
// `since` is the context_through high-water mark from ask_slack_sessions
// — every message at or before that timestamp has already been delivered
// into the session. Zero `since` means no lower bound (first-touch: all
// messages before `before` are returned). `before` is the occurred_at of
// the @-mention itself; the mention is the question being answered and
// must not appear in the injected context.
//
// Diderot's own replies are never present here because the consumer
// self-filters self-authored messages before ingest (slack.go ~line 639,
// `skipped_self`). No additional dedup against bot output is needed.
//
// The query ORs across the JOIN: thread_ts is empty on the root message when it arrives
// (Slack populates it only after the first reply), so the root's detail
// row has thread_ts = ” even though it belongs to the thread. We catch
// it via the external_id = channel||':'||ts branch.
//
// Returns ("", nil) when there are no qualifying messages.
func fetchThreadContextBetween(
	ctx context.Context,
	pool *pgxpool.Pool,
	channelID, threadTS string,
	since, before time.Time,
) (string, error) {
	// Query: most-recent-N+1 DESC (the +1 lets us distinguish "exactly N
	// messages in thread" from "more than N — cap hit and some omitted").
	// We reverse to oldest-first in Go before rendering. The OR-join
	// catches the thread root whose thread_ts was '' at ingest time.
	//
	// A zero `since` (first-touch: no lower bound) needs no special
	// casing: Go's zero time.Time is year 1, below any real
	// occurred_at, so the strict > matches every thread message.
	// Image captions live in event_chunks(kind='image_caption'), one
	// row per image on the event, ordered by chunk_idx. We pull them
	// as an aggregated text[] via a correlated array_agg subquery
	// rather than a LEFT JOIN so a multi-image message doesn't fan out
	// into duplicate body rows (the JOIN would repeat e.body once per
	// caption). NULL array (no captions) scans into a nil slice.
	const q = `
		SELECT COALESCE(NULLIF(e.actor, ''), NULLIF(b.name, ''),
		                NULLIF(e.actor_canonical, ''), NULLIF(e.actor_id, ''),
		                'unknown') AS actor,
		       e.occurred_at, e.body,
		       (SELECT array_agg(ec.content ORDER BY ec.chunk_idx)
		          FROM event_chunks ec
		         WHERE ec.event_id = e.id
		           AND ec.kind = 'image_caption') AS captions
		  FROM events e
		  JOIN slack_message_details d ON d.event_id = e.id
		  LEFT JOIN slack_bots b ON b.bot_id = e.actor_id
		 WHERE e.source = 'slack'
		   AND d.channel_id = $1
		   AND (d.thread_ts = $2 OR e.external_id = $1 || ':' || $2)
		   AND e.deleted = false
		   AND e.occurred_at > $3
		   AND e.occurred_at < $4
		 ORDER BY e.occurred_at DESC, e.id DESC
		 LIMIT $5
	`
	rows, err := pool.Query(ctx, q, channelID, threadTS, since, before, threadContextMsgCap+1)
	if err != nil {
		return "", fmt.Errorf("fetchThreadContextBetween: %w", err)
	}
	defer rows.Close()

	type row struct {
		actor      string
		occurredAt time.Time
		body       string
		captions   []string
	}
	var msgs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.actor, &r.occurredAt, &r.body, &r.captions); err != nil {
			return "", fmt.Errorf("fetchThreadContextBetween scan: %w", err)
		}
		msgs = append(msgs, r)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("fetchThreadContextBetween rows: %w", err)
	}
	if len(msgs) == 0 {
		return "", nil
	}

	// Reverse: query returned DESC (newest first), we want oldest first.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	// The cap+1 fetch lets us distinguish "thread has exactly N messages"
	// from "thread has more than N — some were omitted." Only emit the
	// omission marker when the extra probe row exists, then discard it.
	//
	// After the DESC query and reversal, the slice is oldest-first:
	// index 0 is the oldest fetched row (the extra probe) and the last
	// index is the message immediately before the mention. Drop the probe
	// row from the front with msgs[1:], NOT msgs[:cap], to keep the most
	// recent cap messages rather than the oldest cap.
	truncated := len(msgs) > threadContextMsgCap
	if truncated {
		msgs = msgs[1:]
	}

	rendered := make([]askchat.ContextMessage, 0, len(msgs))
	for _, m := range msgs {
		rendered = append(rendered, askchat.ContextMessage{At: m.occurredAt, Actor: m.actor, Body: m.body, Captions: m.captions})
	}
	return askchat.RenderThreadContext(rendered, truncated), nil
}

// appendMentionCaptions appends the mention message's own image
// captions to the question string. Each caption becomes a
// "\n\n[attached image: <caption>]" block so the model reads the
// screenshot the user pasted alongside the @-mention — the mention
// message is excluded from the thread-context block (strict <), so
// this is the only place its captions reach the prompt.
//
// Same prompt-tag escaping and shared rune budget as captionLines:
// a caption is model-influenced text, and a many-image mention can't
// be allowed to blow the envelope. Returns question unchanged when
// there are no captions.
func appendMentionCaptions(question string, captions []string) string {
	if len(captions) == 0 {
		return question
	}
	var b strings.Builder
	b.WriteString(question)
	remaining := threadContextCaptionMax
	for _, capt := range captions {
		if remaining <= 0 {
			b.WriteString("\n\n[attached image: …]")
			break
		}
		capt = strings.TrimRight(capt, "\n")
		if utf8.RuneCountInString(capt) > remaining {
			runes := []rune(capt)
			capt = string(runes[:remaining]) + "…"
		}
		remaining -= utf8.RuneCountInString(capt)
		capt = promptTagReplacer.Replace(capt)
		fmt.Fprintf(&b, "\n\n[attached image: %s]", capt)
	}
	return b.String()
}

// appendMentionImages renders the mention's direct images and returns a copy
// of the manifest stamped with the exact caption text delivered to the model.
// Images without a usable caption remain explicit in the prompt so the model
// cannot plausibly infer that it saw their contents.
func appendMentionImages(question string, images []mentionImage) (string, []mentionImage) {
	if len(images) == 0 {
		return question, nil
	}
	delivered := append([]mentionImage(nil), images...)
	var b strings.Builder
	b.WriteString(question)
	remaining := threadContextCaptionMax
	for i := range delivered {
		image := &delivered[i]
		if image.Caption != "" && remaining > 0 {
			caption := strings.TrimRight(image.Caption, "\n")
			if utf8.RuneCountInString(caption) > remaining {
				runes := []rune(caption)
				caption = string(runes[:remaining]) + "…"
			}
			remaining -= utf8.RuneCountInString(caption)
			caption = promptTagReplacer.Replace(caption)
			fmt.Fprintf(&b, "\n\n[attached image: %s]", caption)
			image.CaptionSnapshot = caption
			image.DeliveryStatus = "captioned"
			image.FailureReason = ""
			continue
		}

		if image.Caption != "" {
			image.FailureReason = "caption_budget_exhausted"
		}
		image.DeliveryStatus = "unavailable"
		filename := strings.TrimSpace(image.Filename)
		if filename == "" {
			filename = image.SourceImageID
		}
		if filename == "" {
			filename = "unnamed image"
		}
		filename = promptTagReplacer.Replace(filename)
		fmt.Fprintf(&b, "\n\n[attached image unavailable: %s. The image contents were not delivered. Tell the asker you could not read this attachment, and do not answer as though you saw it.]", filename)
	}
	return b.String(), delivered
}

// appendMentionDocuments injects extracted text and marks pending documents.
func appendMentionDocuments(question string, docs []mentionDocument) string {
	if len(docs) == 0 {
		return question
	}
	var b strings.Builder
	remaining := docAppendixBudget
	for _, d := range docs {
		name := promptTagReplacer.Replace(d.filename)
		if !d.settled {
			fmt.Fprintf(&b, "\n\n[attached document: %s — text not extracted yet. "+
				"Say the document is still being processed rather than answering "+
				"as if it were empty.]", name)
			continue
		}
		text := strings.TrimSpace(d.text)
		if text == "" {
			continue
		}
		if remaining <= 0 {
			fmt.Fprintf(&b, "\n\n[attached document: %s — omitted, extracted-text "+
				"budget exhausted]", name)
			continue
		}
		budget := perDocBudget
		if budget > remaining {
			budget = remaining
		}
		text, truncated := truncateRunesMarked(text, budget)
		remaining -= utf8.RuneCountInString(text)
		text = promptTagReplacer.Replace(text)

		fmt.Fprintf(&b, "\n\n[attached document: %s", name)
		if d.completeness != "complete" {
			fmt.Fprintf(&b, " — %s extraction", d.completeness)
		}
		fmt.Fprintf(&b, "]\n%s", text)
		if truncated {
			fmt.Fprintf(&b, "\n[...%s truncated; ask about a specific section for more]", name)
		}
	}
	if b.Len() == 0 {
		return question
	}
	return question + b.String()
}
