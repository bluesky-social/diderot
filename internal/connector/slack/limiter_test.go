package slack

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"golang.org/x/time/rate"
)

// testMethod is a stand-in slack-go method name passed through
// limiter calls in tests where the specific method label doesn't
// matter. Production sites use the methodFoo constants from
// limiter.go.
const testMethod = "test.method"

// fastLimiterCfg is the test-suite default. Tight intervals so we
// don't wait real seconds. testMethod is unmapped, so it routes to
// the default tier (tier2); NormalInterval/NormalBurst override that
// tier's pacing, which is why these knobs still bite for testMethod.
func fastLimiterCfg() LimiterConfig {
	return LimiterConfig{
		NormalInterval: 5 * time.Millisecond,
		NormalBurst:    5,
	}
}

func TestLimiterRecordsAreMetricOnly(t *testing.T) {
	// Record429 / RecordOk / RecordOther must not panic and must
	// not adjust the underlying token-bucket pacing — the post-1d
	// limiter is mode-less. -race + the Wait timing in
	// TestLimiterRecord429DoesNotSlowSubsequentWaits cover the rest.
	l := NewLimiter(fastLimiterCfg(), nil, nil)
	l.Record429(testMethod)
	l.RecordOk(testMethod)
	l.RecordOther(testMethod)
}

func TestLimiterRecord429DoesNotSlowSubsequentWaits(t *testing.T) {
	// Pre-1d, a 429 flipped the limiter to 1 req/min for ~5
	// minutes. Post-1d, Record429 is metric-only — the bucket
	// pacing must stay at NormalInterval. Verify by recording a
	// 429 then measuring the gap between two Wait calls; it
	// should hover near NormalInterval (5ms here), not jump to
	// 1 req/min.
	l := NewLimiter(fastLimiterCfg(), nil, nil)
	l.Record429(testMethod)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// Burn the burst.
	for i := 0; i < fastLimiterCfg().NormalBurst; i++ {
		if err := l.Wait(ctx, testMethod); err != nil {
			t.Fatalf("burn burst[%d]: %v", i, err)
		}
	}
	t0 := time.Now()
	if err := l.Wait(ctx, testMethod); err != nil {
		t.Fatalf("post-burst wait: %v", err)
	}
	gap := time.Since(t0)
	if gap > 50*time.Millisecond {
		t.Errorf("post-429 Wait should pace at NormalInterval (~5ms); got %v — degraded-mode regression", gap)
	}
}

func TestLimiterWaitRespectsContextCancel(t *testing.T) {
	cfg := fastLimiterCfg()
	cfg.NormalBurst = 1 // exhaust the bucket immediately
	cfg.NormalInterval = 1 * time.Hour
	l := NewLimiter(cfg, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Wait(ctx, testMethod); err != nil {
		t.Fatalf("first Wait should succeed: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- l.Wait(ctx, testMethod)
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected ctx cancel error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after ctx cancel")
	}
}

func TestLimiterConcurrentRecordsAreSafe(t *testing.T) {
	// Goroutines hammering Record* concurrently should never panic.
	// -race catches the rest.
	l := NewLimiter(fastLimiterCfg(), nil, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(stop)
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			n := uint64(0)
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch (n + uint64(seed)) % 3 {
				case 0:
					l.Record429(testMethod)
				case 1:
					l.RecordOk(testMethod)
				case 2:
					l.RecordOther(testMethod)
				}
				n++
			}
		}(i)
	}
	wg.Wait()
}

func TestWithSlackRetryNilLimiterPassesThrough(t *testing.T) {
	// nil limiter = no Wait, no Record*, just one fn call.
	calls := 0
	err := WithSlackRetry(context.Background(), nil, testMethod, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("nil-limiter happy path: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 fn call, got %d", calls)
	}
}

