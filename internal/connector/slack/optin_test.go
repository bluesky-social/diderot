package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/slack-go/slack"
)

// TestExternalMemberRepliesNameConfiguredOrg proves that an organization
// named Acme sends external members the replies it sent before the name
// moved to the Organization page, that one with no saved name names no
// organization, and that each reply reads the current name.
func TestExternalMemberRepliesNameConfiguredOrg(t *testing.T) {
	name := "Acme"
	c := (&Consumer{}).WithOrgName(func() string { return name })
	if got, want := c.externalNotEnabledMessage("C0EXAMPLE01"),
		"Hi! I saw your question, but I'm not enabled to answer in <#C0EXAMPLE01> yet — answering is opt-in per channel. Ask someone from the Acme team in this channel to enable me."; got != want {
		t.Errorf("not-enabled reply\n got: %q\nwant: %q", got, want)
	}
	if got, want := c.externalDMDeniedMessage(), "sorry, Diderot DMs are only available to internal Acme members."; got != want {
		t.Errorf("DM denial\n got: %q\nwant: %q", got, want)
	}

	name = " "
	neutral := c
	if got, want := neutral.externalNotEnabledMessage("C0EXAMPLE01"),
		"Hi! I saw your question, but I'm not enabled to answer in <#C0EXAMPLE01> yet — answering is opt-in per channel. Ask an internal member in this channel to enable me."; got != want {
		t.Errorf("neutral not-enabled reply\n got: %q\nwant: %q", got, want)
	}
	if got, want := neutral.externalDMDeniedMessage(), "sorry, Diderot DMs are only available to internal members."; got != want {
		t.Errorf("neutral DM denial\n got: %q\nwant: %q", got, want)
	}
	if got, want := (&Consumer{}).externalDMDeniedMessage(), "sorry, Diderot DMs are only available to internal members."; got != want {
		t.Errorf("no name source\n got: %q\nwant: %q", got, want)
	}
}

// captureResponseURL spins an httptest server that records webhook
// posts to the interaction response_url and returns the server +
// accessor for the recorded payloads.
func captureResponseURL(t *testing.T) (*httptest.Server, func() []slack.WebhookMessage) {
	t.Helper()
	var mu sync.Mutex
	var msgs []slack.WebhookMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m slack.WebhookMessage
		if err := json.Unmarshal(body, &m); err != nil {
			t.Errorf("response_url payload not JSON: %v", err)
		}
		mu.Lock()
		msgs = append(msgs, m)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []slack.WebhookMessage {
		mu.Lock()
		defer mu.Unlock()
		out := make([]slack.WebhookMessage, len(msgs))
		copy(out, msgs)
		return out
	}
}

func enableAskCallback(channelID, userID, responseURL string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type:        slack.InteractionTypeBlockActions,
		ResponseURL: responseURL,
		User:        slack.User{ID: userID},
		Channel:     slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: channelID}}},
		ActionCallback: slack.ActionCallbacks{
			BlockActions: []*slack.BlockAction{
				{ActionID: actionEnableAsk, Value: channelID},
			},
		},
	}
}

// TestEnableAskClick_FlipsAskEnabled is the happy path: internal
// clicker, no external members → ask_enabled=true with the clicker
// in the audit column, response_url gets the confirmation.
func TestEnableAskClick_FlipsAskEnabled(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_ROBIN": {ID: "U_ROBIN", Name: "robin"},
		},
	})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_TEST": {"U_ROBIN"},
		}}, nil)

	srv, gotMsgs := captureResponseURL(t)
	c.handleInteractive(context.Background(), enableAskCallback("C_TEST", "U_ROBIN", srv.URL))

	cfg, err := c.channelConfig.Get(context.Background(), "C_TEST")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AskEnabled {
		t.Error("ask_enabled should be true after opt-in click")
	}
	if cfg.UpdatedBy != "robin (slack opt-in)" {
		t.Errorf("audit column: got %q, want %q", cfg.UpdatedBy, "robin (slack opt-in)")
	}
	msgs := gotMsgs()
	if len(msgs) != 1 {
		t.Fatalf("response_url posts: got %d, want 1", len(msgs))
	}
	if !msgs[0].ReplaceOriginal {
		t.Error("confirmation should replace the button ephemeral")
	}
	if got := testutil.ToFloat64(c.metrics.askOptin.WithLabelValues("enabled")); got != 1 {
		t.Errorf("askOptin{enabled}: got %v, want 1", got)
	}
}

