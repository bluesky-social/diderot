package slack

import (
	"context"
	"testing"
)

// A user Slack reports as not found is cached as a row with no profile.
// Read back from that cache the lookup succeeds, with every external flag
// false, so the gates must refuse it by name: an identity Slack would not
// resolve is not an internal one.

func TestAskGateRefusesAnAskerSlackCannotResolve(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{})

	// The first lookup fails and caches the absence; the second reads it.
	for attempt := 1; attempt <= 2; attempt++ {
		outcome, _, _ := c.evaluateAskGates(context.Background(), "C_ANY", "U_GONE")
		if outcome != gateBlockedError {
			t.Fatalf("attempt %d: got outcome %q, want %q", attempt, outcome, gateBlockedError)
		}
	}
}

func TestEnableAskClickRefusesAClickerSlackCannotResolve(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_TEST": {"U_GONE"},
		}}, nil)

	srv, _ := captureResponseURL(t)
	for attempt := 1; attempt <= 2; attempt++ {
		c.handleInteractive(context.Background(), enableAskCallback("C_TEST", "U_GONE", srv.URL))
		cfg, err := c.channelConfig.Get(context.Background(), "C_TEST")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AskEnabled {
			t.Fatalf("attempt %d: ask was enabled by a clicker Slack cannot resolve", attempt)
		}
	}
}
