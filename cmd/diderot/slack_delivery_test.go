package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/authorization"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/reembed"
	"github.com/bluesky-social/diderot/internal/scheduledreport"
	"github.com/bluesky-social/diderot/internal/settings"
)

// fakeWorkspace is the Slack workspace one Slack run posts to. It records
// the channel of each message posted; ops@example.com is UOPS, whose DM is
// DOPS. With hold set, each post reports on posting and waits for hold to
// close before it is answered.
type fakeWorkspace struct {
	mu      sync.Mutex
	posts   []string
	hold    chan struct{}
	posting chan struct{}
}

func (w *fakeWorkspace) posted() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.posts...)
}

// slackRun is what a Slack run posting to workspace lends.
func slackRun(t *testing.T, pool *pgxpool.Pool, workspace *fakeWorkspace) slackcx.Delivery {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/users.lookupByEmail":
			_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"UOPS"}}`))
		case "/conversations.open":
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"DOPS"}}`))
		case "/conversations.list":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"CLEGAL","name":"legal","is_private":true}],"response_metadata":{"next_cursor":""}}`))
		case "/chat.postMessage":
			if workspace.hold != nil {
				workspace.posting <- struct{}{}
				select {
				case <-workspace.hold:
				case <-r.Context().Done():
					return
				}
			}
			workspace.mu.Lock()
			workspace.posts = append(workspace.posts, r.Form.Get("channel"))
			workspace.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"channel":"CLEGAL","ts":"1792006300.000001"}`))
		default:
			t.Errorf("unexpected Slack request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	api := slack.New("bot-token", slack.OptionAPIURL(server.URL+"/"))
	policy, err := authorization.New(authorization.Config{})
	require.NoError(t, err)
	consumer := slackcx.NewConsumer(pool, nil, "", "", "", nil, nil).WithAPI(api)
	return slackcx.Delivery{API: api, Consumer: consumer, Channels: &slackcx.ChannelAuthorizer{Policy: policy}}
}

// TestSlackRunEndsAfterUsesInProgress: when a Slack run stops (its context
// ends, then its Clear waits for the uses holding its delivery), a report
// post still waiting on Slack is stopped, as it was when reports ran inside
// that run, so a stalled Slack call can't hold the run's end. Nothing goes
// through the run once it has ended.
func TestSlackRunEndsAfterUsesInProgress(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_reports")
	// stopping lends a run posting to workspace, starts use, waits for its
	// post to reach Slack, and stops the run.
	stopping := func(t *testing.T, workspace *fakeWorkspace, use func(*settings.Live[slackcx.Delivery]) error) (*settings.Live[slackcx.Delivery], chan error, chan struct{}) {
		t.Helper()
		live := &settings.Live[slackcx.Delivery]{}
		runCtx, stop := context.WithCancel(ctx)
		run := slackRun(t, pool, workspace)
		run.Run = runCtx
		live.Set(run)
		used := make(chan error, 1)
		go func() { used <- use(live) }()
		<-workspace.posting
		ended := make(chan struct{})
		go func() {
			stop()
			live.Clear()
			close(ended)
		}()
		return live, used, ended
	}

	t.Run("report", func(t *testing.T) {
		workspace := &fakeWorkspace{hold: make(chan struct{}), posting: make(chan struct{}, 1)}
		defer close(workspace.hold)
		live, used, ended := stopping(t, workspace, func(live *settings.Live[slackcx.Delivery]) error {
			_, err := slackReports{live: live}.DeliverScheduledReport(ctx, scheduledreport.DeliverySlackChannel, "CLEGAL", uuid.New(), "Weekly update.")
			return err
		})
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("a post stalled on Slack held the run's end")
		}
		require.ErrorIs(t, <-used, context.Canceled)
		assert.Empty(t, workspace.posted())
		_, err := slackReports{live: live}.DeliverScheduledReport(ctx, scheduledreport.DeliverySlackChannel, "CLEGAL", uuid.New(), "Weekly update.")
		require.ErrorIs(t, err, errSlackOff, "after its run ends, nothing posts through it")
	})
}

// TestChannelRefreshFollowsTheRunningSlack: the scope reconcile re-reads
// channel privacy through the Slack run of the moment, and does nothing
// while Slack is off.
func TestChannelRefreshFollowsTheRunningSlack(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_reports")
	live := &settings.Live[slackcx.Delivery]{}
	refresh := slackChannelRefresh(live, pool)
	refreshed, err := refresh(ctx)
	require.NoError(t, err)
	assert.Zero(t, refreshed)
	live.Set(slackRun(t, pool, &fakeWorkspace{}))
	refreshed, err = refresh(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, refreshed)
}

// TestReembedRebuildsThroughTheRunningSlack: the re-embed driver rebuilds
// Slack events through the rebuilder of the Slack run at each row, and
// re-embedding Slack while it is off ends with one error that says so.
func TestReembedRebuildsThroughTheRunningSlack(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_reports")
	_, err := pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	// A stored delete, which a rebuild skips without reading Slack.
	_, err = sink.Upsert(ctx, events.Event{
		Scope: events.ScopePublic, Source: "slack", ExternalID: "C1:1792006300.000001", OccurredAt: time.Now().UTC(),
		Body: "gone", Raw: []byte(`{"type":"message","subtype":"message_deleted","channel":"C1","deleted_ts":"1792006300.000001"}`),
	})
	require.NoError(t, err)
	live := &settings.Live[slackcx.Delivery]{}
	driver := reembed.NewDriver(pool, sink, discardLogger(), slackRebuilder{live: live})
	require.Equal(t, []string{"slack"}, driver.Sources())

	off := &reembed.Progress{}
	require.NoError(t, driver.Run(ctx, nil, off))
	assert.Zero(t, off.Processed.Load())
	assert.Equal(t, int64(1), off.Errors.Load())
	assert.Equal(t, "slack: the source's connector is off", off.LastErr.Load())

	live.Set(slackcx.Delivery{Rebuilder: slackcx.NewRebuilder(nil, nil, "example.slack.com")})
	on := &reembed.Progress{}
	require.NoError(t, driver.Run(ctx, nil, on))
	assert.Equal(t, int64(1), on.Processed.Load())
	assert.Equal(t, int64(1), on.Skipped.Load())
	assert.Zero(t, on.Errors.Load())
}

