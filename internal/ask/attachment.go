package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bluesky-social/diderot/internal/events"
)

const (
	// maxListedCaptionRunes bounds each caption in one expand result's
	// attachment list, and listedCaptionBudgetRunes all of them together.
	// Every attachment is always listed, so each stays fetchable; the
	// attachment tool returns a caption whole.
	maxListedCaptionRunes    = 1000
	listedCaptionBudgetRunes = 20000

	// maxAttachmentImageBytes is the largest image the attachment tool
	// returns as image content. Base64 grows it by a third on the wire, and
	// models refuse images much past this size.
	maxAttachmentImageBytes = 4 << 20
)

// AttachmentReader reads attachments for the attachment tool.
type AttachmentReader interface {
	// Attachment returns one attachment whose event is live and in scopes,
	// with a document's extracted text; anything else is
	// events.ErrAttachmentNotFound.
	Attachment(ctx context.Context, ref events.AttachmentRef, scopes []string) (events.Attachment, error)
	// Bytes returns the stored bytes at an attachment's StorageKey.
	Bytes(ctx context.Context, storageKey string) ([]byte, error)
}

// attachmentInfo is the model-facing description of one attachment, in
// expand's list and in the attachment tool's result.
type attachmentInfo struct {
	// ID is what the attachment tool takes. A file Diderot lists but does
	// not hold has none.
	ID      string `json:"id,omitempty"`
	EventID int64  `json:"event_id"`
	Name    string `json:"name,omitempty"`
	Type    string `json:"type,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Caption string `json:"caption,omitempty"`
}

func attachmentInfoOf(a events.Attachment) attachmentInfo {
	info := attachmentInfo{EventID: a.EventID, Name: a.Name, Type: a.MediaType, Size: a.Bytes, Caption: a.Caption}
	if a.Kind != "" {
		info.ID = a.String()
	}
	return info
}

// listedAttachments projects an expand result's attachments for the model.
// It lists every one, and bounds the captions: each is cut to
// maxListedCaptionRunes, and once the list has spent
// listedCaptionBudgetRunes the rest are listed without theirs.
func listedAttachments(attachments []events.Attachment) []attachmentInfo {
	if len(attachments) == 0 {
		return nil
	}
	listed := make([]attachmentInfo, 0, len(attachments))
	budget := listedCaptionBudgetRunes
	for _, a := range attachments {
		info := attachmentInfoOf(a)
		runes := []rune(info.Caption)
		switch {
		case budget <= 0:
			info.Caption = ""
		case len(runes) > maxListedCaptionRunes:
			info.Caption = string(runes[:maxListedCaptionRunes]) + "…"
			budget -= maxListedCaptionRunes
		default:
			budget -= len(runes)
		}
		listed = append(listed, info)
	}
	return listed
}

const attachmentToolDescription = "Fetch one attachment that an `expand` result lists under `attachments`, by its `id`. An image comes back as image content together with its caption. A document (PDF, DOCX, XLSX) comes back as its extracted text under `text`; when `next_offset` is present the text continues, so call again with that value as `offset`. When content is missing or incomplete (an image over 4 MB, an image not stored yet, a document not extracted yet or extracted only in part), `note` says why, and the caption, if there is one, is still included."

var attachmentSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {
      "type": "string",
      "description": "The id of an entry in an expand result's attachments."
    },
    "offset": {
      "type": "integer",
      "minimum": 0,
      "default": 0,
      "description": "For a document: the next_offset of the previous call, to continue its text."
    }
  },
  "required": ["id"]
}`)

type attachmentInput struct {
	ID     string      `json:"id"`
	Offset json.Number `json:"offset,omitempty"`
}

type attachmentOutput struct {
	attachmentInfo
	// Text is a document's extracted text from the requested offset.
	Text string `json:"text,omitempty"`
	// NextOffset is set when the document's text continues past Text.
	NextOffset int `json:"next_offset,omitempty"`
	// Note says why the content is absent or incomplete.
	Note string `json:"note,omitempty"`
}

// errAttachmentUnavailable is the one answer for an attachment that is
// missing, deleted, or outside the caller's scopes.
func errAttachmentUnavailable(id string) error {
	return fmt.Errorf("no attachment with id %q accessible from this context", id)
}

