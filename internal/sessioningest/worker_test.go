package sessioningest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/sessiontitle"
)

type fakeFeed struct {
	pages    []*ChangesPage
	revision map[string][]byte
	err      error
	calls    int
	fetches  atomic.Int64
}

func (f *fakeFeed) ListSessionChanges(ctx context.Context, after int64, limit int) (*ChangesPage, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.calls < len(f.pages) {
		p := f.pages[f.calls]
		f.calls++
		// Ensure the page carries a consistent explicit next_cursor so
		// the drain's page-validation passes by default.
		if p.NextCursor == 0 {
			p.NextCursor = after
			for _, c := range p.Changes {
				if c.ChangeSeq > p.NextCursor {
					p.NextCursor = c.ChangeSeq
				}
			}
		}
		return p, nil
	}
	// Terminal empty page echoes the requested cursor (as the feed does).
	return &ChangesPage{NextCursor: after}, nil
}

func (f *fakeFeed) getSessionRevision(sessionID, revision string) ([]byte, error) {
	if b, ok := f.revision[sessionID+"|"+revision]; ok {
		return b, nil
	}
	return nil, errors.New("no revision")
}

func (f *fakeFeed) GetSessionRevisionBytes(_ context.Context, sessionID, revision string, maxRevisionBytes int64) ([]byte, error) {
	f.fetches.Add(1)
	b, err := f.getSessionRevision(sessionID, revision)
	if err != nil {
		return nil, err
	}
	if maxRevisionBytes > 0 && int64(len(b)) > maxRevisionBytes {
		return nil, ErrRevisionTooLarge
	}
	return b, nil
}

type recorderPublisher struct {
	mu         sync.Mutex
	store      *Store
	published  []string
	summaries  []SummaryResult
	tombstones []string
	err        error
}

type staticSummarizer struct{}

func (staticSummarizer) Summarize(_ context.Context, doc *sessionDocument, id Identity, _ string, _ int64) (SummaryResult, error) {
	return SummaryResult{
		Narrative: "summary",
		Chunks:    []SummaryRecord{{Identity: id, RecordStart: 0, RecordEnd: doc.RecordCount, Summary: "chunk"}},
	}, nil
}

// stagedFeed reveals one new change only after the test's initial discovery
// pass has entered its deliberately blocked summarization batch.
type stagedFeed struct {
	change   SessionChange
	reveal   <-chan struct{}
	revision map[string][]byte
	calls    atomic.Int64
}

type cancelingEmptyFeed struct {
	cancel context.CancelFunc
}

func (f cancelingEmptyFeed) ListSessionChanges(_ context.Context, after int64, _ int) (*ChangesPage, error) {
	f.cancel()
	return &ChangesPage{NextCursor: after}, nil
}

func (cancelingEmptyFeed) GetSessionRevisionBytes(context.Context, string, string, int64) ([]byte, error) {
	return nil, errors.New("unexpected revision fetch")
}

func (f *stagedFeed) ListSessionChanges(_ context.Context, after int64, _ int) (*ChangesPage, error) {
	f.calls.Add(1)
	if after >= f.change.ChangeSeq {
		return &ChangesPage{NextCursor: after}, nil
	}
	select {
	case <-f.reveal:
		return &ChangesPage{
			Changes:    []SessionChange{f.change},
			NextCursor: f.change.ChangeSeq,
		}, nil
	default:
		return &ChangesPage{NextCursor: after}, nil
	}
}

func (f *stagedFeed) GetSessionRevisionBytes(_ context.Context, sessionID, revision string, maxRevisionBytes int64) ([]byte, error) {
	b, ok := f.revision[sessionID+"|"+revision]
	if !ok {
		return nil, errors.New("no revision")
	}
	if maxRevisionBytes > 0 && int64(len(b)) > maxRevisionBytes {
		return nil, ErrRevisionTooLarge
	}
	return b, nil
}

type blockingFirstSummarizer struct {
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *blockingFirstSummarizer) Summarize(ctx context.Context, doc *sessionDocument, id Identity, _ string, _ int64) (SummaryResult, error) {
	blocked := false
	s.once.Do(func() {
		blocked = true
		close(s.started)
	})
	if blocked {
		select {
		case <-ctx.Done():
			return SummaryResult{}, ctx.Err()
		case <-s.release:
		}
	}
	return SummaryResult{
		Narrative: "summary",
		Chunks:    []SummaryRecord{{Identity: id, RecordStart: 0, RecordEnd: doc.RecordCount, Summary: "chunk"}},
	}, nil
}

