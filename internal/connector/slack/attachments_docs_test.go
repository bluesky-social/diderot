package slack

import "testing"

// TestDocumentRefsFromAttachmentsModeFiltering pins the decisive
// filter. Slack's `mode` — not the mimetype — determines whether there
// are bytes to fetch, and getting this wrong means queueing external
// files (Google Docs/Sheets/Slides) that can only ever dead-letter.
func TestDocumentRefsFromAttachmentsModeFiltering(t *testing.T) {
	atts := []FileAttachment{
		{FileID: "F1", Name: "contract.pdf", Mimetype: "application/pdf", Mode: "hosted"},
		// external: a Google Drive link. Slack holds no bytes at all.
		{FileID: "F2", Name: "plan.gdoc", Mimetype: "application/vnd.google-apps.document", Mode: "external"},
		// An externally-hosted PDF is still unfetchable — mode wins over
		// a mimetype we'd otherwise accept.
		{FileID: "F3", Name: "external.pdf", Mimetype: "application/pdf", Mode: "external"},
		// snippet: content is already inline in the message object.
		{FileID: "F4", Name: "paste.txt", Mimetype: "text/plain", Mode: "snippet"},
		// email / quip: different shapes entirely.
		{FileID: "F5", Name: "fwd.html", Mimetype: "text/html", Mode: "email"},
		{FileID: "F6", Name: "canvas", Mimetype: "application/vnd.slack-docs", Mode: "quip"},
		// Empty mode occurs on some bot uploads and does carry
		// url_private, so it's treated as hosted.
		{FileID: "F7", Name: "report.xlsx", Mimetype: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
	}
	got := DocumentRefsFromAttachments(atts)
	want := map[string]string{"F1": "pdf", "F7": "xlsx"}
	if len(got) != len(want) {
		t.Fatalf("got %d refs, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		format, ok := want[r.SourceFileID]
		if !ok {
			t.Errorf("unexpected ref %s (%s)", r.SourceFileID, r.Mimetype)
			continue
		}
		if r.DocFormat != format {
			t.Errorf("%s: DocFormat = %q, want %q", r.SourceFileID, r.DocFormat, format)
		}
	}
}

// TestDocumentRefsExcludesMediaTypesOwnedElsewhere: images belong to
// the caption pipeline. Double-processing one would produce two
// competing chunk kinds for the same file.
func TestDocumentRefsExcludesMediaTypesOwnedElsewhere(t *testing.T) {
	atts := []FileAttachment{
		{FileID: "F1", Name: "shot.png", Mimetype: "image/png", Mode: "hosted"},
		{FileID: "F2", Name: "clip.mp4", Mimetype: "video/mp4", Mode: "hosted"},
		{FileID: "F3", Name: "call.mp3", Mimetype: "audio/mpeg", Mode: "hosted"},
		{FileID: "F4", Name: "deck.pptx", Mimetype: "application/vnd.openxmlformats-officedocument.presentationml.presentation", Mode: "hosted"},
		{FileID: "F5", Name: "archive.zip", Mimetype: "application/zip", Mode: "hosted"},
	}
	if got := DocumentRefsFromAttachments(atts); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}

// TestDocumentRefsSkipsMissingFileID mirrors projectAttachments'
// behavior: Slack occasionally wraps a tombstoned file with empty
// fields, and a ref with no stable handle can't be reconciled.
func TestDocumentRefsSkipsMissingFileID(t *testing.T) {
	atts := []FileAttachment{
		{FileID: "", Name: "ghost.pdf", Mimetype: "application/pdf", Mode: "hosted"},
	}
	if got := DocumentRefsFromAttachments(atts); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}

func TestDocumentRefsFilenameFallback(t *testing.T) {
	atts := []FileAttachment{
		{FileID: "F1", Title: "Just A Title", Mimetype: "application/pdf", Mode: "hosted"},
	}
	got := DocumentRefsFromAttachments(atts)
	if len(got) != 1 {
		t.Fatalf("got %d refs", len(got))
	}
	if got[0].Filename != "Just A Title" {
		t.Errorf("Filename = %q, want the Title fallback", got[0].Filename)
	}
}

func TestDocumentRefsEmpty(t *testing.T) {
	if got := DocumentRefsFromAttachments(nil); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}