// TestEnableAskClick_ExternalMembersRefused verifies the TOCTOU
// guard: even though the button only renders on the not-enabled
// ephemeral, the click re-scans membership and refuses to self-serve
// a channel containing externals. allow_external_ask stays
// operator-only.
func TestEnableAskClick_ExternalMembersRefused(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_ROBIN":  {ID: "U_ROBIN", Name: "robin"},
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_MIXED": {"U_ROBIN", "U_VENDOR"},
		}}, nil)

	srv, gotMsgs := captureResponseURL(t)
	c.handleInteractive(context.Background(), enableAskCallback("C_MIXED", "U_ROBIN", srv.URL))

	cfg, err := c.channelConfig.Get(context.Background(), "C_MIXED")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AskEnabled {
		t.Error("ask_enabled must stay false for an external-membered channel")
	}
	msgs := gotMsgs()
	if len(msgs) != 1 {
		t.Fatalf("response_url posts: got %d, want 1", len(msgs))
	}
	if got := testutil.ToFloat64(c.metrics.askOptin.WithLabelValues("refused_external_members")); got != 1 {
		t.Errorf("askOptin{refused_external_members}: got %v, want 1", got)
	}
}

// TestEnableAskClick_ExternalClickerRefused: a guest who somehow
// obtains the button (e.g. forwarded screenshot race, or an internal
// user converted to guest between ephemeral and click) cannot flip
// the gate.
func TestEnableAskClick_ExternalClickerRefused(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_GUEST": {ID: "U_GUEST", Name: "guest", IsRestricted: true},
		},
	})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_TEST": {"U_GUEST"},
		}}, nil)

	srv, gotMsgs := captureResponseURL(t)
	c.handleInteractive(context.Background(), enableAskCallback("C_TEST", "U_GUEST", srv.URL))

	cfg, err := c.channelConfig.Get(context.Background(), "C_TEST")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AskEnabled {
		t.Error("ask_enabled must stay false when the clicker is external")
	}
	if len(gotMsgs()) != 1 {
		t.Fatal("clicker should still get a response_url explanation")
	}
	if got := testutil.ToFloat64(c.metrics.askOptin.WithLabelValues("refused_external_clicker")); got != 1 {
		t.Errorf("askOptin{refused_external_clicker}: got %v, want 1", got)
	}
}

// TestEnableAskClick_UnknownActionIgnored: payloads with other
// action_ids fall through without touching the store.
func TestEnableAskClick_UnknownActionIgnored(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.channelConfig = NewChannelConfigStore(pool, nil, &fakeMembersAPI{}, nil)

	cb := enableAskCallback("C_TEST", "U_X", "")
	cb.ActionCallback.BlockActions[0].ActionID = "some_future_button"
	c.handleInteractive(context.Background(), cb)

	cfg, err := c.channelConfig.Get(context.Background(), "C_TEST")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AskEnabled {
		t.Error("unknown action must not flip ask_enabled")
	}
}

// TestGateOutcome_ExternalMembersBeatsNotEnabled pins the gate
// ordering: a channel that is BOTH not-enabled AND
// external-membered reports blocked_external_members, so the
// invoker-facing ephemeral never offers the self-serve button for a
// channel the operator must vet by hand.
func TestGateOutcome_ExternalMembersBeatsNotEnabled(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_AVERY":  {ID: "U_AVERY", Name: "avery"},
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_MIXED": {"U_AVERY", "U_VENDOR"},
		}}, nil)

	outcome, externals, _ := c.evaluateAskGates(context.Background(), "C_MIXED", "U_AVERY")
	if outcome != gateBlockedExternalMembers {
		t.Errorf("got outcome %q, want %q", outcome, gateBlockedExternalMembers)
	}
	// The blocked outcome carries WHO is external so the ephemeral
	// can name them.
	if len(externals) != 1 || externals[0] != "U_VENDOR" {
		t.Errorf("externals = %v, want [U_VENDOR]", externals)
	}
}