func (r *recorderPublisher) PublishSession(ctx context.Context, doc *sessionDocument, id Identity, _ string, rev string, rewriteEpoch int64, contentDigest, _ string, summary SummaryResult) (PublishResult, error) {
	if r.err != nil {
		return PublishResult{}, r.err
	}
	key := rev
	if key == "" {
		key = doc.Bsky.Revision
	}
	r.mu.Lock()
	r.published = append(r.published, id.SessionID+"|"+key)
	r.summaries = append(r.summaries, summary)
	r.mu.Unlock()
	// Mirror production atomic publish: advance the ledger through the
	// store so the worker's publish() agrees with the corpus. A fake
	// that doesn't advance the ledger would leave the candidate
	// genuinely-pending and re-publish every tick.
	if r.store != nil {
		return r.store.PublishRevision(ctx, id, rev, rewriteEpoch, contentDigest)
	}
	return PublishResult{Published: true}, nil
}

type recordingTitleGenerator struct {
	input string
}

func (g *recordingTitleGenerator) Generate(_ context.Context, input string) (sessiontitle.Result, error) {
	g.input = input
	return sessiontitle.Result{Title: "Compact session title", ModelID: "title-model"}, nil
}
func (r *recorderPublisher) Tombstone(_ context.Context, id Identity) error {
	r.mu.Lock()
	r.tombstones = append(r.tombstones, id.SessionID)
	r.mu.Unlock()
	return nil
}

func (r *recorderPublisher) ReconcileChunkEvents(context.Context, int) (ChunkReconcileResult, error) {
	if r.err != nil {
		return ChunkReconcileResult{}, r.err
	}
	return ChunkReconcileResult{}, nil
}

func newWorkerTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	store, pool := newTestStore(t)
	require.NoError(t, store.SetSessionSummarizationEnabled(context.Background(), true))
	return store, pool
}

func validRevisionBody(sessionID, revision string) []byte {
	return validRevisionBodyFor(Identity{Owner: "alex@example.com", Client: "codex", SessionID: sessionID}, revision)
}

func TestTick_EndToEnd_PublishesAndAdvancesCursor(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := context.Background()
	feed := &fakeFeed{
		pages: []*ChangesPage{
			{Changes: []SessionChange{
				{ChangeSeq: 1, SessionID: "s1",
					Owner: "alex@example.com", OwnerKind: "human",
					Client: "codex", Role: "primary", OriginApp: "codex",
					Revision: "sha256:v1", RewriteEpoch: 1,
					RecordCount: 20, UserTurnCount: 5, ContentBytes: 50000,
					SourceStartedAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
					SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
					UpdatedAt:       time.Now().Add(-30 * time.Minute), Complete: true},
			}},
		},
		revision: map[string][]byte{"s1|sha256:v1": validRevisionBody("s1", "sha256:v1")},
	}
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: true},
		Store: store, Feed: feed, Summarize: staticSummarizer{}, Publish: rec, Metrics: NewMetrics(nil),
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := store.GetCursor(ctx)
	if c.ChangeSeq != 1 {
		t.Fatalf("cursor = %d, want 1", c.ChangeSeq)
	}
	id := Identity{Owner: "alex@example.com", Client: "codex", SessionID: "s1"}
	ok, _ := store.LoadPublishedRevision(ctx, id, "sha256:v1")
	if !ok {
		t.Fatal("revision should be published in the ledger")
	}
	if len(rec.published) != 1 || rec.published[0] != "s1|sha256:v1" {
		t.Fatalf("corpus publish = %+v", rec.published)
	}
}

func TestTick_GeneratesTitleFromFinalSummary(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := context.Background()
	feed := &fakeFeed{
		pages: []*ChangesPage{{Changes: []SessionChange{{
			ChangeSeq: 1, SessionID: "titled", Owner: "alex@example.com",
			OwnerKind: "human", Client: "codex", Role: "primary",
			Revision: "sha256:titled", RecordCount: 1, UserTurnCount: 1,
			SourceStartedAt: time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC),
			SourceUpdatedAt: time.Date(2026, 8, 31, 10, 1, 0, 0, time.UTC),
			UpdatedAt:       time.Now().Add(-time.Hour), Complete: true,
		}}}},
		revision: map[string][]byte{"titled|sha256:titled": validRevisionBody("titled", "sha256:titled")},
	}
	titles := &recordingTitleGenerator{}
	recorder := &recorderPublisher{store: store}
	worker := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: true},
		Store: store, Feed: feed, Summarize: staticSummarizer{},
		TitleGenerator: titles, Publish: recorder, Metrics: NewMetrics(nil),
	}
	require.NoError(t, worker.Tick(ctx))
	require.Equal(t, "summary", titles.input)
	require.Len(t, recorder.summaries, 1)
	require.Equal(t, "Compact session title", recorder.summaries[0].Title)
	require.Equal(t, "title-model", recorder.summaries[0].TitleModel)
}

