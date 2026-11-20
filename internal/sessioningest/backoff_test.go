package sessioningest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func identityOf(ch SessionChange) Identity {
	return Identity{Owner: ch.Owner, Client: ch.Client, SessionID: ch.SessionID}
}

func pendingIDs(t *testing.T, s *Store, limit int) []string {
	t.Helper()
	cands, err := s.FindPendingCandidates(context.Background(), limit)
	if errors.Is(err, ErrNoCandidates) {
		return nil
	}
	require.NoError(t, err)
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.SessionID)
	}
	return ids
}

// TestRecordIndexFailure_BacksOffAndUnblocksQueue proves a transiently
// failing revision leaves the oldest-first batch until its backoff expires,
// so the session behind it is selected instead of the same failure again.
func TestRecordIndexFailure_BacksOffAndUnblocksQueue(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	poison := change(1, "a-poison", "o1", "codex")
	next := change(2, "b-next", "o1", "codex")
	_, err := s.UpsertChanges(ctx, []SessionChange{poison, next}, admitAll)
	require.NoError(t, err)
	require.Equal(t, []string{poison.SessionID}, pendingIDs(t, s, 1))

	before := time.Now()
	failure, err := s.RecordIndexFailure(ctx, identityOf(poison), poison.Revision, errors.New("context deadline exceeded"), false)
	require.NoError(t, err)
	require.True(t, failure.Recorded)
	require.False(t, failure.Quarantined)
	require.Equal(t, 1, failure.Count)
	require.WithinDuration(t, before.Add(indexRetryBase), failure.RetryAt, 5*time.Second)

	require.Equal(t, []string{next.SessionID}, pendingIDs(t, s, 1))
	counts, err := s.CountPendingCandidates(ctx)
	require.NoError(t, err)
	require.Equal(t, PendingCounts{Pending: 2, BackingOff: 1}, counts)

	// A second consecutive failure doubles the delay.
	failure, err = s.RecordIndexFailure(ctx, identityOf(poison), poison.Revision, errors.New("empty narrative"), false)
	require.NoError(t, err)
	require.Equal(t, 2, failure.Count)
	require.WithinDuration(t, time.Now().Add(2*indexRetryBase), failure.RetryAt, 5*time.Second)

	var lastErr string
	require.NoError(t, pool.QueryRow(ctx, `SELECT index_last_error FROM session_ingest_candidates
		WHERE session_id=$1`, poison.SessionID).Scan(&lastErr))
	require.Equal(t, "empty narrative", lastErr)

	// Once the backoff expires the revision returns to the front of the batch.
	_, err = pool.Exec(ctx, `UPDATE session_ingest_candidates
		SET index_retry_at = now() - interval '1 second' WHERE session_id=$1`, poison.SessionID)
	require.NoError(t, err)
	require.Equal(t, []string{poison.SessionID}, pendingIDs(t, s, 1))
}