// TestGateOutcome_NotEnabled: internal asker, internal-only channel,
// no opt-in row → blocked_not_enabled (the self-serve case).
func TestGateOutcome_NotEnabled(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_AVERY": {ID: "U_AVERY", Name: "avery"},
		},
	})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_DARK": {"U_AVERY"},
		}}, nil)

	outcome, _, _ := c.evaluateAskGates(context.Background(), "C_DARK", "U_AVERY")
	if outcome != gateBlockedNotEnabled {
		t.Errorf("got outcome %q, want %q", outcome, gateBlockedNotEnabled)
	}
}

// TestGateOutcome_FullyOptedInRoutesOnScanFailure preserves the
// pre-restructure posture: when the members scan fails but the
// operator fully opted the channel in (ask_enabled AND
// allow_external_ask), transient scan errors don't block routing.
func TestGateOutcome_FullyOptedInRoutesOnScanFailure(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_AVERY": {ID: "U_AVERY", Name: "avery"},
		},
	})
	store := NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{failOnce: true}, nil)
	ctx := context.Background()
	if err := store.SetAskEnabled(ctx, "C_OPTED", true, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAllowExternalAsk(ctx, "C_OPTED", true, "test"); err != nil {
		t.Fatal(err)
	}
	c.channelConfig = store

	outcome, _, _ := c.evaluateAskGates(ctx, "C_OPTED", "U_AVERY")
	if outcome != gateAllowed {
		t.Errorf("got outcome %q, want %q (scan failure on fully-opted channel must route)", outcome, gateAllowed)
	}
}

// TestGateOutcome_ScanFailureNotOptedInBlocks: scan failure on an
// unconfigured channel fails closed.
func TestGateOutcome_ScanFailureNotOptedInBlocks(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_AVERY": {ID: "U_AVERY", Name: "avery"},
		},
	})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{failOnce: true}, nil)

	outcome, _, _ := c.evaluateAskGates(context.Background(), "C_DARK", "U_AVERY")
	if outcome != gateBlockedError {
		t.Errorf("got outcome %q, want %q", outcome, gateBlockedError)
	}
}

// TestGateOutcome_ExternalAsker_NoWhitelist: a guest in a channel
// that has NOT been whitelisted for external engagement
// (allow_external_ask=false) is silenced — gateBlockedExternalAsker.
func TestGateOutcome_ExternalAsker_NoWhitelist(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_ANY": {"U_VENDOR"},
		}}, nil)

	outcome, _, askerIsExternal := c.evaluateAskGates(context.Background(), "C_ANY", "U_VENDOR")
	if outcome != gateBlockedExternalAsker {
		t.Errorf("got outcome %q, want %q", outcome, gateBlockedExternalAsker)
	}
	if !askerIsExternal {
		t.Error("askerIsExternal must be true for a guest")
	}
}

// TestGateOutcome_ExternalAsker_NilConfig: when channelConfig is nil
// (test-only wiring — no DB), an external asker is blocked. No
// whitelist evidence means deny-by-default.
func TestGateOutcome_ExternalAsker_NilConfig(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	// c.channelConfig intentionally nil.

	outcome, _, _ := c.evaluateAskGates(context.Background(), "C_ANY", "U_VENDOR")
	if outcome != gateBlockedExternalAsker {
		t.Errorf("got outcome %q, want %q", outcome, gateBlockedExternalAsker)
	}
}

// TestGateOutcome_ExternalAsker_Whitelisted_AskEnabled: external asker
// in a fully-whitelisted channel (allow_external_ask=true,
// ask_enabled=true) is allowed. The operator accepted "externals can
// read answers here" when setting the flag.
func TestGateOutcome_ExternalAsker_Whitelisted_AskEnabled(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	store := NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_MIXED": {"U_VENDOR"},
		}}, nil)
	ctx := context.Background()
	if err := store.SetAskEnabled(ctx, "C_MIXED", true, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAllowExternalAsk(ctx, "C_MIXED", true, "test"); err != nil {
		t.Fatal(err)
	}
	c.channelConfig = store

	outcome, _, askerIsExternal := c.evaluateAskGates(ctx, "C_MIXED", "U_VENDOR")
	if outcome != gateAllowed {
		t.Errorf("got outcome %q, want %q", outcome, gateAllowed)
	}
	if !askerIsExternal {
		t.Error("askerIsExternal must be true for a guest even when allowed")
	}
}