func TestTick_DiscoversFeedChangesWhileLargeSummaryBatchIsBlocked(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	policy := DefaultPolicy()

	// Fill the default 100-session schedule batch. The first model call will
	// block, representing the production failure mode where the outer Tick
	// otherwise cannot return to its next discovery pass for hours.
	const backlog = 100
	changes := make([]SessionChange, 0, backlog)
	revisions := make(map[string][]byte, backlog+1)
	for i := 1; i <= backlog; i++ {
		sessionID := fmt.Sprintf("backlog-%03d", i)
		ch := change(int64(i), sessionID, "alex@example.com", "codex")
		changes = append(changes, ch)
		revisions[sessionID+"|"+ch.Revision] = validRevisionBody(sessionID, ch.Revision)
	}
	if _, err := store.UpsertChangesAt(context.Background(), changes, backlog, admitAll); err != nil {
		t.Fatal(err)
	}

	newChange := change(backlog+1, "arrived-during-summary", "alex@example.com", "codex")
	revisions[newChange.SessionID+"|"+newChange.Revision] = validRevisionBody(newChange.SessionID, newChange.Revision)
	reveal := make(chan struct{})
	feed := &stagedFeed{change: newChange, reveal: reveal, revision: revisions}
	started := make(chan struct{})
	release := make(chan struct{})
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg: WorkerConfig{
			TickInterval: 10 * time.Millisecond,
			BatchSize:    backlog,
			Policy:       policy,
			Summarize:    true,
		},
		Store: store, Feed: feed,
		Summarize: &blockingFirstSummarizer{started: started, release: release},
		Publish:   rec,
		Metrics:   NewMetrics(nil),
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tickDone := make(chan error, 1)
	go func() { tickDone <- w.Tick(ctx) }()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("summarization did not start")
	}
	if got := testutil.ToFloat64(w.Metrics.LastDiscoverySuccess); got <= 1 {
		t.Fatalf("discovery heartbeat = %v, want initial pass recorded before summarization", got)
	}
	// Prove a later discovery pass refreshes the heartbeat while the first
	// model call remains blocked. A sentinel avoids relying on wall-clock
	// second boundaries in this millisecond-scale test.
	w.Metrics.LastDiscoverySuccess.Set(1)
	close(reveal)

	deadline := time.Now().Add(time.Second)
	for {
		cursor, err := store.GetCursor(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if cursor.ChangeSeq == newChange.ChangeSeq {
			if heartbeat := testutil.ToFloat64(w.Metrics.LastDiscoverySuccess); heartbeat > 1 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("cursor remained at %d while summarization was blocked; feed calls=%d", cursor.ChangeSeq, feed.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Discovery must have happened before either the blocked model call or
	// the 100-item schedule batch was allowed to finish.
	select {
	case err := <-tickDone:
		t.Fatalf("Tick returned before blocked summary was released: %v", err)
	default:
	}

	cancel()
	select {
	case err := <-tickDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Tick error after cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Tick did not stop after context cancellation")
	}
}

func TestTick_FailedDiscoveryDoesNotAdvanceHeartbeat(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	metrics := NewMetrics(nil)
	metrics.LastDiscoverySuccess.Set(123)
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy()},
		Store: store, Feed: &fakeFeed{err: errors.New("feed unavailable")}, Metrics: metrics,
	}

	if err := w.Tick(context.Background()); err == nil {
		t.Fatal("Tick succeeded with a failed discovery pass")
	}
	if got := testutil.ToFloat64(metrics.LastDiscoverySuccess); got != 123 {
		t.Fatalf("discovery heartbeat = %v, want unchanged sentinel 123", got)
	}
}

func TestTick_DownstreamFailureRetainsSuccessfulDiscoveryHeartbeat(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	metrics := NewMetrics(nil)
	metrics.LastDiscoverySuccess.Set(1)
	rec := &recorderPublisher{store: store, err: errors.New("publisher unavailable")}
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: true},
		Store: store, Feed: &fakeFeed{}, Summarize: staticSummarizer{}, Publish: rec, Metrics: metrics,
	}

	if err := w.Tick(context.Background()); err == nil {
		t.Fatal("Tick succeeded with a downstream reconciliation failure")
	}
	if got := testutil.ToFloat64(metrics.LastDiscoverySuccess); got <= 1 {
		t.Fatalf("discovery heartbeat = %v, want successful feed refresh recorded despite downstream failure", got)
	}
}

