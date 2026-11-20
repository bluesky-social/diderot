package notion

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// blockJSON is a small helper to build a Block with one
// type-specific payload field. Skips dealing with the
// UnmarshalJSON catch-all path — tests just need the Payload map
// populated.
func blockJSON(t *testing.T, id, blockType string, payload any) Block {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Block{
		ID:   id,
		Type: blockType,
		Payload: map[string]json.RawMessage{
			blockType: raw,
		},
	}
}

func TestExtractImagesFileBlock(t *testing.T) {
	blocks := []Block{
		blockJSON(t, "block-uuid-1", "image", map[string]any{
			"type": "file",
			"file": map[string]any{
				"url": "https://prod-files-secure.s3.us-west-2.amazonaws.com/" +
					"workspace-id/abc-def/screenshot.png?X-Amz-Sig=ROTATING",
			},
		}),
	}
	got, err := ExtractImages(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d refs, want 1", len(got))
	}
	bid, ck, ok := SplitSourceImageID(got[0].SourceImageID)
	if !ok || bid != "block-uuid-1" || ck == "" {
		t.Errorf("source_image_id parse failed: id=%q ck=%q ok=%v", bid, ck, ok)
	}
	if got[0].Mimetype != "image/png" {
		t.Errorf("mimetype: got %q want image/png", got[0].Mimetype)
	}
}

