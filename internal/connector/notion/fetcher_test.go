package notion

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	files "github.com/bluesky-social/diderot/internal/attachments/files"
)

func TestFetchImageRefreshesExpiredSignedURL(t *testing.T) {
	bytesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("X-Amz-Signature") == "old" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fresh-image"))
	}))
	defer bytesServer.Close()

	oldURL := bytesServer.URL + "/image.png?X-Amz-Signature=old"
	newURL := bytesServer.URL + "/image.png?X-Amz-Signature=new"
	apiCalls := 0
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		imageURL := oldURL
		if apiCalls > 1 {
			imageURL = newURL
		}
		_, _ = fmt.Fprintf(w, `{"object":"block","id":"block-signed","type":"image","image":{"type":"file","file":{"url":%q}}}`, imageURL)
	}))
	defer apiServer.Close()

	client, err := NewClient(fastClientCfg(apiServer))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	fetcher := NewFetcher(client, bytesServer.Client(), nil)
	data, mediaType, err := fetcher.FetchImage(context.Background(),
		"block-signed:"+contentKey("file", oldURL), "")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if string(data) != "fresh-image" || mediaType != "image/png" {
		t.Fatalf("data=%q media=%q", data, mediaType)
	}
	if apiCalls != 2 {
		t.Fatalf("blocks.retrieve calls=%d, want 2", apiCalls)
	}
}

// TestDecodeDataURI table-tests the data: URI parser in isolation:
// a valid base64 PNG payload decodes to bytes+mimetype, a malformed
// base64 payload errors, and a non-base64 data URI errors too (we
// don't try to guess a decode for that shape).
func TestDecodeDataURI(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	validB64 := base64.StdEncoding.EncodeToString(pngBytes)

	tests := []struct {
		name      string
		rawURL    string
		wantOK    bool
		wantErr   bool
		wantData  []byte
		wantMedia string
	}{
		{
			name:      "valid png data uri",
			rawURL:    "data:image/png;base64," + validB64,
			wantOK:    true,
			wantErr:   false,
			wantData:  pngBytes,
			wantMedia: "image/png",
		},
		{
			name:    "malformed base64 payload",
			rawURL:  "data:image/png;base64,not-valid-base64!!!",
			wantOK:  true,
			wantErr: true,
		},
		{
			name:    "non-base64 data uri",
			rawURL:  "data:text/plain,Hello%2C%20World!",
			wantOK:  true,
			wantErr: true,
		},
		{
			name:   "not a data uri at all",
			rawURL: "https://example.com/img.png",
			wantOK: false,
		},
		{
			name:      "uppercase DATA scheme is still recognized",
			rawURL:    "DATA:image/png;base64," + validB64,
			wantOK:    true,
			wantErr:   false,
			wantData:  pngBytes,
			wantMedia: "image/png",
		},
		{
			name:      "uppercase BASE64 marker is still recognized",
			rawURL:    "data:image/png;BASE64," + validB64,
			wantOK:    true,
			wantErr:   false,
			wantData:  pngBytes,
			wantMedia: "image/png",
		},
		{
			name:    "missing comma separator",
			rawURL:  "data:image/png;base64",
			wantOK:  true,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data, media, ok, err := decodeDataURI(tc.rawURL)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (err=%v)", ok, tc.wantOK, err)
			}
			if !tc.wantOK {
				return
			}
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(data) != string(tc.wantData) {
				t.Errorf("data = %v, want %v", data, tc.wantData)
			}
			if media != tc.wantMedia {
				t.Errorf("media = %q, want %q", media, tc.wantMedia)
			}
		})
	}
}

// notionBlockServer stands up a minimal httptest server that answers
// GET /blocks/{id} with a fixed image block body. Enough for
// FetchImage's blocks.retrieve call without a real Notion API.
func notionBlockServer(t *testing.T, blockJSONBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(blockJSONBody))
	}))
}