func TestTick_PostDrainFailureRetainsSuccessfulDiscoveryHeartbeat(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	metrics := NewMetrics(nil)
	metrics.LastDiscoverySuccess.Set(1)
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy()},
		Store: store, Feed: cancelingEmptyFeed{cancel: cancel}, Metrics: metrics,
	}

	if err := w.Tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Tick error = %v, want context.Canceled after the feed drain", err)
	}
	if got := testutil.ToFloat64(metrics.LastDiscoverySuccess); got <= 1 {
		t.Fatalf("discovery heartbeat = %v, want successful feed drain recorded before later work", got)
	}
}

func TestTick_SummarizeDisabled_DoesNotPublishWatermark(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := context.Background()
	feed := &fakeFeed{
		pages: []*ChangesPage{
			{Changes: []SessionChange{
				{ChangeSeq: 1, SessionID: "s1",
					Owner: "alex@example.com", OwnerKind: "human",
					Client: "codex", Role: "primary", OriginApp: "codex",
					Revision: "sha256:v1", RewriteEpoch: 1,
					RecordCount: 20, Complete: true,
					UpdatedAt: time.Now().Add(-30 * time.Minute)},
			}},
		},
	}
	rec := &recorderPublisher{store: store}
	w := &Worker{
		// Summarize=false: shadow observe-only.
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: false},
		Store: store, Feed: feed, Summarize: staticSummarizer{}, Publish: rec, Metrics: NewMetrics(nil),
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	id := Identity{Owner: "alex@example.com", Client: "codex", SessionID: "s1"}
	c, _ := store.LoadCandidate(ctx, id)
	if c.PublishedRevision != "" {
		t.Fatalf("published watermark advanced with Summarize off: %q", c.PublishedRevision)
	}
	if len(rec.published) != 0 {
		t.Fatalf("corpus published with Summarize off: %+v", rec.published)
	}
}

func TestTick_RuntimePauseKeepsDiscoveryAndResumePublishesPending(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	feed := &fakeFeed{pages: []*ChangesPage{{Changes: []SessionChange{change(1, "runtime-pause", "alex@example.com", "codex")}}}, revision: map[string][]byte{"runtime-pause|sha256:revruntime-pause": validRevisionBody("runtime-pause", "sha256:revruntime-pause")}}
	recorder := &recorderPublisher{store: store}
	worker := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: true},
		Store: store, Feed: feed, Summarize: staticSummarizer{}, Publish: recorder,
		Control: NewSummaryControl(store, nil), Metrics: NewMetrics(nil),
	}
	require.NoError(t, worker.Tick(ctx))
	cursor, err := store.GetCursor(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), cursor.ChangeSeq)
	require.Empty(t, recorder.published)

	require.NoError(t, store.SetSessionSummarizationEnabled(ctx, true))
	require.NoError(t, worker.Tick(ctx))
	require.Equal(t, []string{"runtime-pause|sha256:revruntime-pause"}, recorder.published)
}

func TestTick_RuntimePauseSkipsQueuedCandidateBeforeItStarts(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := context.Background()
	policy := DefaultPolicy()
	first := change(1, "first", "alex@example.com", "codex")
	second := change(2, "second", "alex@example.com", "codex")
	_, err := store.UpsertChangesAt(ctx, []SessionChange{first, second}, 2, admitAll)
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	recorder := &recorderPublisher{store: store}
	worker := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, BatchSize: 2, Policy: policy, Summarize: true},
		Store: store,
		Feed: &fakeFeed{revision: map[string][]byte{
			"first|" + first.Revision:   validRevisionBody("first", first.Revision),
			"second|" + second.Revision: validRevisionBody("second", second.Revision),
		}},
		Summarize: &blockingFirstSummarizer{started: started, release: release}, Publish: recorder,
		Control: NewSummaryControl(store, nil), Metrics: NewMetrics(nil),
	}
	done := make(chan error, 1)
	go func() { done <- worker.Tick(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first summary did not start")
	}
	require.NoError(t, store.SetSessionSummarizationEnabled(ctx, false))
	close(release)
	require.NoError(t, <-done)
	require.Equal(t, []string{"first|" + first.Revision}, recorder.published)
}

