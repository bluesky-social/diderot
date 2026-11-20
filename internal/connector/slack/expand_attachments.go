package slack

import (
	"context"

	"github.com/bluesky-social/diderot/internal/events"
)

// attachments lists the files on the rendered messages: every file Slack
// reported on each message, in Slack's order, joined to what the caption and
// document workers derived from it. A file Diderot did not ingest (a snippet,
// an external link, an unsupported type) is listed with its name and no
// Kind, so a reader knows it exists and that its content is not held.
//
// Like the document appendix, this reads by event_id from a set the expander
// has already filtered to the caller's scopes.
//
// It is best-effort: the conversation is still useful without its file list,
// so a failed lookup returns nil.
func (e *Expander) attachments(ctx context.Context, eventIDs []int64) []events.Attachment {
	derived, err := events.ListAttachments(ctx, e.pool, eventIDs)
	if err != nil {
		return nil
	}
	byFile := make(map[int64]map[string]events.Attachment, len(eventIDs))
	for _, a := range derived {
		if byFile[a.EventID] == nil {
			byFile[a.EventID] = map[string]events.Attachment{}
		}
		byFile[a.EventID][a.FileID] = a
	}

	rows, err := e.pool.Query(ctx, `
		SELECT e.id, e.raw
		  FROM events e
		  JOIN slack_message_details d ON d.event_id = e.id
		 WHERE e.id = ANY($1::bigint[]) AND d.has_files = true AND e.deleted = false
	`, eventIDs)
	if err != nil {
		return nil
	}
	defer rows.Close()
	reported := make(map[int64][]FileAttachment, len(eventIDs))
	for rows.Next() {
		var (
			eventID int64
			raw     []byte
		)
		if err := rows.Scan(&eventID, &raw); err != nil {
			return nil
		}
		if details, err := DetailsFrom(raw); err == nil {
			reported[eventID] = details.Attachments
		}
	}
	if rows.Err() != nil {
		return nil
	}

	var out []events.Attachment
	for _, eventID := range eventIDs {
		for _, file := range reported[eventID] {
			a, ok := byFile[eventID][file.FileID]
			if ok {
				delete(byFile[eventID], file.FileID)
			} else {
				a = events.Attachment{AttachmentRef: events.AttachmentRef{EventID: eventID, FileID: file.FileID}}
			}
			if a.Name == "" {
				a.Name = attachmentFilename(file)
			}
			if a.MediaType == "" {
				a.MediaType = file.Mimetype
			}
			if a.Bytes == 0 {
				a.Bytes = file.Size
			}
			out = append(out, a)
		}
	}
	// Derived rows Slack's stored message no longer reports keep their
	// caption and text reachable.
	for _, a := range derived {
		if _, left := byFile[a.EventID][a.FileID]; left {
			out = append(out, a)
		}
	}
	return out
}
