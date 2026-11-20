package pagerduty

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/automation"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/settings"
)

// heldAsk answers investigations from the database's own sessions. While
// hold is set, each turn reports on started and waits for hold to close.
type heldAsk struct {
	pool    *pgxpool.Pool
	hold    chan struct{}
	started chan struct{}
	mu      sync.Mutex
	turns   int
}

func (a *heldAsk) CreateSession(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := a.pool.QueryRow(ctx, `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&id)
	return id, err
}

func (a *heldAsk) ExecuteTurn(ctx context.Context, _ ask.TurnRequest) (ask.TurnResult, error) {
	a.mu.Lock()
	a.turns++
	a.mu.Unlock()
	if a.hold != nil {
		a.started <- struct{}{}
		select {
		case <-a.hold:
		case <-ctx.Done():
			return ask.TurnResult{}, ctx.Err()
		}
	}
	return ask.TurnResult{Reply: "Hypothesis: the deploy preceded the errors.", AssistantTurn: 1}, nil
}

func (*heldAsk) LoadResultArtifacts(context.Context, uuid.UUID, int) ([]ask.ArtifactDescriptor, error) {
	return nil, nil
}

func (a *heldAsk) turnCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turns
}

// alertsChannel is a PagerDuty connector's settings that reply in the
// fixtures' alerts channel.
func alertsChannel() settings.Resolved {
	return settings.NewResolved(ConnectorName, settings.Values{
		FieldFromEmail: "oncall@example.com", FieldSlackChannelID: "CBOTALERTS", FieldSlackChannelName: "ops-alerts",
		FieldAllowedScopes: []string{"public", "internal"},
	}, map[string]string{FieldAPIToken: "pd-fixture-value", FieldWebhookSecrets: "signing-fixture"})
}

// slackLend is what a running Slack source would lend, posting to a fake
// workspace that fixture answers for.
func slackLend(t *testing.T, store *Store, fixture *slackFixture) slackcx.Delivery {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.serve(t, writer, request)
	}))
	t.Cleanup(server.Close)
	api := slack.New("bot-token", slack.OptionAPIURL(server.URL+"/slack/"))
	return slackcx.Delivery{API: api, Consumer: slackcx.NewConsumer(store.pool, nil, "", "", "", nil, nil).WithAPI(api)}
}

func automationDeps(pool *pgxpool.Pool, agent automation.Ask, live *settings.Live[slackcx.Delivery]) automation.Deps {
	return automation.Deps{
		Pool: pool, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: settings.Metrics{},
		Ask: agent, Observability: func() bool { return true }, AskAllowed: func() bool { return true }, Slack: live,
	}
}

func (fixture *slackFixture) postCount() int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return fixture.posts
}

func investigationState(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var state string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT state FROM pagerduty_investigations WHERE event_id=$1`, testEvent().ID).Scan(&state))
	return state
}

// TestInvestigationsWaitForSlack: enabled before Slack, investigations
// leave pages queued without asking anything, and start once Slack lends
// its delivery.
func TestInvestigationsWaitForSlack(t *testing.T) {
	store, pool := testStore(t)
	require.NoError(t, store.Enqueue(t.Context(), testEvent()))
	agent := &heldAsk{pool: pool}
	live := &settings.Live[slackcx.Delivery]{}
	investigations := Connector{}.investigator(automationDeps(pool, agent, live), alertsChannel())
	pager := &fakePager{}
	investigations.runner.Pager = pager

	require.NoError(t, investigations.tick(t.Context()))
	assert.Zero(t, agent.turnCount(), "nothing is investigated while Slack is off")
	assert.Equal(t, "pending", investigationState(t, pool))

	fixture := &slackFixture{rootVisible: true}
	live.Set(slackLend(t, store, fixture))
	require.NoError(t, investigations.tick(t.Context()))
	assert.Equal(t, 1, agent.turnCount())
	assert.Equal(t, 1, fixture.postCount())
	assert.Equal(t, 1, pager.calls)
	assert.Equal(t, "done", investigationState(t, pool))
}

// TestInvestigationsWaitWhileTheInstallIsInSetup: Ask may not answer the
// alerts channel before the install is live, so pages stay queued.
func TestInvestigationsWaitWhileTheInstallIsInSetup(t *testing.T) {
	store, pool := testStore(t)
	require.NoError(t, store.Enqueue(t.Context(), testEvent()))
	agent := &heldAsk{pool: pool}
	live := &settings.Live[slackcx.Delivery]{}
	live.Set(slackLend(t, store, &slackFixture{rootVisible: true}))
	deps := automationDeps(pool, agent, live)
	deps.AskAllowed = func() bool { return false }
	require.NoError(t, Connector{}.investigator(deps, alertsChannel()).tick(t.Context()))
	assert.Zero(t, agent.turnCount())
	assert.Equal(t, "pending", investigationState(t, pool))
}