func TestTick_RejectsIncompleteSession(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := context.Background()
	feed := &fakeFeed{
		pages: []*ChangesPage{
			{Changes: []SessionChange{
				{ChangeSeq: 1, SessionID: "s1",
					OwnerKind: "human", Client: "codex", Role: "primary", OriginApp: "codex",
					Revision: "sha256:v1", RewriteEpoch: 1, Complete: true,
					UpdatedAt: time.Now().Add(-time.Hour)},
			}},
		},
		revision: map[string][]byte{"s1|sha256:v1": []byte(`{"type":"session","schema":"bsky.session.v1","session_id":"s1","revision":"sha256:v1","record_count":1,"complete":false}
{"index":0,"id":"rec0","role":"user","content":"x"}
`)},
	}
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: true},
		Store: store, Feed: feed, Summarize: staticSummarizer{}, Publish: rec, Metrics: NewMetrics(nil),
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	id := Identity{Owner: "", Client: "codex", SessionID: "s1"}
	// Incomplete session must NOT be published.
	c, _ := store.LoadCandidate(ctx, id)
	if c.PublishedRevision != "" {
		t.Fatalf("incomplete session was published: %q", c.PublishedRevision)
	}
	if len(rec.published) != 0 {
		t.Fatalf("incomplete session rendered to corpus: %+v", rec.published)
	}
}

func TestTick_TombstonesDeletedHeadCorpus(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := context.Background()
	// First index a revision; it becomes the active published head.
	_, _ = store.UpsertChanges(ctx, []SessionChange{{
		ChangeSeq: 1, SessionID: "s1", Owner: "o1", Client: "codex",
		OwnerKind: "human", Role: "primary", OriginApp: "codex",
		Revision: "sha256:v1", RewriteEpoch: 1, RecordCount: 10,
		SourceStartedAt: time.Now().Add(-2 * time.Hour),
		SourceUpdatedAt: time.Now().Add(-time.Hour),
		Complete:        true, UpdatedAt: time.Now().Add(-time.Hour),
	}}, admitAll)
	id := Identity{Owner: "o1", Client: "codex", SessionID: "s1"}
	_, _ = store.PublishRevision(ctx, id, "sha256:v1", 1, "d1")

	// Soft-delete head arrives.
	feed := &fakeFeed{
		pages: []*ChangesPage{
			{Changes: []SessionChange{{
				ChangeSeq: 2, SessionID: "s1", Owner: "o1", Client: "codex",
				OwnerKind: "human", Role: "primary", OriginApp: "codex",
				Revision: "", Deleted: true, UpdatedAt: time.Now(),
			}}},
		},
	}
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy()},
		Store: store, Feed: feed, Publish: rec, Metrics: NewMetrics(nil),
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := store.LoadCandidate(ctx, id)
	if c.ExcludedAt == nil {
		t.Error("deleted head should be excluded/tombstoned")
	}
	if len(rec.tombstones) != 1 || rec.tombstones[0] != "s1" {
		t.Fatalf("expected corpus tombstone for deleted head, got %+v", rec.tombstones)
	}
}

func TestTick_AcceptsAndPersistsExplicitCursorGreaterThanServed(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := context.Background()
	// The feed's explicit next_cursor (11) is greater than the largest
	// served change_seq (5). The page validator must accept it (it only
	// guards regressions and cursors that trail the served rows), and the
	// durable cursor must advance to exactly 11 — never re-derived from
	// the local max, which would double-process the leading rows.
	feed := &fakeFeed{
		pages: []*ChangesPage{
			{NextCursor: 11, Changes: []SessionChange{
				{ChangeSeq: 5, SessionID: "s1",
					Owner: "alex@example.com", OwnerKind: "human",
					Client: "codex", Role: "primary", OriginApp: "codex",
					Revision: "sha256:v1", RewriteEpoch: 1, Complete: true,
					UpdatedAt: time.Now().Add(-time.Hour)},
			}},
		},
	}
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: false},
		Store: store, Feed: feed, Publish: rec, Metrics: NewMetrics(nil),
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := store.GetCursor(ctx)
	if c.ChangeSeq != 11 {
		t.Fatalf("cursor = %d, want 11 (explicit next_cursor, not re-derived)", c.ChangeSeq)
	}
}