func TestExtractImagesFileBlockSignatureRotationStable(t *testing.T) {
	// The contentKey must be stable across re-fetches even though
	// the URL's query-string signature rotates every blocks.retrieve.
	makeBlock := func(sig string) Block {
		return blockJSON(t, "block-uuid-1", "image", map[string]any{
			"type": "file",
			"file": map[string]any{
				"url": "https://prod-files-secure.s3.us-west-2.amazonaws.com/" +
					"workspace-id/abc-def/screenshot.png?X-Amz-Sig=" + sig,
			},
		})
	}
	r1, err := ExtractImages(context.Background(), []Block{makeBlock("AAA")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := ExtractImages(context.Background(), []Block{makeBlock("BBB")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1[0].SourceImageID != r2[0].SourceImageID {
		t.Errorf("contentKey changed across signature rotations: %q vs %q",
			r1[0].SourceImageID, r2[0].SourceImageID)
	}
}

func TestExtractImagesFileReuploadDifferentKey(t *testing.T) {
	// A re-upload of the same block changes the path component
	// (different file_uuid in the URL). contentKey MUST change so
	// reconcile creates a new event_images row.
	original := blockJSON(t, "block-uuid-1", "image", map[string]any{
		"type": "file",
		"file": map[string]any{
			"url": "https://prod-files-secure.s3.us-west-2.amazonaws.com/" +
				"workspace-id/abc-def/screenshot.png?sig=X",
		},
	})
	reuploaded := blockJSON(t, "block-uuid-1", "image", map[string]any{
		"type": "file",
		"file": map[string]any{
			"url": "https://prod-files-secure.s3.us-west-2.amazonaws.com/" +
				"workspace-id/different-uuid/screenshot.png?sig=Y",
		},
	})
	a, _ := ExtractImages(context.Background(), []Block{original}, nil)
	b, _ := ExtractImages(context.Background(), []Block{reuploaded}, nil)
	if a[0].SourceImageID == b[0].SourceImageID {
		t.Errorf("re-upload should change contentKey: both = %q", a[0].SourceImageID)
	}
}

func TestExtractImagesExternalBlock(t *testing.T) {
	blocks := []Block{
		blockJSON(t, "block-uuid-2", "image", map[string]any{
			"type": "external",
			"external": map[string]any{
				"url": "https://example.com/img.jpg",
			},
		}),
	}
	got, err := ExtractImages(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d refs, want 1", len(got))
	}
	if got[0].Mimetype != "image/jpeg" {
		t.Errorf("mimetype: got %q want image/jpeg", got[0].Mimetype)
	}
}

func TestExtractImagesExtensionlessURLGetsGenericImageMimetype(t *testing.T) {
	// S3 signed URLs and Notion's own file-serving proxy frequently
	// omit a filename suffix entirely. The fallback mimetype MUST
	// still carry the "image/" prefix because event_images is an
	// image-only table and every row here is unconditionally a
	// Notion "image" block by construction.
	blocks := []Block{
		blockJSON(t, "block-uuid-3", "image", map[string]any{
			"type": "file",
			"file": map[string]any{
				"url": "https://prod-files-secure.s3.us-west-2.amazonaws.com/" +
					"workspace-id/abc-def-uuid-no-suffix?X-Amz-Sig=ROTATING",
			},
		}),
	}
	got, err := ExtractImages(context.Background(), blocks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d refs, want 1", len(got))
	}
	if !strings.HasPrefix(got[0].Mimetype, "image/") {
		t.Errorf("mimetype: got %q, want image/* prefix", got[0].Mimetype)
	}
}

func TestExtractImagesNested(t *testing.T) {
	// An image nested inside a toggle block should be found via
	// the recursive walk, not just the top level.
	parent := Block{
		ID:          "toggle-1",
		Type:        "toggle",
		HasChildren: true,
		Payload:     map[string]json.RawMessage{},
	}
	child := blockJSON(t, "image-in-toggle", "image", map[string]any{
		"type": "external",
		"external": map[string]any{
			"url": "https://example.com/nested.png",
		},
	})

	fetch := func(ctx context.Context, id string) ([]Block, error) {
		if id == "toggle-1" {
			return []Block{child}, nil
		}
		return nil, nil
	}
	got, err := ExtractImages(context.Background(), []Block{parent}, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d refs, want 1", len(got))
	}
	bid, _, _ := SplitSourceImageID(got[0].SourceImageID)
	if bid != "image-in-toggle" {
		t.Errorf("expected nested image extracted; got block_id=%q", bid)
	}
}

func TestExtractImagesNoFetcherSkipsNested(t *testing.T) {
	// When fetchChildren is nil, has_children blocks are not
	// recursed into. Top-level images still found.
	top := blockJSON(t, "top-image", "image", map[string]any{
		"type": "external",
		"external": map[string]any{
			"url": "https://example.com/top.png",
		},
	})
	parent := Block{
		ID:          "toggle-1",
		Type:        "toggle",
		HasChildren: true,
		Payload:     map[string]json.RawMessage{},
	}
	got, err := ExtractImages(context.Background(), []Block{top, parent}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d refs, want 1", len(got))
	}
}

func TestExtractImagesArchivedSkipped(t *testing.T) {
	b := blockJSON(t, "img-archived", "image", map[string]any{
		"type": "external",
		"external": map[string]any{
			"url": "https://example.com/x.png",
		},
	})
	b.Archived = true
	got, err := ExtractImages(context.Background(), []Block{b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("archived image should be skipped: got %d refs", len(got))
	}
}

func TestExtractImagesMalformedSkipped(t *testing.T) {
	// Block claims type=image but payload has neither file nor
	// external — extractor returns ok=false; the walk continues
	// past it without erroring.
	bad := Block{
		ID:   "bad-1",
		Type: "image",
		Payload: map[string]json.RawMessage{
			"image": json.RawMessage(`{"type":"file"}`),
		},
	}
	good := blockJSON(t, "good-1", "image", map[string]any{
		"type": "external",
		"external": map[string]any{
			"url": "https://example.com/good.png",
		},
	})
	got, err := ExtractImages(context.Background(), []Block{bad, good}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d refs, want 1 (bad block skipped)", len(got))
	}
	bid, _, _ := SplitSourceImageID(got[0].SourceImageID)
	if bid != "good-1" {
		t.Errorf("expected the good block; got %q", bid)
	}
}

func TestSplitSourceImageID(t *testing.T) {
	tests := []struct {
		in   string
		bID  string
		ck   string
		want bool
	}{
		{"abc-123:deadbeef0123", "abc-123", "deadbeef0123", true},
		{"no-colon", "", "", false},
		{":only-suffix", "", "only-suffix", true},
		{"only-prefix:", "only-prefix", "", true},
	}
	for _, tc := range tests {
		gotID, gotCK, gotOK := SplitSourceImageID(tc.in)
		if gotOK != tc.want || gotID != tc.bID || gotCK != tc.ck {
			t.Errorf("Split(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, gotID, gotCK, gotOK, tc.bID, tc.ck, tc.want)
		}
	}
}

func TestContentKeyForBlock(t *testing.T) {
	b := blockJSON(t, "x", "image", map[string]any{
		"type": "external",
		"external": map[string]any{
			"url": "https://example.com/foo.png",
		},
	})
	ck, ok := ContentKeyForBlock(&b)
	if !ok || ck == "" {
		t.Fatalf("ContentKeyForBlock failed: ok=%v ck=%q", ok, ck)
	}
	// Same block twice → same contentKey.
	ck2, _ := ContentKeyForBlock(&b)
	if ck != ck2 {
		t.Errorf("contentKey not stable across calls: %q vs %q", ck, ck2)
	}
}