// TestRecordIndexFailure_GivesUpAfterMaxIndexFailures proves no revision is
// retried without end: each failure doubles the wait, and the last one the
// budget allows parks the revision, out of selection and the pending gauge.
func TestRecordIndexFailure_GivesUpAfterMaxIndexFailures(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	ch := change(1, "refused", "o1", "codex")
	_, err := s.UpsertChanges(ctx, []SessionChange{ch}, admitAll)
	require.NoError(t, err)

	wait := indexRetryBase
	for attempt := 1; attempt < MaxIndexFailures; attempt++ {
		failure, err := s.RecordIndexFailure(ctx, identityOf(ch), ch.Revision, errors.New(`reduce: unexpected stop reason: "refusal"`), false)
		require.NoError(t, err)
		require.Equal(t, attempt, failure.Count)
		require.False(t, failure.Quarantined, "attempt %d of %d still has retries left", attempt, MaxIndexFailures)
		require.WithinDuration(t, time.Now().Add(wait), failure.RetryAt, 5*time.Second)
		wait *= 2
	}
	failure, err := s.RecordIndexFailure(ctx, identityOf(ch), ch.Revision, errors.New(`reduce: unexpected stop reason: "refusal"`), false)
	require.NoError(t, err)
	require.Equal(t, MaxIndexFailures, failure.Count)
	require.True(t, failure.Quarantined, "the last attempt the budget allows parks the revision")
	require.True(t, failure.RetryAt.IsZero(), "a parked revision has no next attempt")
	require.Empty(t, pendingIDs(t, s, 10))
	counts, err := s.CountPendingCandidates(ctx)
	require.NoError(t, err)
	require.Equal(t, PendingCounts{Quarantined: 1}, counts)

	// A revision that had already failed far more often than the budget,
	// from before there was one, is parked by its next failure.
	old := change(2, "retried-for-weeks", "o1", "codex")
	_, err = s.UpsertChanges(ctx, []SessionChange{old}, admitAll)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE session_ingest_candidates
		SET index_failure_revision=revision, index_failure_count=5000 WHERE session_id=$1`, old.SessionID)
	require.NoError(t, err)
	failure, err = s.RecordIndexFailure(ctx, identityOf(old), old.Revision, errors.New("boom"), false)
	require.NoError(t, err)
	require.Equal(t, 5001, failure.Count)
	require.True(t, failure.Quarantined)
}

// TestRecordIndexFailure_QuarantineReleasedByNewRevision proves a permanent
// failure parks only the failed revision: it stays out of both selection and
// the pending gauge, and a new head is selectable immediately with a fresh
// failure count.
func TestRecordIndexFailure_QuarantineReleasedByNewRevision(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	ch := change(1, "huge", "o1", "codex")
	_, err := s.UpsertChanges(ctx, []SessionChange{ch}, admitAll)
	require.NoError(t, err)

	failure, err := s.RecordIndexFailure(ctx, identityOf(ch), ch.Revision,
		fmt.Errorf("fetch: %w", ErrRevisionTooLarge), true)
	require.NoError(t, err)
	require.True(t, failure.Quarantined)
	require.True(t, failure.RetryAt.IsZero())
	require.Empty(t, pendingIDs(t, s, 10))
	counts, err := s.CountPendingCandidates(ctx)
	require.NoError(t, err)
	require.Equal(t, PendingCounts{Quarantined: 1}, counts)

	next := ch
	next.ChangeSeq = 2
	next.Revision = "sha256:rev-huge-2"
	next.ContentBytes = 50000
	_, err = s.UpsertChanges(ctx, []SessionChange{next}, admitAll)
	require.NoError(t, err)
	require.Equal(t, []string{ch.SessionID}, pendingIDs(t, s, 10))
	counts, err = s.CountPendingCandidates(ctx)
	require.NoError(t, err)
	require.Equal(t, PendingCounts{Pending: 1}, counts)

	failure, err = s.RecordIndexFailure(ctx, identityOf(next), next.Revision, errors.New("transient"), false)
	require.NoError(t, err)
	require.Equal(t, 1, failure.Count)
	require.False(t, failure.Quarantined)
}

// TestRecordIndexFailure_IgnoresStaleRevision proves failure state is never
// attached to a revision that is no longer the unpublished head.
func TestRecordIndexFailure_IgnoresStaleRevision(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	ch := change(1, "moved", "o1", "codex")
	_, err := s.UpsertChanges(ctx, []SessionChange{ch}, admitAll)
	require.NoError(t, err)

	failure, err := s.RecordIndexFailure(ctx, identityOf(ch), "sha256:older", errors.New("boom"), true)
	require.NoError(t, err)
	require.False(t, failure.Recorded)

	_, err = s.PublishRevision(ctx, identityOf(ch), ch.Revision, ch.RewriteEpoch, "sha256:body")
	require.NoError(t, err)
	failure, err = s.RecordIndexFailure(ctx, identityOf(ch), ch.Revision, errors.New("boom"), true)
	require.NoError(t, err)
	require.False(t, failure.Recorded)
}

func TestBoundedIndexError(t *testing.T) {
	long := strings.Repeat("é", 800) // 1600 bytes
	got := boundedIndexError(errors.New(long))
	require.LessOrEqual(t, len(got), 1000)
	require.True(t, strings.HasPrefix(long, got))
	require.Equal(t, "ab", boundedIndexError(errors.New("a\x00b")))
}

// TestTick_OverCapContentBytesQuarantinesWithoutFetch proves a revision the
// feed already reports as over the byte cap is rejected before any download
// and parked permanently rather than retried every tick.
func TestTick_OverCapContentBytesQuarantinesWithoutFetch(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	revisions := seedCandidateBatch(t, store, 1, "huge") // ContentBytes 40000
	feed := &fakeFeed{revision: revisions}
	w := &Worker{
		Cfg: WorkerConfig{
			TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: true,
			MaxRevisionBytes: 1000,
		},
		Store: store, Feed: feed, Summarize: staticSummarizer{},
		Publish: &recorderPublisher{store: store}, Metrics: NewMetrics(nil),
	}
	require.NoError(t, w.Tick(context.Background()))
	require.Zero(t, feed.fetches.Load(), "over-cap revision was downloaded")
	require.Equal(t, 1.0, testutil.ToFloat64(w.Metrics.IndexErrorsTotal))

	require.NoError(t, w.Tick(context.Background()))
	require.Zero(t, feed.fetches.Load())
	require.Equal(t, 1.0, testutil.ToFloat64(w.Metrics.IndexErrorsTotal), "quarantined revision was retried")
	require.Equal(t, 1.0, testutil.ToFloat64(w.Metrics.QuarantinedSessions))
	require.Equal(t, 0.0, testutil.ToFloat64(w.Metrics.PendingSessions))
}

// countingSummarizer counts the sessions it is asked to summarize.
type countingSummarizer struct{ calls *int }

func (c countingSummarizer) Summarize(ctx context.Context, doc *sessionDocument, id Identity, revision string, epoch int64) (SummaryResult, error) {
	*c.calls++
	return staticSummarizer{}.Summarize(ctx, doc, id, revision, epoch)
}

// TestTick_RevisionWithNoRecordsIsParkedUnsummarized proves a finished
// session with no records is neither sent to the summarizer nor retried: a
// revision never gains records, so each retry would fail the same way.
func TestTick_RevisionWithNoRecordsIsParkedUnsummarized(t *testing.T) {
	store, pool := newWorkerTestStore(t)
	revisions := seedCandidateBatch(t, store, 1, "empty")
	for key := range revisions {
		sessionID, revision, _ := strings.Cut(key, "|")
		revisions[key] = []byte(fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":%q,"revision":%q,"record_count":0,"user_turn_count":0,"content_bytes":0,"complete":true,"owner":"alex@example.com","owner_kind":"human","source":"codex","session_role":"primary","origin_app":"codex","started_at":"2026-07-24T18:00:00Z","updated_at":"2026-07-24T18:42:00Z"}`,
			sessionID, revision) + "\n")
	}
	feed := &fakeFeed{revision: revisions}
	summarized := 0
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: true},
		Store: store, Feed: feed, Summarize: countingSummarizer{calls: &summarized},
		Publish: rec, Metrics: NewMetrics(nil),
	}
	require.NoError(t, w.Tick(context.Background()))
	require.Equal(t, int64(1), feed.fetches.Load())
	require.Zero(t, summarized, "a revision with no records was sent to be summarized")
	require.Empty(t, rec.published)
	require.Equal(t, 1.0, testutil.ToFloat64(w.Metrics.IndexErrorsTotal))
	var recorded string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT index_last_error FROM session_ingest_candidates WHERE index_quarantined_at IS NOT NULL`).Scan(&recorded))
	require.Contains(t, recorded, "session revision has no records", "it is parked for being empty, not for failing to parse")

	require.NoError(t, w.Tick(context.Background()))
	require.Equal(t, int64(1), feed.fetches.Load(), "a parked revision was fetched again")
	require.Equal(t, 1.0, testutil.ToFloat64(w.Metrics.IndexErrorsTotal), "a parked revision was retried")
	require.Equal(t, 1.0, testutil.ToFloat64(w.Metrics.QuarantinedSessions))
	require.Equal(t, 0.0, testutil.ToFloat64(w.Metrics.PendingSessions))
}

// failingSummarizer fails every session in fail and summarizes the rest.
type failingSummarizer struct {
	fail map[string]bool
}

func (f failingSummarizer) Summarize(ctx context.Context, doc *sessionDocument, id Identity, revision string, epoch int64) (SummaryResult, error) {
	if f.fail[id.SessionID] {
		return SummaryResult{}, errors.New("sessioningest: reduce: empty narrative")
	}
	return staticSummarizer{}.Summarize(ctx, doc, id, revision, epoch)
}

// TestTick_FailingBatchBacksOffAndQueueDrains covers a batch filled by the
// oldest sessions, all of which fail every attempt. Without backoff, every
// tick re-selects the same failures and nothing behind them publishes.
func TestTick_FailingBatchBacksOffAndQueueDrains(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	revisions := seedCandidateBatch(t, store, 3, "hol")
	rec := &recorderPublisher{store: store}
	w := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, BatchSize: 2, Policy: DefaultPolicy(), Summarize: true},
		Store: store, Feed: &fakeFeed{revision: revisions},
		Summarize: failingSummarizer{fail: map[string]bool{"hol-001": true, "hol-002": true}},
		Publish:   rec, Metrics: NewMetrics(nil),
	}
	require.NoError(t, w.Tick(context.Background()))
	require.Empty(t, rec.published)
	require.Equal(t, 2.0, testutil.ToFloat64(w.Metrics.IndexErrorsTotal))

	require.NoError(t, w.Tick(context.Background()))
	require.Len(t, rec.published, 1)
	require.Contains(t, rec.published[0], "hol-003")
	require.Equal(t, 2.0, testutil.ToFloat64(w.Metrics.IndexErrorsTotal), "backing-off sessions were retried")
	require.Equal(t, 2.0, testutil.ToFloat64(w.Metrics.BackoffSessions))
	require.Zero(t, testutil.ToFloat64(w.Metrics.QuarantinedSessions), "transient failures must not quarantine")
}

// TestOwnerSessionsSaySummaryFailedOnlyWhileParked proves an owner's session
// list tells a session nothing is trying any more apart from one still
// waiting: failing with retries left is waiting, parked is not, and a new
// revision of a parked session waits again.
func TestOwnerSessionsSaySummaryFailedOnlyWhileParked(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	ch := change(1, "mine", "o1", "codex")
	_, err := s.UpsertChanges(ctx, []SessionChange{ch}, admitAll)
	require.NoError(t, err)
	service, err := NewPrivacyService(s, DefaultPolicy())
	require.NoError(t, err)
	failed := func() bool {
		t.Helper()
		page, err := service.ListSessions(ctx, ch.Owner, 10, 0)
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		return page.Sessions[0].SummaryFailed
	}
	require.False(t, failed(), "a session that has not been tried is waiting")

	_, err = s.RecordIndexFailure(ctx, identityOf(ch), ch.Revision, errors.New("status 503"), false)
	require.NoError(t, err)
	require.False(t, failed(), "a failure with retries left is still waiting")

	_, err = s.RecordIndexFailure(ctx, identityOf(ch), ch.Revision, fmt.Errorf("fetch: %w", ErrRevisionTooLarge), true)
	require.NoError(t, err)
	require.True(t, failed(), "a parked revision is not waiting")

	next := ch
	next.ChangeSeq, next.Revision = 2, "sha256:next"
	_, err = s.UpsertChanges(ctx, []SessionChange{next}, admitAll)
	require.NoError(t, err)
	require.False(t, failed(), "the session changed, so it waits again")
}
