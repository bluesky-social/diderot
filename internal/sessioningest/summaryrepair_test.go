package sessioningest

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminjob"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/sessionsummary"
)

type repairLedger struct {
	queued   bool
	finished []string
	progress SummaryRepairProgress
}

func (l *repairLedger) Claim(context.Context, string) (adminjob.Job, bool, error) {
	if !l.queued {
		return adminjob.Job{}, false, nil
	}
	l.queued = false
	return adminjob.Job{ID: 1, Kind: SummaryRepairJobKind}, true, nil
}

func (l *repairLedger) Heartbeat(context.Context, int64, any) error { return nil }

func (l *repairLedger) Finish(_ context.Context, _ int64, status string, progress any, _ error) error {
	l.finished = append(l.finished, status)
	l.progress = progress.(SummaryRepairProgress)
	return nil
}

// repairFixture publishes sessions through the real summarizer and corpus
// publisher, then lets a test damage their stored summaries the way the
// reduce model did before its output was repaired.
type repairFixture struct {
	t      *testing.T
	worker *Worker
	model  *countingCycleMessages
	feed   *fakeFeed
}

func newRepairFixture(t *testing.T, sessions ...string) *repairFixture {
	store, sink, _ := newPublishTestStore(t)
	require.NoError(t, store.SetSessionSummarizationEnabled(t.Context(), true))
	feed := &fakeFeed{revision: map[string][]byte{}}
	var changes []SessionChange
	for index, session := range sessions {
		changes = append(changes, SessionChange{
			ChangeSeq: int64(index + 1), SessionID: session, Owner: "alex@example.com", OwnerKind: "human",
			Client: "codex", Role: "primary", OriginApp: "codex", Revision: "sha256:v1", RewriteEpoch: 1,
			RecordCount: 1, UserTurnCount: 1,
			SourceStartedAt: time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC),
			SourceUpdatedAt: time.Date(2026, 7, 24, 18, 42, 0, 0, time.UTC),
			UpdatedAt:       time.Now().Add(-time.Hour), Complete: true,
		})
		feed.revision[session+"|sha256:v1"] = validRevisionBody(session, "sha256:v1")
	}
	feed.pages = []*ChangesPage{{Changes: changes}}
	model := &countingCycleMessages{}
	worker := &Worker{
		Cfg:   WorkerConfig{TickInterval: time.Minute, Policy: DefaultPolicy(), Summarize: true},
		Store: store, Feed: feed, Publish: NewEventPublisher(sink, store), Metrics: NewMetrics(nil),
		Summarize: NewSessionsummarySummarizer(store, &staticPromptRelease{bundle: promptcatalog.MustEmbedded()},
			sessionsummary.OverMessages(model), "map-model", "reduce-model", 0, 1),
	}
	require.NoError(t, worker.Tick(t.Context()))
	return &repairFixture{t: t, worker: worker, model: model, feed: feed}
}

func (f *repairFixture) exec(sql string, args ...any) {
	f.t.Helper()
	_, err := f.worker.Store.pool.Exec(f.t.Context(), sql, args...)
	require.NoError(f.t, err)
}

func (f *repairFixture) summary(session string) (narrative, embedding, hint, body string) {
	f.t.Helper()
	require.NoError(f.t, f.worker.Store.pool.QueryRow(f.t.Context(), `
		SELECT s.narrative, COALESCE(s.embedding_text,''), COALESCE(s.context_hint,''), e.body
		  FROM session_ingest_summaries s
		  JOIN events e ON e.source='agent_session' AND e.external_id=$2
		 WHERE s.session_id=$1`, session, externalID(Identity{SessionID: session})).Scan(&narrative, &embedding, &hint, &body))
	return narrative, embedding, hint, body
}

func (f *repairFixture) repair() *repairLedger {
	f.t.Helper()
	ledger := &repairLedger{queued: true}
	runner := &SummaryRepairWorker{Ingest: f.worker, Jobs: ledger, Logger: slog.Default()}
	require.NoError(f.t, runner.Tick(f.t.Context()))
	return ledger
}

