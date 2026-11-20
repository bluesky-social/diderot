package notion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/bluesky-social/diderot/internal/events"
)

// ImageRef is the per-image projection produced by the walker's
// image-extraction pass and consumed in-process by
// events.ReconcileImages.
//
// SourceImageID is the composite "<block_id>:<contentKey>" rather than
// the bare block_id: contentKey changes when the underlying file URL
// changes (or when the author swaps an external URL), so a re-ingest of an
// in-place edit produces a different SourceImageID and reconcile
// DELETEs+INSERTs cleanly.
type ImageRef struct {
	SourceImageID string `json:"source_image_id"`
	Mimetype      string `json:"mimetype,omitempty"`
}

// AsEventsImageRef projects to the source-agnostic shape consumed
// by events.ReconcileImages. Symmetric to slack's
// ImageRefsFromAttachments helper.
func (r ImageRef) AsEventsImageRef() events.ImageRef {
	return events.ImageRef{
		SourceImageID: r.SourceImageID,
		Mimetype:      r.Mimetype,
	}
}

// ImageRefsToEvents projects a slice of notion ImageRefs to the
// source-agnostic []events.ImageRef.
func ImageRefsToEvents(refs []ImageRef) []events.ImageRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]events.ImageRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.AsEventsImageRef())
	}
	return out
}

// ExtractImages walks a block tree depth-first and collects every
// image block into []ImageRef. Recurses through any block with
// HasChildren=true via fetchChildren (same pattern the body
// renderer uses), so toggled / column-nested / synced-block-
// hosted images all get found. fetchChildren may be nil; in that
// case nested children are skipped (test mode).
//
// File and PDF and audio blocks are deliberately NOT extracted —
// only image/* types ride this pipeline today (per design doc §
// Out of scope). They stay rendered as markdown links in the body
// instead.
func ExtractImages(ctx context.Context, blocks []Block, fetchChildren childBlocksFn) ([]ImageRef, error) {
	var out []ImageRef
	if err := walkImages(ctx, blocks, fetchChildren, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func walkImages(ctx context.Context, blocks []Block, fetchChildren childBlocksFn, out *[]ImageRef) error {
	for i := range blocks {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		b := &blocks[i]
		if b.Archived || b.InTrash {
			continue
		}
		if b.Type == "image" {
			if ref, ok := imageRefFromBlock(b); ok {
				*out = append(*out, ref)
			}
		}
		// Recurse into any block with children. We recurse on
		// every block type (toggle, column, callout, list_item,
		// synced_block, etc.) rather than enumerate, because a
		// missed type silently drops images.
		if b.HasChildren && fetchChildren != nil {
			children, err := fetchChildren(ctx, b.ID)
			if err != nil {
				return fmt.Errorf("list children of %s: %w", b.ID, err)
			}
			if err := walkImages(ctx, children, fetchChildren, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// imageRefFromBlock parses the image block's payload, computes
// its contentKey, and returns the ImageRef. Returns ok=false for
// malformed blocks (no URL recoverable) so the caller can skip
// without erroring out the whole walk.
func imageRefFromBlock(b *Block) (ImageRef, bool) {
	raw, ok := b.Payload["image"]
	if !ok {
		return ImageRef{}, false
	}
	var body struct {
		Type string `json:"type"`
		File *struct {
			URL string `json:"url"`
		} `json:"file,omitempty"`
		External *struct {
			URL string `json:"url"`
		} `json:"external,omitempty"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ImageRef{}, false
	}
	var rawURL string
	switch {
	case body.File != nil && body.File.URL != "":
		rawURL = body.File.URL
	case body.External != nil && body.External.URL != "":
		rawURL = body.External.URL
	default:
		return ImageRef{}, false
	}
	ck := contentKey(body.Type, rawURL)
	return ImageRef{
		SourceImageID: b.ID + ":" + ck,
		Mimetype:      mimetypeFromURL(rawURL),
	}, true
}

// contentKey derives a short stable identifier for the underlying
// image bytes. Notion's signed file URLs have a path component
// that carries Notion's internal file UUID and a query string that
// carries a rotating signature; stripping the query gives us a
// stable per-file identifier. For external URLs the whole URL is
// stable per-author-choice, so we hash the whole thing.
//
// Returns the first 12 hex chars of sha256, which is plenty of
// collision space for the (per-event) namespace where this is
// keyed.
func contentKey(blockType, rawURL string) string {
	stable := rawURL
	if blockType == "file" {
		if u, err := url.Parse(rawURL); err == nil {
			u.RawQuery = ""
			u.Fragment = ""
			stable = u.String()
		}
	}
	sum := sha256.Sum256([]byte(stable))
	return hex.EncodeToString(sum[:6]) // 12 hex chars
}

// SplitSourceImageID recovers (block_id, content_key) from a
// composite source_image_id of the form "<block_id>:<contentKey>".
// Returns ok=false on a malformed input. Used by the fetcher to
// recover the block_id for blocks.retrieve.
func SplitSourceImageID(s string) (blockID, contentKey string, ok bool) {
	idx := strings.LastIndexByte(s, ':')
	if idx < 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// ContentKeyForBlock recomputes the contentKey from a freshly
// retrieved block. Used by the fetcher to verify that a stored
// source_image_id still matches the current block state — see
// design doc § "Image edits within a stable block" for the race
// scenario.
func ContentKeyForBlock(b *Block) (string, bool) {
	r, ok := imageRefFromBlock(b)
	if !ok {
		return "", false
	}
	_, ck, ok := SplitSourceImageID(r.SourceImageID)
	if !ok {
		return "", false
	}
	return ck, true
}

// mimetypeFromURL derives a best-effort mimetype from the URL's
// path extension. Notion's image block payload doesn't include a
// mimetype field — Notion derives it from the upload's filename
// at ingest time and never resurfaces it.
//
// The fallback (no recognizable extension — common for S3 signed
// URLs and Notion's own file-serving proxy, which frequently omit a
// filename suffix) MUST carry the "image/" prefix rather than the
// generic "application/octet-stream". The event_images schema is
// deliberately image-only, and every row ExtractImages produces
// comes from a b.Type == "image" block, so "this is *an* image,
// format unknown" is always an honest claim here.
// The caption worker's fetch-time Content-Type header is the
// authoritative mimetype for the actual VLM call
// (fetcher.go FetchImage + worker.go's `if ct == "" { ct =
// row.mediaType }`); this stored value is never used to pick a
// codec or feed real bytes anywhere.
func mimetypeFromURL(rawURL string) string {
	const unknownImageMimetype = "image/unknown"
	u, err := url.Parse(rawURL)
	if err != nil {
		return unknownImageMimetype
	}
	path := strings.ToLower(u.Path)
	switch {
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(path, ".gif"):
		return "image/gif"
	case strings.HasSuffix(path, ".webp"):
		return "image/webp"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	}
	return unknownImageMimetype
}