func TestTick_RejectsInconsistentPageCursor(t *testing.T) {
	// An inconsistent page: explicit next_cursor behind its own max served
	// change_seq. The tick-level validator must reject it so the durable
	// cursor can never be advanced past a page whose cursor trails its rows.
	if err := validateFeedPage(&ChangesPage{
		NextCursor: 5,
		Changes:    []SessionChange{{ChangeSeq: 7, SessionID: "s"}},
	}, 0); err == nil {
		t.Fatal("page next_cursor trailing its own served rows must be rejected")
	}
}

// concurrencyGateSummarizer blocks every Summarize call until release is
// closed and reports the peak number of calls in flight. It proves the
// schedule worker pool never exceeds its configured bound: while the gate is
// shut, no in-flight call can finish, so a correct pool of N workers can
// never admit a (N+1)-th session.
type concurrencyGateSummarizer struct {
	want     int64
	active   atomic.Int64
	max      atomic.Int64
	full     chan struct{}
	release  chan struct{}
	fullOnce sync.Once
}

func (s *concurrencyGateSummarizer) Summarize(ctx context.Context, doc *sessionDocument, id Identity, _ string, _ int64) (SummaryResult, error) {
	cur := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		prev := s.max.Load()
		if cur <= prev || s.max.CompareAndSwap(prev, cur) {
			break
		}
	}
	if cur == s.want {
		s.fullOnce.Do(func() { close(s.full) })
	}
	select {
	case <-ctx.Done():
		return SummaryResult{}, ctx.Err()
	case <-s.release:
	}
	return SummaryResult{
		Narrative: "summary",
		Chunks:    []SummaryRecord{{Identity: id, RecordStart: 0, RecordEnd: doc.RecordCount, Summary: "chunk"}},
	}, nil
}

// seedCandidateBatch loads n distinct, immediately-eligible candidate
// sessions into the store and returns their revision bodies.
func seedCandidateBatch(t *testing.T, store *Store, n int, prefix string) map[string][]byte {
	t.Helper()
	changes := make([]SessionChange, 0, n)
	revisions := make(map[string][]byte, n)
	for i := 1; i <= n; i++ {
		sessionID := fmt.Sprintf("%s-%03d", prefix, i)
		ch := change(int64(i), sessionID, "alex@example.com", "codex")
		changes = append(changes, ch)
		revisions[sessionID+"|"+ch.Revision] = validRevisionBody(sessionID, ch.Revision)
	}
	if _, err := store.UpsertChangesAt(context.Background(), changes, int64(n), admitAll); err != nil {
		t.Fatal(err)
	}
	return revisions
}

func TestDiscoveryPass_ReportsCompleteBacklogNotBatchSize(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	const backlog = 7
	seedCandidateBatch(t, store, backlog, "metric")
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, BatchSize: 2, Policy: DefaultPolicy()},
		Store: store, Feed: &fakeFeed{}, Metrics: NewMetrics(nil),
	}
	if err := w.discoveryPass(context.Background(), w.Cfg.Policy.Hash()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(w.Metrics.PendingSessions); got != backlog {
		t.Fatalf("pending gauge = %.0f, want complete backlog %d (not batch size %d)", got, backlog, w.Cfg.BatchSize)
	}
}

// assertPublishedExactlyOnce asserts every candidate in the batch was
// published exactly once as a distinct session.
func assertPublishedExactlyOnce(t *testing.T, rec *recorderPublisher, n int) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.published) != n {
		t.Fatalf("published %d revisions, want %d: %+v", len(rec.published), n, rec.published)
	}
	seen := make(map[string]int, n)
	for _, p := range rec.published {
		seen[strings.SplitN(p, "|", 2)[0]]++
	}
	for session, count := range seen {
		if count != 1 {
			t.Fatalf("session %s published %d times, want exactly 1", session, count)
		}
	}
}