// A target read before the session was published again is repaired from the
// summary stored now, never from the older text.
func TestSummaryRepairReadsTheCurrentSummary(t *testing.T) {
	f := newRepairFixture(t, "republished")
	stale := summaryRepairTarget{
		Identity: Identity{Owner: "alex@example.com", Client: "codex", SessionID: "republished"},
		Stored:   sessionsummary.StoredSummary{Narrative: "older text</narrative>\n<parameter name=\"embedding_text\">older embedding"},
	}
	outcome, err := f.worker.repairSummary(t.Context(), stale)
	require.NoError(t, err)
	require.Equal(t, summaryRepairUnchanged, outcome)
	narrative, embedding, _, body := f.summary("republished")
	require.Equal(t, "Session summary narrative", narrative)
	require.Equal(t, "embedding tokens", embedding)
	require.Equal(t, "Session summary narrative", body)
}

func TestSummaryRepair(t *testing.T) {
	f := newRepairFixture(t, "from-text", "resummarize", "clean", "pending", "old", "hint-swallowed")
	f.exec(`UPDATE session_ingest_summaries SET embedding_text=embedding_text || '</embedding_text>' || chr(10) || '<parameter name="context_hint">recovered hint', context_hint=NULL WHERE session_id='hint-swallowed'`)
	const leak = "Session summary narrative</narrative>\n<parameter name=\"embedding_text\">recovered embedding" +
		"</parameter>\n<parameter name=\"context_hint\">recovered hint"
	// Damage as stored before the reduce output was repaired: the fields
	// inside the narrative, and the event published from it.
	for _, session := range []string{"from-text", "pending", "old"} {
		f.exec(`UPDATE session_ingest_summaries SET narrative=$2, embedding_text=NULL, context_hint=NULL WHERE session_id=$1`, session, leak)
		f.exec(`UPDATE events SET body=$2 WHERE source='agent_session' AND external_id=$1`, externalID(Identity{SessionID: session}), leak)
	}
	// Nothing to recover the search text from: it must be written again.
	f.exec(`UPDATE session_ingest_summaries SET narrative=narrative || '</narrative>', embedding_text=NULL, context_hint=NULL WHERE session_id='resummarize'`)
	f.exec(`UPDATE session_ingest_summaries SET summarized_at=now() - interval '31 days' WHERE session_id='old'`)
	f.exec(`UPDATE session_ingest_candidates SET revision='sha256:v2' WHERE session_id='pending'`)

	plan, err := f.worker.Store.PlanSummaryRepair(t.Context())
	require.NoError(t, err)
	require.Equal(t, SummaryRepairPlan{FromText: 2, Resummarize: 1, ResummarizeBytes: plan.ResummarizeBytes}, plan,
		"the pending session and the summary older than 30 days are left alone")

	f.model.mapCalls, f.model.reduceCalls = 0, 0
	ledger := f.repair()
	require.Equal(t, []string{"succeeded"}, ledger.finished)
	require.Equal(t, SummaryRepairProgress{Total: 3, Processed: 3, FromText: 2, Resummarize: 1}, ledger.progress)
	require.Zero(t, f.model.mapCalls, "no chunk is mapped again")
	require.Equal(t, 1, f.model.reduceCalls, "only the summary with nothing to recover calls the model")

	narrative, embedding, hint, body := f.summary("from-text")
	require.Equal(t, "Session summary narrative", narrative)
	require.Equal(t, "recovered embedding", embedding)
	require.Equal(t, "recovered hint", hint)
	require.Equal(t, "Session summary narrative", body, "the searchable event is republished from the repaired summary")

	narrative, embedding, hint, _ = f.summary("resummarize")
	require.Equal(t, "Session summary narrative", narrative)
	require.Equal(t, "embedding tokens", embedding)
	require.Equal(t, "repo: task", hint)

	narrative, _, _, _ = f.summary("old")
	require.Equal(t, leak, narrative, "a summary older than 30 days is out of scope")

	f.model.reduceCalls = 0
	again := f.repair()
	require.Equal(t, []string{"succeeded"}, again.finished)
	require.Equal(t, SummaryRepairProgress{}, again.progress, "a second run finds nothing to repair")
	require.Zero(t, f.model.reduceCalls)
}
