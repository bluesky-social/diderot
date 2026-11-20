package slack

import (
	"strings"

	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
)

// ImageRefsFromAttachments projects slack's per-file
// FileAttachment list into the source-agnostic []events.ImageRef
// shape consumed by events.ReconcileImages. Slack's FileAttachment
// carries Name + Size for human-readable rendering elsewhere; the
// reconcile path only needs the (id, mimetype) tuple.
//
// The actual reconcile logic lives in
// internal/events/reconcile_images.go because every source's
// image-bearing event ends up in the same shared event_images
// table. This file is the slack-specific thin adapter.
func ImageRefsFromAttachments(atts []FileAttachment) []events.ImageRef {
	if len(atts) == 0 {
		return nil
	}
	out := make([]events.ImageRef, 0, len(atts))
	for _, a := range atts {
		if a.FileID == "" || !files.CaptionableImage(a.Mimetype) {
			continue
		}
		out = append(out, events.ImageRef{
			SourceImageID: a.FileID,
			Mimetype:      a.Mimetype,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DocumentRefsFromAttachments projects the FileAttachment list into
// []events.DocumentRef, keeping only attachments whose text we can
// actually extract. The source-specific filtering lives here rather
// than in the reconcile path or the worker, so an unextractable
// attachment never becomes a queue row that later dead-letters.
//
// Four filters, in order of how much they exclude:
//
//  1. mode == "hosted". This is the decisive one and it's about
//     Slack's storage class, not the file type: "external" files
//     (Google Docs/Sheets/Slides, usually the largest non-hosted
//     category) have no bytes in Slack at all, only a link.
//     "snippet" content is already inline in the message object,
//     "email" and "quip" are different shapes.
//  2. Not image/*. Images belong to the caption pipeline
//     (event_images); double-processing them would produce two
//     competing chunk kinds for the same file.
//  3. Not video/* or audio/*. Nothing to extract — transcription is a
//     different project.
//  4. A format supported by the extraction router.
func DocumentRefsFromAttachments(atts []FileAttachment) []events.DocumentRef {
	if len(atts) == 0 {
		return nil
	}
	out := make([]events.DocumentRef, 0, len(atts))
	for _, a := range atts {
		if a.FileID == "" {
			continue
		}
		// Slack omits mode on some bot uploads; treat empty as hosted
		// since those do carry url_private. Any other explicit mode is
		// excluded.
		if a.Mode != "" && a.Mode != "hosted" {
			continue
		}
		mt := strings.ToLower(strings.TrimSpace(a.Mimetype))
		if strings.HasPrefix(mt, "image/") ||
			strings.HasPrefix(mt, "video/") ||
			strings.HasPrefix(mt, "audio/") {
			continue
		}
		format := events.DocFormatForMediaType(a.Mimetype, a.Name)
		if format == "" {
			continue
		}
		out = append(out, events.DocumentRef{
			SourceFileID: a.FileID,
			Filename:     attachmentFilename(a),
			Mimetype:     a.Mimetype,
			DocFormat:    format,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// attachmentFilename picks the name shown in chunk context and citations.
func attachmentFilename(a FileAttachment) string {
	if a.Name != "" {
		return a.Name
	}
	if a.Title != "" {
		return a.Title
	}
	return "(unnamed)"
}