// TestReembedKeepsNothingRebuiltAsSlackStops: a rebuild that ends after its
// Slack run began to stop is an error for that row, never a result to save.
func TestReembedKeepsNothingRebuiltAsSlackStops(t *testing.T) {
	run, stop := context.WithCancel(t.Context())
	stop()
	live := &settings.Live[slackcx.Delivery]{}
	live.Set(slackcx.Delivery{Run: run, Rebuilder: slackcx.NewRebuilder(nil, nil, "example.slack.com")})
	// A stored delete, which a rebuild skips without an error of its own.
	event, err := slackRebuilder{live: live}.Rebuild(t.Context(), reembed.StoredRow{
		Raw: []byte(`{"type":"message","subtype":"message_deleted","channel":"C1","deleted_ts":"1792006300.000001"}`),
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, reembed.ErrSourceOff)
	assert.Nil(t, event)
}

// reportAgent writes reports. While hold is set, each report reports on
// started and waits for hold to close.
type reportAgent struct {
	pool    *pgxpool.Pool
	hold    chan struct{}
	started chan struct{}
	mu      sync.Mutex
	turns   int
}

func (a *reportAgent) CreateSession(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := a.pool.QueryRow(ctx, `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&id)
	return id, err
}

func (a *reportAgent) ExecuteTurn(ctx context.Context, _ ask.TurnRequest) (ask.TurnResult, error) {
	a.mu.Lock()
	a.turns++
	hold := a.hold
	a.mu.Unlock()
	if hold != nil {
		a.started <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
			return ask.TurnResult{}, ctx.Err()
		}
	}
	return ask.TurnResult{Reply: "Nothing material changed this week."}, nil
}

func (a *reportAgent) turnCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turns
}

// TestScheduledReportsDeliverThroughTheRunningSlack: with Slack off, a due
// report waits in the queue and nothing is written; once Slack runs it is
// delivered through that run. Slack disabled while a report is being
// written fails only its delivery, never through the run that ended, and
// the next Slack run posts the saved report without writing it again.
func TestScheduledReportsDeliverThroughTheRunningSlack(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_reports")
	_, err := pool.Exec(ctx, `TRUNCATE scheduled_reports, ask_sessions RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE connector_settings SET settings = '{"state":"live"}', version = version + 1 WHERE connector = 'org'`)
	require.NoError(t, err)
	gate := settings.NewStore(pool, discardLogger())
	require.NoError(t, gate.Load(ctx))
	reports := scheduledreport.NewStore(pool)
	report, err := reports.Create(ctx, scheduledreport.CreateParams{
		Name: "Legal weekly", Prompt: "Make the leadership update.",
		Cadence: scheduledreport.CadenceDaily, ScheduleHour: 9, Timezone: "America/Los_Angeles",
		DeliveryKind: scheduledreport.DeliverySlackChannel, SlackTarget: "CLEGAL",
		OwnerEmail: "legal@example.com", CreatedBy: "admin@example.com",
	})
	require.NoError(t, err)
	status := func() string {
		t.Helper()
		runs, err := reports.ListRuns(ctx, report.ID, 1)
		require.NoError(t, err)
		require.Len(t, runs, 1)
		return runs[0].Status
	}
	agent := &reportAgent{pool: pool}
	live := &settings.Live[slackcx.Delivery]{}
	worker := scheduledReports(reports, agent, live, gate, discardLogger())

	// Due while Slack is off: nothing is written or posted.
	_, err = reports.EnqueueNow(ctx, report.ID)
	require.NoError(t, err)
	require.NoError(t, worker.Run(ctx))
	assert.Zero(t, agent.turnCount())
	assert.Equal(t, "pending", status())

	// Slack starts: the report is written and posted through it.
	first := &fakeWorkspace{}
	live.Set(slackRun(t, pool, first))
	require.NoError(t, worker.Run(ctx))
	assert.Equal(t, 1, agent.turnCount())
	assert.Equal(t, []string{"CLEGAL"}, first.posted())
	assert.Equal(t, "succeeded", status())

	// Slack is disabled while the next report is being written.
	agent.mu.Lock()
	agent.hold, agent.started = make(chan struct{}), make(chan struct{}, 1)
	agent.mu.Unlock()
	_, err = reports.EnqueueNow(ctx, report.ID)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	<-agent.started
	live.Clear()
	close(agent.hold)
	require.ErrorIs(t, <-done, errSlackOff)
	assert.Equal(t, []string{"CLEGAL"}, first.posted(), "nothing is posted through a Slack run that has ended")
	assert.Equal(t, "generated", status())

	// The next Slack run posts the saved report.
	second := &fakeWorkspace{}
	live.Set(slackRun(t, pool, second))
	require.NoError(t, worker.Run(ctx))
	assert.Equal(t, []string{"CLEGAL"}, second.posted())
	assert.Equal(t, 2, agent.turnCount(), "the saved report is posted, not written again")
	assert.Equal(t, "succeeded", status())
}
