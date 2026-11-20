package files

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// recordingMirror is a test S3Mirror that records every Get key in
// order and resolves them through a caller-supplied handler, so a
// test can assert both which keys were probed and in what order.
type recordingMirror struct {
	get   func(key string) ([]byte, string, error)
	order []string
}

func (m *recordingMirror) Put(context.Context, string, string, []byte) error { return nil }

func (m *recordingMirror) Bucket() string { return "test-bucket" }

func (m *recordingMirror) Get(_ context.Context, key string) ([]byte, string, error) {
	m.order = append(m.order, key)
	return m.get(key)
}

// missingErr mimics s3.go's NoSuchKey classification: the message
// carries the "(missing)" trailer isMissingKey matches on.
func missingErr(key string) error {
	return fmt.Errorf("GetObject bucket/%s: NoSuchKey (missing)", key)
}

// deniedErr mimics the AccessDenied a missing key returns to
// credentials with GetObject but not ListBucket. Crucially it does NOT contain
// "(missing)", so it must never be mistaken for a missing key.
func deniedErr(key string) error {
	return fmt.Errorf("GetObject bucket/%s: operation error S3: AccessDenied, 403: denied", key)
}

const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newTestFetcher(m *recordingMirror) *AgentSessionFetcher {
	return NewAgentSessionFetcher(m)
}

// A JPEG media type must resolve straight to the .jpg key without
// ever probing .png first. An ordered probe aborts on a missing
// first extension before reaching the real file.
func TestAgentSessionFetcherJPEGDoesNotProbePNGFirst(t *testing.T) {
	m := &recordingMirror{get: func(key string) ([]byte, string, error) {
		if strings.HasSuffix(key, ".jpg") {
			return []byte("jpeg-bytes"), "image/jpeg", nil
		}
		return nil, "", missingErr(key)
	}}
	f := newTestFetcher(m)

	data, ct, err := f.FetchImage(context.Background(), "sha256:"+testSHA, "image/jpeg")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if string(data) != "jpeg-bytes" || ct != "image/jpeg" {
		t.Fatalf("got %q/%q, want jpeg-bytes/image/jpeg", data, ct)
	}

	// The .jpg key must have been the very first (and only) probe.
	if len(m.order) != 1 || !strings.HasSuffix(m.order[0], ".jpg") {
		t.Fatalf("probed keys = %v, want only the .jpg key first", m.order)
	}
	for _, k := range m.order {
		if strings.Contains(k, ".png") {
			t.Fatalf("JPEG lookup probed a .png key first: %v", m.order)
		}
	}
}

// A genuine authorization failure (AccessDenied, not "(missing)")
// must surface as an error rather than being silently treated as a
// missing key and blended into the probe fallback.
func TestAgentSessionFetcherAuthorizationFailureSurfaces(t *testing.T) {
	m := &recordingMirror{get: func(key string) ([]byte, string, error) {
		return nil, "", deniedErr(key)
	}}
	f := newTestFetcher(m)

	_, _, err := f.FetchImage(context.Background(), "sha256:"+testSHA, "image/png")
	if err == nil {
		t.Fatal("FetchImage: expected AccessDenied to surface, got nil")
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("error should carry AccessDenied, got %q", err)
	}
	// The probe must stop at the first non-missing error — it must
	// not keep trying later extensions on the assumption of "missing".
	if len(m.order) != 1 {
		t.Fatalf("authorization error probed %d keys, want 1 (no fallthrough): %v", len(m.order), m.order)
	}
}

// A genuine NoSuchKey ("(missing)") on the media-typed key is the one
// case that may fall through to the extension probe (stale/mismatched
// media_type) — the safety net must still work without treating 403 as
// missing.
func TestAgentSessionFetcherGenuineMissingFallsToProbe(t *testing.T) {
	m := &recordingMirror{get: func(key string) ([]byte, string, error) {
		if strings.HasSuffix(key, ".webp") {
			return []byte("webp-bytes"), "image/webp", nil
		}
		return nil, "", missingErr(key)
	}}
	f := newTestFetcher(m)

	// media_type claims jpeg but the object only lives as .webp:
	// the exact .jpg key is a genuine NoSuchKey, so the probe should
	// continue and find .webp.
	data, ct, err := f.FetchImage(context.Background(), "sha256:"+testSHA, "image/jpeg")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if string(data) != "webp-bytes" || ct != "image/webp" {
		t.Fatalf("got %q/%q, want webp-bytes/image/webp", data, ct)
	}
	if len(m.order) < 2 || !strings.HasSuffix(m.order[0], ".jpg") {
		t.Fatalf("probe order wrong, want .jpg then .webp: %v", m.order)
	}
}

// The transcript pre-pass historically treats noncanonical MIME strings as
// unknown and writes an extensionless object. The fetcher must mirror that
// exact behavior instead of normalizing first, or a 403 on a nonexistent
// normalized key prevents it from reaching the real object.
func TestAgentSessionFetcherMirrorsWriterExtensionMapping(t *testing.T) {
	m := &recordingMirror{get: func(key string) ([]byte, string, error) {
		if !strings.Contains(key, ".") {
			return []byte("legacy-bytes"), "image/jpeg; charset=binary", nil
		}
		return nil, "", deniedErr(key)
	}}
	f := newTestFetcher(m)

	data, _, err := f.FetchImage(context.Background(), "sha256:"+testSHA, "image/jpeg; charset=binary")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if string(data) != "legacy-bytes" {
		t.Fatalf("got %q, want legacy-bytes", data)
	}
	if len(m.order) != 1 || strings.Contains(m.order[0], ".") {
		t.Fatalf("expected writer-compatible extensionless lookup, got %v", m.order)
	}
}

// Unknown/empty media_type maps to the extensionless key, which is
// what the pre-pass wrote for non-image/unknown payloads; it must be
// tried first.
func TestAgentSessionFetcherUnknownMediaTypeUsesExtensionlessKey(t *testing.T) {
	m := &recordingMirror{get: func(key string) ([]byte, string, error) {
		if !strings.Contains(key, ".") {
			return []byte("raw-bytes"), "application/octet-stream", nil
		}
		return nil, "", missingErr(key)
	}}
	f := newTestFetcher(m)

	data, _, err := f.FetchImage(context.Background(), "sha256:"+testSHA, "application/octet-stream")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if string(data) != "raw-bytes" {
		t.Fatalf("got %q, want raw-bytes", data)
	}
	if len(m.order) != 1 || strings.Contains(m.order[0], ".") {
		t.Fatalf("expected a single extensionless probe, got %v", m.order)
	}
}
