package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/source"
)

// Expansion bounds: a thread's first messages, or the messages around one
// in its channel.
const (
	expandThreadLimit = 200
	expandAround      = 15
)

type expandRow struct {
	ID         int64
	ExternalID string
	ThreadID   string
	Actor      string
	OccurredAt time.Time
	Body       string
	Raw        []byte
}

// Expand returns the conversation an event is part of: its whole thread,
// or, for a message in a channel, the messages around it and the one it
// replies to. It returns only what scopes may read.
func (Connector) Expand(ctx context.Context, deps source.Deps, eventID int64, scopes []string) (source.Expanded, error) {
	readable := events.ReadableScopes(scopes)
	var focus expandRow
	err := deps.Pool.QueryRow(ctx, `
		SELECT id, external_id, COALESCE(thread_id, ''), COALESCE(actor, ''), occurred_at, body, raw::text
		  FROM events WHERE id = $1 AND source = $2 AND NOT deleted`, eventID, ConnectorName).
		Scan(&focus.ID, &focus.ExternalID, &focus.ThreadID, &focus.Actor, &focus.OccurredAt, &focus.Body, &focus.Raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return source.Expanded{}, events.ErrEventNotFound
	}
	if err != nil {
		return source.Expanded{}, fmt.Errorf("discord expand: %w", err)
	}
	channelPrefix := focus.ExternalID[:strings.LastIndexByte(focus.ExternalID, '/')+1]
	var rows []expandRow
	var truncated bool
	if focus.ThreadID != "" {
		rows, err = queryRows(ctx, deps, `
			SELECT id, external_id, COALESCE(thread_id, ''), COALESCE(actor, ''), occurred_at, body, raw::text
			  FROM events
			 WHERE source = $1 AND (thread_id = $2 OR external_id = $2) AND NOT deleted AND scope = ANY($3)
			 ORDER BY occurred_at, id LIMIT $4`, ConnectorName, focus.ThreadID, readable, expandThreadLimit+1)
		if len(rows) > expandThreadLimit {
			rows, truncated = rows[:expandThreadLimit], true
		}
	} else {
		var before, after []expandRow
		before, err = queryRows(ctx, deps, `
			SELECT id, external_id, COALESCE(thread_id, ''), COALESCE(actor, ''), occurred_at, body, raw::text
			  FROM events
			 WHERE source = $1 AND starts_with(external_id, $2) AND (thread_id IS NULL OR thread_id = '' OR thread_id = external_id)
			   AND NOT deleted AND scope = ANY($3) AND (occurred_at, id) < ($4, $5)
			 ORDER BY occurred_at DESC, id DESC LIMIT $6`, ConnectorName, channelPrefix, readable, focus.OccurredAt, focus.ID, expandAround)
		if err == nil {
			after, err = queryRows(ctx, deps, `
				SELECT id, external_id, COALESCE(thread_id, ''), COALESCE(actor, ''), occurred_at, body, raw::text
				  FROM events
				 WHERE source = $1 AND starts_with(external_id, $2) AND (thread_id IS NULL OR thread_id = '' OR thread_id = external_id)
				   AND NOT deleted AND scope = ANY($3) AND (occurred_at, id) >= ($4, $5)
				 ORDER BY occurred_at, id LIMIT $6`, ConnectorName, channelPrefix, readable, focus.OccurredAt, focus.ID, expandAround+1)
		}
		for i := len(before) - 1; i >= 0; i-- {
			rows = append(rows, before[i])
		}
		rows = append(rows, after...)
		truncated = len(before) == expandAround || len(after) > expandAround
	}
	if err != nil {
		return source.Expanded{}, fmt.Errorf("discord expand: %w", err)
	}
	var b strings.Builder
	if ref := repliedTo(ctx, deps, focus, channelPrefix, readable, rows); ref != nil {
		fmt.Fprintf(&b, "In reply to:\n%s\n\n", line(*ref, false))
	}
	if focus.ThreadID != "" {
		b.WriteString("The thread:\n")
	} else {
		b.WriteString("The channel around the message (marked »):\n")
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		b.WriteString(line(row, row.ID == focus.ID && focus.ThreadID == ""))
		b.WriteString("\n")
		ids = append(ids, row.ID)
	}
	attachments, err := events.ListAttachments(ctx, deps.Pool, ids)
	if err != nil {
		return source.Expanded{}, fmt.Errorf("discord expand: %w", err)
	}
	return source.Expanded{Content: strings.TrimRight(b.String(), "\n"), Truncated: truncated, Attachments: attachments}, nil
}

func line(row expandRow, focus bool) string {
	mark := ""
	if focus {
		mark = "» "
	}
	return fmt.Sprintf("%s[%s] %s: %s", mark, row.OccurredAt.UTC().Format("2006-01-02 15:04"), row.Actor, row.Body)
}

func queryRows(ctx context.Context, deps source.Deps, sql string, args ...any) ([]expandRow, error) {
	rows, err := deps.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (expandRow, error) {
		var r expandRow
		err := row.Scan(&r.ID, &r.ExternalID, &r.ThreadID, &r.Actor, &r.OccurredAt, &r.Body, &r.Raw)
		return r, err
	})
}

// repliedTo is the message the focus replies to, when it is indexed,
// readable, and not already in rows.
func repliedTo(ctx context.Context, deps source.Deps, focus expandRow, channelPrefix string, readable []string, rows []expandRow) *expandRow {
	var raw struct {
		MessageReference *messageReference `json:"message_reference"`
	}
	if json.Unmarshal(focus.Raw, &raw) != nil || raw.MessageReference == nil || raw.MessageReference.Type != 0 || raw.MessageReference.MessageID == "" {
		return nil
	}
	target := channelPrefix + raw.MessageReference.MessageID
	for _, row := range rows {
		if row.ExternalID == target {
			return nil
		}
	}
	found, err := queryRows(ctx, deps, `
		SELECT id, external_id, COALESCE(thread_id, ''), COALESCE(actor, ''), occurred_at, body, raw::text
		  FROM events WHERE source = $1 AND external_id = $2 AND NOT deleted AND scope = ANY($3)`, ConnectorName, target, readable)
	if err != nil || len(found) == 0 {
		return nil
	}
	return &found[0]
}