// readAttachment is the attachment tool: it gates on the parent event's scope
// before reading anything, exactly as expand does, then returns the
// attachment's description and, for an image small enough to return, its
// bytes.
func readAttachment(ctx context.Context, reader AttachmentReader, scopes ScopeLookup, in attachmentInput) (attachmentOutput, []byte, error) {
	if reader == nil {
		return attachmentOutput{}, nil, errors.New("attachments are not available on this deployment")
	}
	ref, err := events.ParseAttachmentRef(in.ID)
	if err != nil {
		return attachmentOutput{}, nil, err
	}
	offset := 0
	if in.Offset != "" {
		n, err := in.Offset.Int64()
		if err != nil || n < 0 {
			return attachmentOutput{}, nil, errors.New("offset must be a non-negative integer")
		}
		offset = int(n)
	}
	readable, err := eventReadable(ctx, scopes, ref.EventID)
	if err != nil {
		return attachmentOutput{}, nil, err
	}
	if !readable {
		return attachmentOutput{}, nil, errAttachmentUnavailable(in.ID)
	}
	attachment, err := reader.Attachment(ctx, ref, scopesFromContext(ctx))
	if errors.Is(err, events.ErrAttachmentNotFound) {
		return attachmentOutput{}, nil, errAttachmentUnavailable(in.ID)
	}
	if err != nil {
		return attachmentOutput{}, nil, err
	}
	out := attachmentOutput{attachmentInfo: attachmentInfoOf(attachment)}
	if attachment.Kind == events.AttachmentDocument {
		out.Text, out.NextOffset, out.Note, err = documentPage(attachment, offset)
		return out, nil, err
	}

	switch {
	case attachment.StorageKey == "":
		out.Note = "The image is not stored: either it has not been fetched yet, so try again in a minute, or this deployment keeps no attachment files."
		return out, nil, nil
	case attachment.Bytes > maxAttachmentImageBytes:
		out.Note = imageTooLargeNote(attachment.Bytes)
		return out, nil, nil
	}
	data, err := reader.Bytes(ctx, attachment.StorageKey)
	if err != nil {
		return attachmentOutput{}, nil, fmt.Errorf("read image: %w", err)
	}
	if len(data) > maxAttachmentImageBytes {
		out.Note = imageTooLargeNote(int64(len(data)))
		return out, nil, nil
	}
	return out, data, nil
}

func imageTooLargeNote(size int64) string {
	return fmt.Sprintf("The image is %.1f MB, over the %d MB limit for returning image content.",
		float64(size)/(1<<20), maxAttachmentImageBytes>>20)
}

// documentPage returns a document's extracted text from offset, cut to
// expand's content budget, the offset of what follows when it was cut, and a
// note when the text is absent or partial.
func documentPage(a events.Attachment, offset int) (text string, nextOffset int, note string, err error) {
	switch {
	case !a.Extracted:
		return "", 0, "The document's text has not been extracted yet; it should be readable within a minute.", nil
	case a.Text == "" && a.RejectedReason != "":
		return "", 0, "No text could be extracted from this document: " + a.RejectedReason + ".", nil
	case a.Text == "":
		return "", 0, "No text was extracted from this document.", nil
	}
	if offset > len(a.Text) || (offset < len(a.Text) && !utf8.RuneStart(a.Text[offset])) {
		return "", 0, "", errors.New("offset must be 0 or a next_offset this document returned")
	}
	text, cut := CapExpandContent(a.Text[offset:], 0)
	if cut {
		nextOffset = offset + len(text)
	}
	if a.IncompleteReason != "" {
		note = "Extraction of this document was partial (" + a.IncompleteReason + "); some pages are missing."
	}
	return text, nextOffset, note, nil
}

// attachmentMCPResult renders the tool's result: the description as JSON
// text and structured content, followed by the image when there is one.
func attachmentMCPResult(out attachmentOutput, image []byte) (*mcp.CallToolResult, error) {
	result, err := corpusMCPResult("attachment", out, nil)
	if err != nil || image == nil {
		return result, err
	}
	result.Content = append(result.Content, &mcp.ImageContent{Data: image, MIMEType: out.Type})
	return result, nil
}