// TestTick_ScheduleConcurrency_LimitsInFlight proves the worker pool never
// has more than Concurrency summarizations in flight, and that every
// candidate in the batch is still published exactly once.
func TestTick_ScheduleConcurrency_LimitsInFlight(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := context.Background()
	const (
		n           = 8
		concurrency = 3
	)
	revisions := seedCandidateBatch(t, store, n, "conc")
	release := make(chan struct{})
	gate := &concurrencyGateSummarizer{
		want:    concurrency,
		full:    make(chan struct{}),
		release: release,
	}
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg: WorkerConfig{
			TickInterval: time.Minute,
			BatchSize:    n,
			Concurrency:  concurrency,
			Policy:       DefaultPolicy(),
			Summarize:    true,
		},
		Store: store, Feed: &fakeFeed{revision: revisions},
		Summarize: gate, Publish: rec, Metrics: NewMetrics(nil),
	}

	tickDone := make(chan error, 1)
	go func() { tickDone <- w.Tick(ctx) }()

	select {
	case <-gate.full:
	case <-time.After(3 * time.Second):
		t.Fatal("expected 3 summaries in flight")
	}
	// Give a broken implementation every chance to start a 4th session.
	time.Sleep(100 * time.Millisecond)
	if got := gate.max.Load(); got != concurrency {
		t.Fatalf("peak in-flight summaries = %d, want %d (bounded pool)", got, concurrency)
	}
	if got := gate.active.Load(); got != concurrency {
		t.Fatalf("in-flight summaries = %d, want %d while gate is shut", got, concurrency)
	}
	rec.mu.Lock()
	publishedWhileBlocked := len(rec.published)
	rec.mu.Unlock()
	if publishedWhileBlocked != 0 {
		t.Fatalf("published while all workers blocked: %+v", rec.published)
	}

	close(release)
	select {
	case err := <-tickDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Tick did not finish after gate release")
	}
	if got := gate.max.Load(); got != concurrency {
		t.Fatalf("peak in-flight summaries = %d, want %d", got, concurrency)
	}
	assertPublishedExactlyOnce(t, rec, n)
}

// TestTick_ScheduleConcurrency_DefaultIsSerial proves an unset
// Concurrency (0) keeps the schedule pass serial: at most one session is
// summarized at a time, and the whole batch still drains.
func TestTick_ScheduleConcurrency_DefaultIsSerial(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := context.Background()
	const n = 6
	revisions := seedCandidateBatch(t, store, n, "serial")
	release := make(chan struct{})
	gate := &concurrencyGateSummarizer{
		want:    1,
		full:    make(chan struct{}),
		release: release,
	}
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg: WorkerConfig{
			TickInterval: time.Minute,
			BatchSize:    n,
			Policy:       DefaultPolicy(),
			Summarize:    true,
		},
		Store: store, Feed: &fakeFeed{revision: revisions},
		Summarize: gate, Publish: rec, Metrics: NewMetrics(nil),
	}

	tickDone := make(chan error, 1)
	go func() { tickDone <- w.Tick(ctx) }()

	select {
	case <-gate.full:
	case <-time.After(3 * time.Second):
		t.Fatal("expected 1 summary in flight")
	}
	time.Sleep(100 * time.Millisecond)
	if got := gate.max.Load(); got != 1 {
		t.Fatalf("peak in-flight summaries = %d, want 1 (default concurrency)", got)
	}

	close(release)
	select {
	case err := <-tickDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Tick did not finish after gate release")
	}
	assertPublishedExactlyOnce(t, rec, n)
}

type deadlineCheckingSummarizer struct {
	test *testing.T
}

func (summarizer deadlineCheckingSummarizer) Summarize(ctx context.Context, doc *sessionDocument, id Identity, revision string, epoch int64) (SummaryResult, error) {
	if deadline, ok := ctx.Deadline(); ok {
		summarizer.test.Errorf("worker imposed a whole-session deadline: %s", deadline)
	}
	return (staticSummarizer{}).Summarize(ctx, doc, id, revision, epoch)
}

func TestTick_ScheduleConcurrency_NoSessionDeadline(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	const n = 3
	revisions := seedCandidateBatch(t, store, n, "stall")
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg: WorkerConfig{
			TickInterval: time.Minute,
			BatchSize:    n,
			Concurrency:  n,
			Policy:       DefaultPolicy(),
			Summarize:    true,
		},
		Store: store, Feed: &fakeFeed{revision: revisions},
		Summarize: deadlineCheckingSummarizer{test: t}, Publish: rec, Metrics: NewMetrics(nil),
	}

	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(w.Metrics.IndexErrorsTotal); got != 0 {
		t.Fatalf("index errors = %v, want 0", got)
	}
	assertPublishedExactlyOnce(t, rec, n)
}