// TestInvestigationsPostThroughTheSlackRunningWhenTheyPost: an investigation
// that began under one Slack run posts through the one running when it
// finishes, never through a run that has ended; with none running, it keeps
// its answer and posts it once Slack is back, without asking again.
func TestInvestigationsPostThroughTheSlackRunningWhenTheyPost(t *testing.T) {
	t.Run("restarted", func(t *testing.T) {
		store, pool := testStore(t)
		require.NoError(t, store.Enqueue(t.Context(), testEvent()))
		agent := &heldAsk{pool: pool, hold: make(chan struct{}), started: make(chan struct{}, 1)}
		live := &settings.Live[slackcx.Delivery]{}
		ended, current := &slackFixture{rootVisible: true}, &slackFixture{rootVisible: true}
		live.Set(slackLend(t, store, ended))
		investigations := Connector{}.investigator(automationDeps(pool, agent, live), alertsChannel())
		investigations.runner.Pager = &fakePager{}

		done := make(chan error, 1)
		go func() { done <- investigations.tick(t.Context()) }()
		<-agent.started
		live.Clear()
		live.Set(slackLend(t, store, current))
		close(agent.hold)
		require.NoError(t, <-done)
		assert.Zero(t, ended.postCount(), "nothing is posted through a Slack run that has ended")
		assert.Equal(t, 1, current.postCount())
		assert.Equal(t, "done", investigationState(t, pool))
	})

	t.Run("disabled", func(t *testing.T) {
		store, pool := testStore(t)
		require.NoError(t, store.Enqueue(t.Context(), testEvent()))
		agent := &heldAsk{pool: pool, hold: make(chan struct{}), started: make(chan struct{}, 1)}
		live := &settings.Live[slackcx.Delivery]{}
		ended := &slackFixture{rootVisible: true}
		live.Set(slackLend(t, store, ended))
		investigations := Connector{}.investigator(automationDeps(pool, agent, live), alertsChannel())
		pager := &fakePager{}
		investigations.runner.Pager = pager

		done := make(chan error, 1)
		go func() { done <- investigations.tick(t.Context()) }()
		<-agent.started
		live.Clear()
		close(agent.hold)
		require.ErrorIs(t, <-done, errSlackOff)
		assert.Zero(t, ended.postCount())
		assert.Equal(t, 1, pager.calls, "the incident note doesn't wait for Slack")
		assert.Equal(t, "pending", investigationState(t, pool))

		_, err := pool.Exec(t.Context(), `UPDATE pagerduty_investigations SET next_attempt_at=now()-interval '1 second'`)
		require.NoError(t, err)
		require.NoError(t, investigations.tick(t.Context()), "a retry while Slack is off leaves the page queued")
		assert.Equal(t, "pending", investigationState(t, pool))

		back := &slackFixture{rootVisible: true}
		live.Set(slackLend(t, store, back))
		require.NoError(t, investigations.tick(t.Context()))
		assert.Equal(t, 1, back.postCount())
		assert.Equal(t, 1, agent.turnCount(), "the saved answer is posted, not asked again")
		assert.Equal(t, 1, pager.calls)
		assert.Equal(t, "done", investigationState(t, pool))
	})
}

// TestRunWithoutTheInvestigatorTakesNothing: without Ask or its Grafana
// investigator, Run takes no page and returns when its leader term ends.
func TestRunWithoutTheInvestigatorTakesNothing(t *testing.T) {
	store, pool := testStore(t)
	require.NoError(t, store.Enqueue(t.Context(), testEvent()))
	live := &settings.Live[slackcx.Delivery]{}
	live.Set(slackLend(t, store, &slackFixture{rootVisible: true}))
	noInvestigator := automationDeps(pool, &heldAsk{pool: pool}, live)
	noInvestigator.Observability = func() bool { return false }
	for name, deps := range map[string]automation.Deps{
		"no ask":          automationDeps(pool, nil, live),
		"no investigator": noInvestigator,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- (Connector{}).Run(ctx, deps, alertsChannel()) }()
			time.Sleep(50 * time.Millisecond)
			assert.Equal(t, "pending", investigationState(t, pool))
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return when its context ended")
			}
		})
	}
}

// TestRunInvestigatesUntilItsTermEnds: Run's workers take a queued page at
// once, and Run returns when its leader term ends.
func TestRunInvestigatesUntilItsTermEnds(t *testing.T) {
	store, pool := testStore(t)
	require.NoError(t, store.Enqueue(t.Context(), testEvent()))
	pagerDuty := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/incidents/P123/notes" && request.Method == http.MethodPost:
			_, _ = writer.Write([]byte(`{"note":{"id":"NOTE1"}}`))
		case request.URL.Path == "/incidents/P123/notes":
			_, _ = writer.Write([]byte(`{"notes":[]}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(pagerDuty.Close)
	agent := &heldAsk{pool: pool}
	live := &settings.Live[slackcx.Delivery]{}
	fixture := &slackFixture{rootVisible: true}
	live.Set(slackLend(t, store, fixture))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Connector{APIURL: pagerDuty.URL}.Run(ctx, automationDeps(pool, agent, live), alertsChannel())
	}()
	require.Eventually(t, func() bool { return investigationState(t, pool) == "done" }, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, 1, fixture.postCount())
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its leader term ended")
	}
}

// TestStatusSaysWhyPagesWait: the page says what investigations wait for.
func TestStatusSaysWhyPagesWait(t *testing.T) {
	on, off := func() bool { return true }, func() bool { return false }
	waiting := func(c Connector) string {
		t.Helper()
		status, err := c.Status(t.Context())
		require.NoError(t, err)
		assert.Empty(t, status.Rows)
		return status.Waiting
	}
	assert.Empty(t, waiting(Connector{Ask: true, Grafana: on, Slack: on}))
	assert.Equal(t, "Waiting for Slack to be enabled. Alerts wait in the queue until then.",
		waiting(Connector{Ask: true, Grafana: on, Slack: off}))
	assert.Equal(t, "Waiting for Ask, which is off in this deployment. Alerts wait in the queue until then.",
		waiting(Connector{Grafana: on, Slack: on}))
	assert.Equal(t, "Waiting for a Grafana connection for Ask to investigate with. Alerts wait in the queue until then.",
		waiting(Connector{Ask: true, Slack: off}))
}