// TestGateOutcome_ExternalAsker_Whitelisted_AskNotEnabled: external
// asker in a channel with allow_external_ask=true but ask_enabled=false
// falls through to the ask_enabled gate → gateBlockedNotEnabled. The
// ephemeral will use the text-only variant (no button) because
// askerIsExternal is true.
func TestGateOutcome_ExternalAsker_Whitelisted_AskNotEnabled(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	store := NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_MIXED": {"U_VENDOR"},
		}}, nil)
	ctx := context.Background()
	// allow_external_ask=true but ask_enabled stays false (default).
	if err := store.SetAllowExternalAsk(ctx, "C_MIXED", true, "test"); err != nil {
		t.Fatal(err)
	}
	c.channelConfig = store

	outcome, _, askerIsExternal := c.evaluateAskGates(ctx, "C_MIXED", "U_VENDOR")
	if outcome != gateBlockedNotEnabled {
		t.Errorf("got outcome %q, want %q", outcome, gateBlockedNotEnabled)
	}
	// askerIsExternal is returned so maybeRouteAsk can pick the
	// button-less ephemeral variant.
	if !askerIsExternal {
		t.Error("askerIsExternal must be true for a guest even when falling through to not-enabled")
	}
}

// TestGateOutcome_ExternalAsker_ScanError_FullyOptedIn: scan failure
// on a channel with allow_external_ask=true AND ask_enabled=true
// preserves the fail-open exception — the external asker routes
// anyway.
func TestGateOutcome_ExternalAsker_ScanError_FullyOptedIn(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	store := NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{failOnce: true}, nil)
	ctx := context.Background()
	if err := store.SetAskEnabled(ctx, "C_OPTED", true, "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAllowExternalAsk(ctx, "C_OPTED", true, "test"); err != nil {
		t.Fatal(err)
	}
	c.channelConfig = store

	outcome, _, _ := c.evaluateAskGates(ctx, "C_OPTED", "U_VENDOR")
	if outcome != gateAllowed {
		t.Errorf("got outcome %q, want %q (scan failure on fully-opted channel must route for external asker too)", outcome, gateAllowed)
	}
}

// TestGateOutcome_ExternalAsker_ScanError_NoWhitelist: scan failure
// for an external asker in a non-whitelisted channel must still
// return gateBlockedExternalAsker (silence) — NOT gateBlockedError.
// Emitting gateBlockedError here would cause maybeRouteAsk to post
// an ephemeral to a guest the operator hasn't vouched for.
func TestGateOutcome_ExternalAsker_ScanError_NoWhitelist(t *testing.T) {
	pool := getChannelConfigPool(t)
	c := newTestConsumer(t, pool)
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		},
	})
	// failOnce causes EnsureExternalMembersFresh to return an error.
	// The channel has no config row, so the stale cfg has
	// AllowExternalAsk=false — the non-whitelisted path.
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{failOnce: true}, nil)

	outcome, _, _ := c.evaluateAskGates(context.Background(), "C_DARK", "U_VENDOR")
	if outcome != gateBlockedExternalAsker {
		t.Errorf("got outcome %q, want %q (scan error in non-whitelisted channel must silence external asker)", outcome, gateBlockedExternalAsker)
	}
}

// TestGateMetric_IncrementsOnBlockedMention drives the full
// handleEventsAPI path for a blocked mention and asserts the gate
// counter (the operator-visible signal that used to be a bare log
// line).
func TestGateMetric_IncrementsOnBlockedMention(t *testing.T) {
	pool := getConsumerPool(t)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE slack_channel_config CASCADE`); err != nil {
		t.Fatal(err)
	}
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	c.dir = newDirectoryWithAPI(pool, &fakeAPI{
		users: map[string]*slack.User{
			"U_AVERY": {ID: "U_AVERY", Name: "avery"},
		},
	})
	c.channelConfig = NewChannelConfigStore(pool, c.dir,
		&fakeMembersAPI{members: map[string][]string{
			"C_DARK": {"U_AVERY"},
		}}, nil)
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router

	raw := `{
		"type": "message",
		"channel": "C_DARK",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> hello",
		"ts": "1700000020.000100"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))

	if got := testutil.ToFloat64(c.metrics.askGate.WithLabelValues(string(gateBlockedNotEnabled))); got != 1 {
		t.Errorf("askGate{blocked_not_enabled}: got %v, want 1", got)
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.createCalls) != 0 {
		t.Errorf("blocked mention must not reach session create, got %d", len(router.createCalls))
	}
}
