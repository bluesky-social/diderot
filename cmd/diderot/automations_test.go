package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/automation"
	"github.com/bluesky-social/diderot/internal/config"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/settings"
)

// TestAutomationRegistryIsComplete: every automation this binary registers
// is a connector its pages know, after the sources and the tools.
func TestAutomationRegistryIsComplete(t *testing.T) {
	var automations automation.Registry
	require.NotPanics(t, func() { automations = automationRegistry(&config.Config{}, nil) })
	require.Equal(t, []string{"pagerduty"}, automations.Names())
	sources, tools := sourceRegistry(nil, nil, nil, nil, nil, nil, false), toolRegistry()
	var connectors []string
	for _, def := range connectorRegistry(sources, tools, automations).Definitions() {
		connectors = append(connectors, def.Name())
	}
	require.Equal(t, append(append(sources.Names(), tools.Names()...), automations.Names()...), connectors)
}

// recordingAutomation records each Run's settings version and, while one
// runs, the Slack delivery that run's deps read.
type recordingAutomation struct {
	automation.Automation
	mu       sync.Mutex
	versions []int64
	running  *automation.Deps
	stops    int
}

func (a *recordingAutomation) Name() string                 { return "pages" }
func (*recordingAutomation) Fields() []settings.Field       { return nil }
func (*recordingAutomation) Validate(settings.Values) error { return nil }

func (a *recordingAutomation) Run(ctx context.Context, deps automation.Deps, s settings.Resolved) error {
	a.mu.Lock()
	a.versions, a.running = append(a.versions, s.Version), &deps
	a.mu.Unlock()
	<-ctx.Done()
	a.mu.Lock()
	a.running = nil
	a.stops++
	a.mu.Unlock()
	return nil
}

// slack is the delivery the running Run reads now, and whether one runs.
func (a *recordingAutomation) slack() (slackcx.Delivery, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running == nil {
		return slackcx.Delivery{}, false, false
	}
	delivery, lent := a.running.Slack.Get()
	return delivery, lent, true
}

func (a *recordingAutomation) runs() ([]int64, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]int64(nil), a.versions...), a.stops
}

// TestAutomationsReadSlackAtEachUse drives an automation under the settings
// supervisor through a leader term: it starts when enabled whether or not
// Slack runs, sees Slack's delivery appear, change with a Slack restart, and
// go away without itself restarting, restarts only on its own settings
// change, and stops when the term ends.
func TestAutomationsReadSlackAtEachUse(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_automations")
	fake := &recordingAutomation{}
	store := settings.NewStore(pool, discardLogger()).WithConnectors(settings.NewRegistry(fake), settings.Secrets{})
	require.NoError(t, store.Load(ctx))
	setEnabled := func(enabled bool) {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE connector_settings SET enabled = $1, version = version + 1 WHERE connector = 'pages'`, enabled)
		require.NoError(t, err)
		require.NoError(t, store.Load(ctx))
	}
	live := &settings.Live[slackcx.Delivery]{}
	supervisor := settings.NewSupervisor(store, discardLogger(), automationRunner{Automation: fake, deps: automation.Deps{
		Pool: pool, Logger: discardLogger(), Metrics: settings.Metrics{}, Slack: live,
		AskAllowed: func() bool { return true },
	}})
	supervisor.Interval = 5 * time.Millisecond
	termCtx, endTerm := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		supervisor.Run(termCtx)
	}()
	waitRuns := func(starts, stops int) {
		t.Helper()
		require.Eventually(t, func() bool {
			versions, stopped := fake.runs()
			return len(versions) == starts && stopped == stops
		}, 5*time.Second, 5*time.Millisecond)
	}
	lent := func() (slackcx.Delivery, bool) {
		t.Helper()
		delivery, ok, running := fake.slack()
		require.True(t, running)
		return delivery, ok
	}

	// Enabled before Slack: it runs, and sees no delivery.
	setEnabled(true)
	waitRuns(1, 0)
	_, ok := lent()
	require.False(t, ok)

	// Slack starts: the same run sees its delivery.
	first := slackcx.Delivery{Consumer: &slackcx.Consumer{}}
	live.Set(first)
	got, ok := lent()
	require.True(t, ok)
	require.Same(t, first.Consumer, got.Consumer)

	// Slack restarts: the same run sees only the new run's delivery.
	live.Clear()
	_, ok = lent()
	require.False(t, ok)
	second := slackcx.Delivery{Consumer: &slackcx.Consumer{}}
	live.Set(second)
	got, _ = lent()
	require.Same(t, second.Consumer, got.Consumer)
	waitRuns(1, 0)

	// Its own settings change restarts it; disabled, it stops; enabled after
	// Slack, it sees the delivery at once.
	setEnabled(true)
	waitRuns(2, 1)
	setEnabled(false)
	waitRuns(2, 2)
	setEnabled(true)
	waitRuns(3, 2)
	got, _ = lent()
	require.Same(t, second.Consumer, got.Consumer)

	// The leader term ends: the run stops and the supervisor returns.
	endTerm()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not return when the leader term ended")
	}
	waitRuns(3, 3)
}
