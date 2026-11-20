package files

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeCaptioner records every Caption call it receives so tests can
// assert a VLM call did (or, more importantly, did NOT) happen.
type fakeCaptioner struct {
	calls  int
	result *CaptionResult
	err    error
	// before, when set, runs at the start of each Caption call to
	// simulate concurrent state changes during the VLM round-trip.
	before func()
}

func (f *fakeCaptioner) Caption(_ context.Context, _ string, _ []byte) (*CaptionResult, error) {
	f.calls++
	if f.before != nil {
		f.before()
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &CaptionResult{Caption: "a fake caption", ModelID: "fake-model"}, nil
}

func (f *fakeCaptioner) ModelID() string { return "fake-model" }

// fakeS3 is an in-memory S3Mirror stand-in. Get serves getData for
// getKey (recording hits); anything else errors, modeling a missing
// object. Put counts calls so tests can assert the mirror-read path
// skips the redundant re-put.
type fakeS3 struct {
	getKey  string
	getData []byte
	getCT   string
	getHits int
	puts    int
}

func (s *fakeS3) Put(_ context.Context, _ string, _ string, _ []byte) error {
	s.puts++
	return nil
}
func (s *fakeS3) Get(_ context.Context, key string) ([]byte, string, error) {
	if s.getKey != "" && key == s.getKey {
		s.getHits++
		return s.getData, s.getCT, nil
	}
	return nil, "", errors.New("fakeS3: no such key " + key)
}
func (s *fakeS3) Bucket() string { return "fake-bucket" }

// fakeFetcher returns a fixed (bytes, content-type) pair for any
// sourceImageID, simulating the post-fetch state captionOne sees.
// calls counts fetches so the mirror-fast-path tests can assert the
// source was never contacted.
type fakeFetcher struct {
	data  []byte
	ct    string
	calls int
}

func (f *fakeFetcher) FetchImage(_ context.Context, _ string, _ string) ([]byte, string, error) {
	f.calls++
	return f.data, f.ct, nil
}

// newCaptionOneWorker builds a fully-wired Worker (metrics + logger
// initialized) suitable for calling captionOne directly, bypassing
// Run's polling loop. Tests substitute Fetchers/Captioner per case.
func newCaptionOneWorker(t *testing.T, pool *pgxpool.Pool, captioner Captioner, fetcher SourceFetcher) *Worker {
	t.Helper()
	w := &Worker{
		Pool:      pool,
		S3:        &fakeS3{},
		Captioner: captioner,
		Fetchers:  map[string]SourceFetcher{"slack": fetcher},
		Logger:    slog.Default(),
		Registry:  prometheus.NewRegistry(),
	}
	w.metrics = newWorkerMetrics(w.Registry)
	return w
}

// encodePNG builds a minimal single-color PNG of the given dimensions.
func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// claimedRow marks imageID as claimed right now and returns a claimRow
// carrying that claim, for tests that exercise captionOne/commitCaption
// directly without going through claimUncaptioned. commitCaption (and the
// unsupported-format skip stamp) only finalize a row while its stored
// claimed_at still equals claimRow.claimedAt, so a hand-built row must
// carry the database's real claimed_at or every commit in these tests
// would be rejected as superseded.
func claimedRow(t *testing.T, pool *pgxpool.Pool, imageID, eventID int64, source, sourceImageID, mediaType string) *claimRow {
	t.Helper()
	ctx := context.Background()
	var claimedAt time.Time
	if err := pool.QueryRow(ctx, `
		UPDATE event_images SET claimed_at = now() WHERE id = $1 RETURNING claimed_at
	`, imageID).Scan(&claimedAt); err != nil {
		t.Fatal(err)
	}
	return &claimRow{
		imageID: imageID, eventID: eventID, source: source,
		sourceImageID: sourceImageID, mediaType: mediaType, claimedAt: claimedAt,
	}
}

func seedImageWithMediaType(t *testing.T, pool *pgxpool.Pool, eventID int64, sourceImageID, mediaType string) int64 {
	t.Helper()
	ctx := context.Background()
	var imageID int64
	// occurred_at derives from the parent event, same as the
	// production ReconcileImages path.
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_images (event_id, source_image_id, media_type, occurred_at)
		SELECT $1, $2, $3, e.occurred_at FROM events e WHERE e.id = $1
		RETURNING id
	`, eventID, sourceImageID, mediaType).Scan(&imageID); err != nil {
		t.Fatal(err)
	}
	return imageID
}

func getImageRow(t *testing.T, pool *pgxpool.Pool, imageID int64) (captionModel *string, captionedAtSet bool, chunkID *int64) {
	t.Helper()
	ctx := context.Background()
	var captionedAt *string
	if err := pool.QueryRow(ctx, `
		SELECT caption_model, captioned_at IS NOT NULL, chunk_id
		  FROM event_images WHERE id = $1
	`, imageID).Scan(&captionModel, &captionedAtSet, &chunkID); err != nil {
		t.Fatal(err)
	}
	_ = captionedAt
	return captionModel, captionedAtSet, chunkID
}

// TestCaptionOneSkipsUnsupportedFormat covers Change 1: a fetch that
// resolves to an image/* content type outside Gemini's supported set
// (image/svg+xml) must skip the VLM call entirely, stamp
// captioned_at, and set caption_model='skipped:unsupported-format'
// with chunk_id left NULL.
func TestCaptionOneSkipsUnsupportedFormat(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/unknown")

	captioner := &fakeCaptioner{}
	fetcher := &fakeFetcher{data: []byte("<svg></svg>"), ct: "image/svg+xml"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)

	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/unknown")
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}

	if captioner.calls != 0 {
		t.Errorf("expected 0 VLM calls for unsupported format, got %d", captioner.calls)
	}
	model, captionedAtSet, chunkID := getImageRow(t, pool, imageID)
	if model == nil || *model != "skipped:unsupported-format" {
		t.Errorf("caption_model = %v, want skipped:unsupported-format", model)
	}
	if !captionedAtSet {
		t.Error("captioned_at should be set")
	}
	if chunkID != nil {
		t.Errorf("chunk_id should be NULL, got %v", *chunkID)
	}
}

// TestCaptionOneSkipsResolvedNonImageContentType pins that the
// unsupported-format filter is not scoped to the image/* prefix. A fetch that authoritatively
// resolves to something like text/html (an external URL that 200s
// with an error page instead of real image bytes) is just as
// unsupported as image/svg+xml and must skip rather than reach the
// VLM (which would 400) or burn an S3 write on bytes that aren't
// even an image.
func TestCaptionOneSkipsResolvedNonImageContentType(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/png")

	captioner := &fakeCaptioner{}
	fetcher := &fakeFetcher{data: []byte("<html>error page</html>"), ct: "text/html"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)

	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/png")
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}
	if captioner.calls != 0 {
		t.Errorf("expected 0 VLM calls for a resolved non-image content type, got %d", captioner.calls)
	}
	model, captionedAtSet, _ := getImageRow(t, pool, imageID)
	if model == nil || *model != "skipped:unsupported-format" {
		t.Errorf("caption_model = %v, want skipped:unsupported-format", model)
	}
	if !captionedAtSet {
		t.Error("captioned_at should be set")
	}
}

// TestCaptionOneSupportedFormatReachesCaptioner is the negative
// control for the unsupported-format skip: a resolved content type
// that IS in the supported set must still reach the fake captioner.
func TestCaptionOneSupportedFormatReachesCaptioner(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	// Large enough to clear the too-small filter too, so this test
	// isolates the format check.
	pngBytes := encodePNG(t, 256, 256)
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/png")

	captioner := &fakeCaptioner{}
	fetcher := &fakeFetcher{data: pngBytes, ct: "image/png"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)

	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/png")
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}
	if captioner.calls != 1 {
		t.Errorf("expected 1 VLM call for a supported format, got %d", captioner.calls)
	}
}

// TestCaptionOneSupportedFormatWithParamsAndCaseReachesCaptioner
// covers Content-Type canonicalization: a Content-Type that
// is legal but not byte-identical to the map key (uppercase subtype,
// or a "; charset=..." parameter per RFC 9110) must still resolve
// to "supported" and reach the VLM, not false-positive as
// unsupported.
func TestCaptionOneSupportedFormatWithParamsAndCaseReachesCaptioner(t *testing.T) {
	pngBytes := encodePNG(t, 256, 256)
	cases := []string{"Image/PNG", "image/png; charset=binary", "IMAGE/PNG; charset=binary"}
	for _, ct := range cases {
		t.Run(ct, func(t *testing.T) {
			pool := getWorkerPool(t)
			ctx := context.Background()
			eventID := seedEventForWorker(t, pool, "C1:1.0")
			imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/png")

			captioner := &fakeCaptioner{}
			fetcher := &fakeFetcher{data: pngBytes, ct: ct}
			w := newCaptionOneWorker(t, pool, captioner, fetcher)

			row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/png")
			if err := w.captionOne(ctx, row); err != nil {
				t.Fatalf("captionOne: %v", err)
			}
			if captioner.calls != 1 {
				t.Errorf("content_type %q: expected 1 VLM call, got %d", ct, captioner.calls)
			}
		})
	}
}

// TestCaptionOneJPGAliasReachesCaptioner covers the
// image/jpg alias: "image/jpg" is a common (non-canonical)
// alias for JPEG that extensionFor already treats as equivalent to
// image/jpeg for the S3 key; the unsupported-format allowlist must
// treat it the same way rather than skipping a real JPEG.
func TestCaptionOneJPGAliasReachesCaptioner(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	pngBytes := encodePNG(t, 256, 256) // pixel content is irrelevant; only the ct label is under test
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/jpg")

	captioner := &fakeCaptioner{}
	fetcher := &fakeFetcher{data: pngBytes, ct: "image/jpg"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)

	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/jpg")
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}
	if captioner.calls != 1 {
		t.Errorf("expected 1 VLM call for the image/jpg alias, got %d", captioner.calls)
	}
}

// TestCaptionOneBareImagePrefixReachesCaptioner covers an
// unresolved content type: a bare "image/" content type (no
// subtype at all) must be treated as potentially-supported per the
// documented contract, not skipped.
func TestCaptionOneBareImagePrefixReachesCaptioner(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	pngBytes := encodePNG(t, 256, 256)
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/")

	captioner := &fakeCaptioner{}
	fetcher := &fakeFetcher{data: pngBytes, ct: "image/"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)

	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/")
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}
	if captioner.calls != 1 {
		t.Errorf("expected 1 VLM call for bare image/ prefix, got %d", captioner.calls)
	}
}

// TestCaptionOneCaptionsSmallImage protects direct Ask attachments and other
// semantically meaningful crops. Pixel dimensions are not a relevance signal:
// even a 32x32 upload must reach the captioner and mirror like any other
// supported image.
func TestCaptionOneCaptionsSmallImage(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	tinyPNG := encodePNG(t, 32, 32)
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/png")

	captioner := &fakeCaptioner{}
	fetcher := &fakeFetcher{data: tinyPNG, ct: "image/png"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)

	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/png")
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}

	if captioner.calls != 1 {
		t.Errorf("expected 1 VLM call for a small image, got %d", captioner.calls)
	}
	model, captionedAtSet, chunkID := getImageRow(t, pool, imageID)
	if model == nil || *model != "fake-model" {
		t.Errorf("caption_model = %v, want fake-model", model)
	}
	if !captionedAtSet {
		t.Error("captioned_at should be set")
	}
	if chunkID == nil {
		t.Error("chunk_id should be set")
	}
	s3 := w.S3.(*fakeS3)
	if s3.puts != 1 {
		t.Errorf("S3 Put calls = %d, want 1", s3.puts)
	}
}

// TestCaptionOneSkipMetricNotIncrementedOnDBFailure covers
// ordering: the captionSkipped counter must only
// increment once the DB stamp actually commits, not before. A
// Prometheus counter can't be rolled back, so incrementing it ahead
// of a failing Exec would overcount on every retry of a row whose
// skip-stamp UPDATE failed. Forces the failure by closing the pool
// before calling captionOne.
func TestCaptionOneSkipMetricNotIncrementedOnDBFailure(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/unknown")

	captioner := &fakeCaptioner{}
	fetcher := &fakeFetcher{data: []byte("<svg></svg>"), ct: "image/svg+xml"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)

	// Claim before closing: claimedRow itself needs a live pool.
	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/unknown")

	pool.Close() // force the stamping Exec to fail

	if err := w.captionOne(ctx, row); err == nil {
		t.Fatal("expected an error once the pool is closed")
	}

	got := testutil.ToFloat64(w.metrics.captionSkipped.WithLabelValues("unsupported_format"))
	if got != 0 {
		t.Errorf("captionSkipped{unsupported_format} = %v, want 0 (Exec failed, nothing committed)", got)
	}
}

// TestIsPermanentTerminal covers Change 3's typed-error mechanism:
// a *PermanentError classifies as terminal, the legacy
// "file_not_found" string fallback still works, and an ordinary
// error does not.
func TestIsPermanentTerminal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "typed PermanentError",
			err:  &PermanentError{Underlying: errors.New("notion: relative url")},
			want: true,
		},
		{
			name: "legacy file_not_found string",
			err:  errors.New("slack files.info: file_not_found"),
			want: true,
		},
		{
			name: "file_deleted string",
			err:  errors.New("slack files.info: file_deleted"),
			want: true,
		},
		{
			name: "ordinary error",
			err:  errors.New("some transient-looking hiccup"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermanentTerminal(tc.err); got != tc.want {
				t.Errorf("isPermanentTerminal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestCaptionOneMirrorFastPath: a row with storage_key set reads its
// bytes from the S3 mirror — the source fetcher must never be called
// (decisive for the Slack 12-month retention case, where the source
// file no longer exists) and the redundant re-Put must be skipped.
func TestCaptionOneMirrorFastPath(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/png")

	pngBytes := encodePNG(t, 256, 256)
	const key = "slack/ab/abcdef.png"
	if _, err := pool.Exec(ctx,
		`UPDATE event_images SET storage_key = $2 WHERE id = $1`, imageID, key); err != nil {
		t.Fatal(err)
	}

	captioner := &fakeCaptioner{}
	fetcher := &fakeFetcher{data: []byte("SOURCE BYTES - MUST NOT BE USED"), ct: "image/png"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)
	s3 := &fakeS3{getKey: key, getData: pngBytes, getCT: "image/png"}
	w.S3 = s3

	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/png")
	row.storageKey = key
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}

	if fetcher.calls != 0 {
		t.Errorf("source fetcher called %d times; mirror fast-path should bypass it", fetcher.calls)
	}
	if s3.getHits != 1 {
		t.Errorf("S3 Get hits = %d, want 1", s3.getHits)
	}
	if s3.puts != 0 {
		t.Errorf("S3 Put called %d times; mirror-read path must not re-put", s3.puts)
	}
	if captioner.calls != 1 {
		t.Errorf("captioner calls = %d, want 1", captioner.calls)
	}
	model, captionedAtSet, chunkID := getImageRow(t, pool, imageID)
	if !captionedAtSet || chunkID == nil {
		t.Errorf("expected captioned row with chunk; got captioned=%v chunk=%v", captionedAtSet, chunkID)
	}
	if model == nil || *model != "fake-model" {
		t.Errorf("caption_model = %v, want fake-model", model)
	}
	// storage_key must be preserved, not clobbered.
	var gotKey string
	if err := pool.QueryRow(ctx,
		`SELECT storage_key FROM event_images WHERE id = $1`, imageID).Scan(&gotKey); err != nil {
		t.Fatal(err)
	}
	if gotKey != key {
		t.Errorf("storage_key = %q, want %q preserved", gotKey, key)
	}
}

// TestCaptionOneMirrorMissFallsBackToSource: storage_key set but the
// object is gone (bucket rot) — the worker falls back to the source
// fetcher rather than failing the row.
func TestCaptionOneMirrorMissFallsBackToSource(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/png")
	if _, err := pool.Exec(ctx,
		`UPDATE event_images SET storage_key = 'slack/00/gone.png' WHERE id = $1`, imageID); err != nil {
		t.Fatal(err)
	}

	pngBytes := encodePNG(t, 256, 256)
	captioner := &fakeCaptioner{}
	fetcher := &fakeFetcher{data: pngBytes, ct: "image/png"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)
	// default fakeS3: every Get errors (no getKey configured)

	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/png")
	row.storageKey = "slack/00/gone.png"
	if err := w.captionOne(ctx, row); err != nil {
		t.Fatalf("captionOne: %v", err)
	}
	if fetcher.calls != 1 {
		t.Errorf("source fetcher calls = %d, want 1 (fallback)", fetcher.calls)
	}
	if captioner.calls != 1 {
		t.Errorf("captioner calls = %d, want 1", captioner.calls)
	}
}

// TestCaptionOneParentDeletedMidCaptionCommitsNothing covers the
// claim→commit race: the event is live at claim time and soft-deleted
// while the VLM call is in flight. The delete path has already dropped
// the event's chunks and the embedder skips deleted events, so a
// caption committed now would sit unembedded forever. commitCaption
// must abort without inserting a chunk or stamping the image row.
func TestCaptionOneParentDeletedMidCaptionCommitsNothing(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	eventID := seedEventForWorker(t, pool, "C1:1.0")
	imageID := seedImageWithMediaType(t, pool, eventID, "F1", "image/png")

	captioner := &fakeCaptioner{before: func() {
		if _, err := pool.Exec(ctx,
			`UPDATE events SET deleted = true WHERE id = $1`, eventID); err != nil {
			t.Fatal(err)
		}
	}}
	fetcher := &fakeFetcher{data: encodePNG(t, 256, 256), ct: "image/png"}
	w := newCaptionOneWorker(t, pool, captioner, fetcher)

	row := claimedRow(t, pool, imageID, eventID, "slack", "F1", "image/png")
	err := w.captionOne(ctx, row)
	if err == nil || !strings.Contains(err.Error(), "parent event deleted") {
		t.Fatalf("captionOne err = %v, want parent event deleted", err)
	}
	if captioner.calls != 1 {
		t.Fatalf("expected the VLM call to run before the delete was seen, got %d calls", captioner.calls)
	}

	var chunks int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM event_chunks WHERE event_id = $1 AND kind = 'image_caption'
	`, eventID).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 {
		t.Errorf("committed %d image_caption chunks for a deleted event", chunks)
	}
	_, captionedAtSet, chunkID := getImageRow(t, pool, imageID)
	if captionedAtSet {
		t.Error("captioned_at should stay NULL when the parent was deleted")
	}
	if chunkID != nil {
		t.Errorf("chunk_id should be NULL, got %v", *chunkID)
	}
}