func TestWithSlackRetryNilLimiterStillRetriesOn429(t *testing.T) {
	// Even without a limiter wired (tests, ad-hoc deploys),
	// Retry-After honoring must still work — that's the
	// pre-rework backcompat contract from backfill.go.
	calls := 0
	err := WithSlackRetry(context.Background(), nil, testMethod, func() error {
		calls++
		if calls == 1 {
			return &slack.RateLimitedError{RetryAfter: 5 * time.Millisecond}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("nil-limiter retry: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 fn calls (429 then ok); got %d", calls)
	}
}

func TestWithSlackRetryRetriesOn429(t *testing.T) {
	// First call returns a Slack 429 with a tiny Retry-After;
	// second call succeeds. Verify both fire.
	l := NewLimiter(fastLimiterCfg(), nil, nil)
	calls := 0
	err := WithSlackRetry(context.Background(), l, testMethod, func() error {
		calls++
		if calls == 1 {
			return &slack.RateLimitedError{RetryAfter: 5 * time.Millisecond}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry happy path: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 fn calls (429 then ok); got %d", calls)
	}
}

func TestWithSlackRetryReturnsNon429Errors(t *testing.T) {
	// Non-429 errors are propagated immediately, no retry.
	l := NewLimiter(fastLimiterCfg(), nil, nil)
	calls := 0
	sentinel := errors.New("slack: internal_error")
	err := WithSlackRetry(context.Background(), l, testMethod, func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 fn call (no retry on non-429); got %d", calls)
	}
}

func TestWithSlackRetryRespectsContextCancelDuringSleep(t *testing.T) {
	// While sleeping for Retry-After, ctx cancel should surface
	// immediately and abort the retry.
	l := NewLimiter(fastLimiterCfg(), nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := WithSlackRetry(ctx, l, testMethod, func() error {
		calls++
		return &slack.RateLimitedError{RetryAfter: 5 * time.Second}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected ctx.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 fn call before ctx cancel; got %d", calls)
	}
}

func TestLimiterWaitConcurrentlyHonorsBurst(t *testing.T) {
	// 10 goroutines all call Wait. The burst is 5, refill interval
	// is 5ms; the first 5 should release ~immediately, the rest
	// should be paced. After ~50ms total elapsed, all 10 should
	// be released.
	cfg := LimiterConfig{
		NormalInterval: 5 * time.Millisecond,
		NormalBurst:    5,
	}
	l := NewLimiter(cfg, nil, nil)
	const N = 10
	var done atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := l.Wait(ctx, testMethod); err == nil {
				done.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if int(done.Load()) != N {
		t.Errorf("expected all %d Waits to succeed; got %d", N, done.Load())
	}
}

// --- per-tier bucket tests ---

// TestTierForKnownMethods asserts every method constant maps to the
// tier we verified against docs.slack.dev (2026-06-12). If a future
// edit drops a method out of methodTiers or remaps it, this fails
// loudly rather than silently routing to the default tier.
func TestTierForKnownMethods(t *testing.T) {
	cases := map[string]tier{
		methodHistory:          tier3,
		methodReplies:          tier3,
		methodConvList:         tier2,
		methodConvUserList:     tier3,
		methodConvMembers:      tier4,
		methodConvInfo:         tier3,
		methodOpenConversation: tier3,
		methodUserInfo:         tier4,
		methodUsersList:        tier2,
		methodUserGroupsList:   tier2,
		methodPostMessage:      tier4,
		methodPostEphemeral:    tier4,
		methodUpdateMessage:    tier3,
		methodReactionsAdd:     tier3,
		methodReactionsRemove:  tier2,
		methodGetPermalink:     tier4,
		methodSetSessionStatus: tier3,
	}
	for method, want := range cases {
		if got := tierFor(method); got != want {
			t.Errorf("tierFor(%q) = %v, want %v", method, got, want)
		}
	}
}

// TestAllMethodConstantsAreMapped guards against a new methodFoo
// constant being added without a methodTiers entry. Every constant we
// declare is a call site we make; an unmapped one would silently fall
// to the default tier, which is a footgun the inventory step is meant
// to prevent. (This list mirrors the const block in limiter.go.)
func TestAllMethodConstantsAreMapped(t *testing.T) {
	all := []string{
		methodHistory, methodReplies, methodConvList, methodConvUserList,
		methodConvMembers, methodConvInfo, methodOpenConversation,
		methodUserInfo, methodUsersList, methodUserGroupsList,
		methodPostMessage, methodPostEphemeral, methodUpdateMessage,
		methodReactionsAdd, methodReactionsRemove, methodGetPermalink,
		methodSetSessionStatus,
	}
	for _, m := range all {
		if _, ok := methodTiers[m]; !ok {
			t.Errorf("method %q has a constant but no methodTiers entry — add it to the tier map", m)
		}
	}
}

// TestTierForUnknownMethodUsesDefault asserts an unmapped method routes
// to the default tier rather than panicking or going unlimited.
func TestTierForUnknownMethodUsesDefault(t *testing.T) {
	if got := tierFor("totally.unknown.method"); got != defaultTier {
		t.Errorf("tierFor(unknown) = %v, want defaultTier (%v)", got, defaultTier)
	}
	// And the limiter must actually serve a Wait for an unknown method
	// (no panic, real bucket).
	l := NewLimiter(DefaultLimiterConfig(), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := l.Wait(ctx, "totally.unknown.method"); err != nil {
		t.Fatalf("Wait on unknown method should succeed: %v", err)
	}
}

// TestPerTierIndependence is the core decoupling guarantee: exhausting
// one tier's bucket must NOT block a call in a different tier. We pick
// a tier-3 method and a tier-4 method, set tier 3 to a glacial refill
// with burst 1, leave tier 4 fast, then prove the tier-4 call returns
// promptly even after the tier-3 bucket is drained.
func TestPerTierIndependence(t *testing.T) {
	l := NewLimiter(DefaultLimiterConfig(), nil, nil)
	// Override the tier-3 and tier-4 buckets directly to make the test
	// deterministic without waiting on the production floors. This
	// reaches into the unexported buckets array — acceptable in a
	// white-box _test in the same package, and the whole point is to
	// assert the buckets are genuinely separate objects.
	l.buckets[tier3] = newTestBucket(time.Hour, 1)        // glacial; one token only
	l.buckets[tier4] = newTestBucket(time.Millisecond, 8) // fast

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Drain tier 3 (methodHistory). First call consumes the lone token.
	if err := l.Wait(ctx, methodHistory); err != nil {
		t.Fatalf("first tier-3 Wait should succeed: %v", err)
	}

	// A second tier-3 call would now block ~1h — so we DON'T make it.
	// Instead, prove a tier-4 method (chat.postMessage) is unaffected:
	// it must release promptly despite tier 3 being exhausted.
	t4start := time.Now()
	for i := 0; i < 5; i++ {
		if err := l.Wait(ctx, methodPostMessage); err != nil {
			t.Fatalf("tier-4 Wait[%d] should not block on drained tier-3: %v", i, err)
		}
	}
	if elapsed := time.Since(t4start); elapsed > 500*time.Millisecond {
		t.Errorf("tier-4 calls took %v — looks coupled to the drained tier-3 bucket", elapsed)
	}

	// Sanity: confirm the tier-3 bucket really is drained (a Wait with a
	// short deadline times out), proving we exhausted the right bucket.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()
	if err := l.Wait(shortCtx, methodHistory); err == nil {
		t.Error("expected drained tier-3 bucket to block the second Wait")
	}
}

// newTestBucket is a small helper mirroring how NewLimiter builds a
// bucket, for tests that swap a tier's bucket to deterministic pacing.
func newTestBucket(interval time.Duration, burst int) *rate.Limiter {
	return rate.NewLimiter(rate.Every(interval), burst)
}

// TestNormalConfigOverridesDefaultTierOnly asserts the backward-compat
// NormalInterval/NormalBurst knobs tune ONLY the default tier (tier2)
// and leave the other tiers at their documented floors. This is the
// config-compat contract: cmd/diderot passes DefaultLimiterConfig() (empty)
// today, but any caller setting Normal* still gets a working default
// tier, and the other tiers are unperturbed.
func TestNormalConfigOverridesDefaultTierOnly(t *testing.T) {
	cfg := LimiterConfig{
		NormalInterval: 7 * time.Second,
		NormalBurst:    11,
	}
	l := NewLimiter(cfg, nil, nil)

	// Default tier (tier2) should reflect the override.
	if got := l.buckets[defaultTier].Burst(); got != 11 {
		t.Errorf("default-tier burst = %d, want 11 (NormalBurst override)", got)
	}
	wantLimit := rate.Every(7 * time.Second)
	if got := l.buckets[defaultTier].Limit(); got != wantLimit {
		t.Errorf("default-tier limit = %v, want %v (NormalInterval override)", got, wantLimit)
	}

	// Other tiers must keep their documented floors, untouched by the
	// override.
	defaults := defaultTierConfigs()
	for _, tr := range []tier{tier1, tier3, tier4} {
		if got := l.buckets[tr].Burst(); got != defaults[tr].Burst {
			t.Errorf("%v burst = %d, want %d (override must not leak across tiers)", tr, got, defaults[tr].Burst)
		}
		if got, want := l.buckets[tr].Limit(), rate.Every(defaults[tr].Interval); got != want {
			t.Errorf("%v limit = %v, want %v (override must not leak across tiers)", tr, got, want)
		}
	}
}

// TestEmptyConfigUsesDocumentedFloors asserts DefaultLimiterConfig()
// (the empty config cmd/diderot passes) yields the documented per-tier
// floors with no override applied.
func TestEmptyConfigUsesDocumentedFloors(t *testing.T) {
	l := NewLimiter(DefaultLimiterConfig(), nil, nil)
	defaults := defaultTierConfigs()
	for _, tr := range []tier{tier1, tier2, tier3, tier4} {
		if got := l.buckets[tr].Burst(); got != defaults[tr].Burst {
			t.Errorf("%v burst = %d, want documented floor %d", tr, got, defaults[tr].Burst)
		}
		if got, want := l.buckets[tr].Limit(), rate.Every(defaults[tr].Interval); got != want {
			t.Errorf("%v limit = %v, want documented floor %v", tr, got, want)
		}
	}
}