// TestFetchImageDataURI exercises FetchImage end to end for a block
// whose image url is a data: URI: no HTTP GET for the bytes should
// be needed (there's no second server to hit), and the returned
// bytes + mimetype should match the decoded payload.
func TestFetchImageDataURI(t *testing.T) {
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	dataURI := "data:image/png;base64," + b64
	ck := contentKey("file", dataURI)

	blockBody := `{
		"object": "block",
		"id": "block-1",
		"type": "image",
		"has_children": false,
		"archived": false,
		"image": {
			"type": "file",
			"file": {"url": "` + dataURI + `"}
		}
	}`
	srv := notionBlockServer(t, blockBody)
	defer srv.Close()

	client, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	f := NewFetcher(client, nil, nil)

	data, ct, err := f.FetchImage(context.Background(), "block-1:"+ck, "")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if string(data) != string(pngBytes) {
		t.Errorf("data = %v, want %v", data, pngBytes)
	}
	if ct != "image/png" {
		t.Errorf("content type = %q, want image/png", ct)
	}
}

// TestFetchImageMalformedDataURIIsPermanent: a malformed data: URI (invalid base64) is terminal — the
// block's payload doesn't change on retry — so FetchImage must
// return a *files.PermanentError rather than an ordinary error that
// would otherwise retry across claim cycles before poisoning.
func TestFetchImageMalformedDataURIIsPermanent(t *testing.T) {
	dataURI := "data:image/png;base64,not-valid-base64!!!"
	ck := contentKey("file", dataURI)

	blockBody := `{
		"object": "block",
		"id": "block-3",
		"type": "image",
		"has_children": false,
		"archived": false,
		"image": {
			"type": "file",
			"file": {"url": "` + dataURI + `"}
		}
	}`
	srv := notionBlockServer(t, blockBody)
	defer srv.Close()

	client, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	f := NewFetcher(client, nil, nil)

	_, _, err = f.FetchImage(context.Background(), "block-3:"+ck, "")
	if err == nil {
		t.Fatal("expected an error for a malformed data uri")
	}
	var pe *files.PermanentError
	if !errors.As(err, &pe) {
		t.Errorf("expected *files.PermanentError, got %T: %v", err, err)
	}
}

// TestFetchImageNonHTTPSchemeIsPermanent covers the
// relative-URL check: a URL with a non-http(s) scheme (e.g.
// ftp://) has a non-empty scheme and host, so a bare
// "scheme=="" || host==""" check lets it through to the HTTP client,
// which fails with "unsupported protocol scheme" — an error that
// isn't classified transient or permanent and would retry pointlessly.
// bytesClient can only ever fetch http(s), so this must be permanent.
func TestFetchImageNonHTTPSchemeIsPermanent(t *testing.T) {
	rawURL := "ftp://example.com/image.png"
	ck := contentKey("external", rawURL)

	blockBody := `{
		"object": "block",
		"id": "block-4",
		"type": "image",
		"has_children": false,
		"archived": false,
		"image": {
			"type": "external",
			"external": {"url": "` + rawURL + `"}
		}
	}`
	srv := notionBlockServer(t, blockBody)
	defer srv.Close()

	client, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	f := NewFetcher(client, nil, nil)

	_, _, err = f.FetchImage(context.Background(), "block-4:"+ck, "")
	if err == nil {
		t.Fatal("expected an error for a non-http(s) scheme")
	}
	var pe *files.PermanentError
	if !errors.As(err, &pe) {
		t.Errorf("expected *files.PermanentError, got %T: %v", err, err)
	}
}

// TestFetchImageRelativeURLIsPermanent exercises the schemeless/
// relative URL path: Notion doesn't serve integration-relative
// assets, so there's nothing to fetch against — FetchImage must
// return an error satisfying errors.As(&files.PermanentError{}) so
// the worker jumps straight to the poison cap instead of burning 5
// transient-retry cycles.
func TestFetchImageRelativeURLIsPermanent(t *testing.T) {
	rawURL := "/images/app-packages/some-icon.png"
	ck := contentKey("external", rawURL)

	blockBody := `{
		"object": "block",
		"id": "block-2",
		"type": "image",
		"has_children": false,
		"archived": false,
		"image": {
			"type": "external",
			"external": {"url": "` + rawURL + `"}
		}
	}`
	srv := notionBlockServer(t, blockBody)
	defer srv.Close()

	client, err := NewClient(fastClientCfg(srv))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	f := NewFetcher(client, nil, nil)

	_, _, err = f.FetchImage(context.Background(), "block-2:"+ck, "")
	if err == nil {
		t.Fatal("expected an error for a relative/schemeless image url")
	}
	var pe *files.PermanentError
	if !errors.As(err, &pe) {
		t.Errorf("expected *files.PermanentError, got %T: %v", err, err)
	}
}