// TestTick_ScheduleConcurrency_CancelIsNotCountedAsIndexError proves a
// shutdown cancellation aborts the batch without counting interrupted
// sessions as index errors; the still-pending revisions re-select on the
// next tick.
func TestTick_ScheduleConcurrency_CancelIsNotCountedAsIndexError(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	const (
		n           = 4
		concurrency = 2
	)
	revisions := seedCandidateBatch(t, store, n, "cancel")
	gate := &concurrencyGateSummarizer{
		want:    concurrency,
		full:    make(chan struct{}),
		release: make(chan struct{}),
	}
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg: WorkerConfig{
			TickInterval: time.Minute,
			BatchSize:    n,
			Concurrency:  concurrency,
			Policy:       DefaultPolicy(),
			Summarize:    true,
		},
		Store: store, Feed: &fakeFeed{revision: revisions},
		Summarize: gate, Publish: rec, Metrics: NewMetrics(nil),
	}

	ctx, cancel := context.WithCancel(context.Background())
	tickDone := make(chan error, 1)
	go func() { tickDone <- w.Tick(ctx) }()

	select {
	case <-gate.full:
	case <-time.After(3 * time.Second):
		t.Fatal("expected summaries in flight")
	}
	cancel()

	select {
	case err := <-tickDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Tick error after cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Tick did not stop after context cancellation")
	}
	if got := testutil.ToFloat64(w.Metrics.IndexErrorsTotal); got != 0 {
		t.Fatalf("shutdown cancellation counted %v index errors, want 0", got)
	}
	if len(rec.published) != 0 {
		t.Fatalf("sessions published after cancellation: %+v", rec.published)
	}
}

// TestDrain_SkippedChangeLogsReasonAndIncrementsMetricThenContinues pins
// Worker.drain's skip branch: the Warn log, the FeedChangesSkippedTotal
// increment, and drain continuing to later pages after a skip.
//
// The first of two pages holds a session already push-bound (writer
// conflict) alongside an unrelated session; the second page holds another,
// independent session. Expected: the warn log line names the skipped
// session and reason; the metric counts it under reason="writer_conflict";
// and drain still reaches and commits the second page.
func TestDrain_SkippedChangeLogsReasonAndIncrementsMetricThenContinues(t *testing.T) {
	store, pool := newWorkerTestStore(t)
	ctx := context.Background()

	const (
		collidingID = "ses-drain-collide-1"
		okID        = "ses-drain-ok-1"
		laterID     = "ses-drain-later-1"
		owner       = "owner-drain@example.com"
		client      = "codex"
	)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = bindSessionTx(ctx, tx, collidingID, owner, WriterPush, "claude-code")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	var logBuf bytes.Buffer
	metrics := NewMetrics(nil)
	w := &Worker{
		Cfg:     WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy()},
		Store:   store,
		Logger:  slog.New(slog.NewTextHandler(&logBuf, nil)),
		Metrics: metrics,
		Feed: &fakeFeed{pages: []*ChangesPage{
			{Changes: []SessionChange{
				change(1, okID, owner, client),
				change(2, collidingID, owner, client),
			}},
			{Changes: []SessionChange{
				change(3, laterID, owner, client),
			}},
		}},
	}

	after, err := w.drain(ctx, 0)
	if err != nil {
		t.Fatalf("drain failed: %v", err)
	}
	if after != 3 {
		t.Fatalf("drain reached %d, want 3 (both pages applied)", after)
	}

	// The skip must be logged with the session id and its reason.
	logged := logBuf.String()
	if !strings.Contains(logged, "session="+collidingID) || !strings.Contains(logged, "reason=writer_conflict") {
		t.Fatalf("skip log = %q, want it to name session=%s and reason=writer_conflict", logged, collidingID)
	}

	// The metric must count exactly one writer_conflict skip.
	if got := testutil.ToFloat64(metrics.FeedChangesSkippedTotal.WithLabelValues("writer_conflict")); got != 1 {
		t.Fatalf("FeedChangesSkippedTotal{reason=writer_conflict} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.FeedChangesSkippedTotal.WithLabelValues("malformed_row")); got != 0 {
		t.Fatalf("FeedChangesSkippedTotal{reason=malformed_row} = %v, want 0", got)
	}

	// okID (same page as the skip) and laterID (the following page) must
	// both have landed: the skip must not have stalled the rest of the
	// page or the pages after it.
	if _, err := store.LoadCandidate(ctx, Identity{Owner: owner, Client: client, SessionID: okID}); err != nil {
		t.Fatalf("okID's change was lost: %v", err)
	}
	if _, err := store.LoadCandidate(ctx, Identity{Owner: owner, Client: client, SessionID: laterID}); err != nil {
		t.Fatalf("laterID's change (later page) was lost: %v", err)
	}
}
