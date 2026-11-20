package slack

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"
	"golang.org/x/time/rate"
)

// Slack-go web API method-name constants. Used as the `method`
// label on limiter metrics so a stray string typo doesn't fork
// cardinality, AND as the key into the method→tier table below. Add
// a new const here when wiring a new call site, and add it to
// methodTiers so it lands in the right bucket instead of the default.
const (
	methodHistory           = "conversations.history"
	methodReplies           = "conversations.replies"
	methodConvList          = "conversations.list"
	methodConvJoin          = "conversations.join"
	methodConvUserList      = "users.conversations"
	methodConvMembers       = "conversations.members"
	methodConvInfo          = "conversations.info"
	methodOpenConversation  = "conversations.open"
	methodUserInfo          = "users.info"
	methodUserLookupByEmail = "users.lookupByEmail"
	methodBotInfo           = "bots.info"
	methodUsersList         = "users.list"
	methodUserGroupsList    = "usergroups.list"
	methodFilesInfo         = "files.info"
	methodPostMessage       = "chat.postMessage"
	methodPostEphemeral     = "chat.postEphemeral"
	methodUpdateMessage     = "chat.update"
	methodReactionsAdd      = "reactions.add"
	methodReactionsRemove   = "reactions.remove"
	methodReactionsGet      = "reactions.get"
	methodGetPermalink      = "chat.getPermalink"
	methodSetSessionStatus  = "agents.sessions.setStatus"
)

// tier identifies which Slack rate-limit class a method falls into.
// Slack enforces limits PER METHOD PER WORKSPACE PER APP, grouped
// into documented tiers (https://docs.slack.dev/apis/web-api/rate-limits):
//
//	Tier 1  ~1+/min    (rare; punitive methods — we call none today)
//	Tier 2  ~20+/min   (directory listings: conversations.list, users.list)
//	Tier 3  ~50+/min   (the bulk: history, replies, info, reactions, update)
//	Tier 4  ~100+/min  (cheap lookups + ephemeral/post: members, users.info)
//
// The "+" in Slack's docs means the floor is guaranteed and bursts
// above it are often tolerated. We pace at the documented floor and
// let the reactive layer (WithSlackRetry honoring Retry-After) be the
// truth-finder when Slack actually pushes back — slackdump over-budgets
// Tier 3 to ~120/min and relies entirely on 429s to correct; we sit at
// or just above the floor because we ALSO have proactive pacing and
// would rather under-utilize than churn 429s.
//
// NOTE on conversations.history / conversations.replies: since May
// 2025 Slack limits these two methods to 1 request per minute, and 15
// messages per request, for commercially distributed apps that are not
// in the Slack Marketplace. Internal customer-built apps (built for and
// installed only in the deployer's own workspace) and Marketplace apps
// keep the classic Tier 3 allowance of 50+ requests per minute. See
// https://docs.slack.dev/changelog/2025/05/29/rate-limit-changes-for-non-marketplace-apps
//
// This table assumes the classic allowance. If your Slack app is a
// distributed non-Marketplace app, these two methods are effectively
// tier 1 and must move to tier1 here; until they do, the proactive
// pacing is far too fast for them and every sweep runs on 429s and
// Retry-After alone.
type tier int

const (
	tier1 tier = iota
	tier2
	tier3
	tier4
	// numTiers is the count of tiers; keep last. Used to size the
	// per-tier bucket slice.
	numTiers
)

func (t tier) String() string {
	switch t {
	case tier1:
		return "tier1"
	case tier2:
		return "tier2"
	case tier3:
		return "tier3"
	case tier4:
		return "tier4"
	default:
		return "unknown"
	}
}

// methodTiers maps every method we call to its documented Slack tier.
// Verified against https://docs.slack.dev/reference/methods (each
// method's Facts section) on 2026-06-12. Methods NOT in this map fall
// to defaultTier (tier2) via tierFor — never unlimited, never a panic.
//
//	conversations.history  T3   (classic; see the NOTE on tier)
//	conversations.replies  T3   (classic; see the NOTE on tier)
//	conversations.list     T2
//	users.conversations    T3
//	conversations.members  T4
//	conversations.info     T3
//	conversations.open     T3
//	users.info             T4
//	users.lookupByEmail    T3   (verified 2026-11-13)
//	bots.info              T3
//	users.list             T2
//	usergroups.list        T2
//	chat.postMessage       T4*  (see chat.postMessage note below)
//	chat.postEphemeral     T4
//	chat.update            T3
//	reactions.add          T3
//	reactions.remove       T2
//	chat.getPermalink      T4   (docs say "special / hundreds per
//	                             minute"; no numeric tier. T4 is the
//	                             most generous numbered bucket, so it's
//	                             the conservative-but-not-starving home.)
var methodTiers = map[string]tier{
	methodHistory:           tier3,
	methodReplies:           tier3,
	methodConvList:          tier2,
	methodConvJoin:          tier3,
	methodConvUserList:      tier3,
	methodConvMembers:       tier4,
	methodConvInfo:          tier3,
	methodOpenConversation:  tier3,
	methodUserInfo:          tier4,
	methodUserLookupByEmail: tier3,
	methodBotInfo:           tier3,
	methodUsersList:         tier2,
	methodUserGroupsList:    tier2,
	methodFilesInfo:         tier4,

	// chat.postMessage is NOT a normal tier. Slack documents it as a
	// special limit of ~1 message/sec/channel "with occasional bursts
	// tolerated." That's a PER-CHANNEL budget, which a single global
	// bucket can't model precisely — our bucket is per-app, not
	// per-channel. With asks spread across channels and a handful of
	// posts per ask, the per-channel limit is nowhere near binding, so a
	// Tier-4-ish global bucket (~100/min, i.e. comfortably above 1/sec
	// aggregate) is a safe approximation. Fanning out many posts into
	// ONE channel rapidly would need a real per-channel sub-bucket.
	methodPostMessage:      tier4,
	methodPostEphemeral:    tier4,
	methodUpdateMessage:    tier3,
	methodReactionsAdd:     tier3,
	methodReactionsRemove:  tier2,
	methodReactionsGet:     tier3, // Slack docs: Tier 3
	methodGetPermalink:     tier4,
	methodSetSessionStatus: tier3,
}

// defaultTier is where unmapped methods land. Tier 2 (~20/min) is the
// safe middle: slow enough that an unknown punitive (tier1) method
// won't 429-storm before we notice and map it, fast enough that a
// genuinely cheap unmapped method isn't needlessly starved. The
// reactive Retry-After layer still corrects if the real limit is
// tighter. Never tier4 (too optimistic for an unknown), never
// unlimited.
const defaultTier = tier2

// tierFor returns the tier for a method, falling back to defaultTier
// for anything not in methodTiers.
func tierFor(method string) tier {
	if t, ok := methodTiers[method]; ok {
		return t
	}
	return defaultTier
}

// Limiter is the per-tier Slack-API budget governor. Every call into
// slack-go (history, replies, conversations.list, users.info,
// chat.postMessage, etc.) goes through Wait → call → Record* via
// WithSlackRetry. Wait routes the method to its tier's token bucket so
// unrelated budgets don't couple: a backfill draining the Tier-3
// history bucket can no longer starve a Tier-4 chat.postMessage on the
// ask path, and each tier runs at roughly its own documented Slack
// allowance instead of all sharing one global 1-req/s bucket.
//
// Per-tier buckets, not per-method: Slack's budget is per-method, but
// methods in the same tier share a documented allowance class and at
// our load the intra-tier contention is negligible. Per-tier is the
// right granularity — it captures the cross-tier decoupling that
// matters (read sweep vs. write path) without exploding into 16
// independent buckets we'd have to tune separately.
//
// Buckets are fixed at construction (one *rate.Limiter per tier) and
// never mutated, so Wait needs no lock — *rate.Limiter is itself
// goroutine-safe. This is mode-less: 429s are recorded as metrics and
// the goroutine that earned the 429 sleeps Retry-After locally before
// retrying (see WithSlackRetry). There is no global mode flip.
//
// The Limiter is NOT a substitute for honoring Slack's Retry-After
// header — proactive pacing is a floor that keeps us from hammering
// Slack, but the reactive Retry-After obedience in WithSlackRetry stays
// the authority on the real limit.
type Limiter struct {
	logger *slog.Logger
	cfg    LimiterConfig

	// buckets is indexed by tier; one *rate.Limiter per tier. Set once
	// in NewLimiter, never mutated — no lock needed.
	buckets [numTiers]*rate.Limiter

	metrics *limiterMetrics
}

// tierConfig is one tier's pacing knobs.
type tierConfig struct {
	// Interval is the time between tokens (sustained rate = 1/Interval).
	Interval time.Duration
	// Burst is the bucket size — how many calls can fire back-to-back
	// before pacing kicks in.
	Burst int
}

// LimiterConfig captures the tunable knobs.
//
// Backward-compatibility: the pre-per-tier shape exposed
// NormalInterval / NormalBurst as the single global bucket's knobs.
// cmd/diderot constructs the Limiter via DefaultLimiterConfig() and never
// sets these by hand, but the fields are KEPT and remain load-bearing
// as an OVERRIDE for the default tier (tier2): a non-zero NormalInterval
// or NormalBurst replaces the corresponding tier2 default. This keeps
// any caller that sets them working, and gives operators a single knob
// to slow the "unknown method" bucket without a new config surface. The
// other tiers (1/3/4) are not externally tunable today — they're paced
// at their documented Slack allowances (see defaultTierConfigs). If
// per-tier tuning is ever needed, add a Tiers map here; until then the
// documented floors are the right values and an extra knob is just
// surface area.
type LimiterConfig struct {
	// NormalInterval, if > 0, overrides the default-tier (tier2)
	// interval. Zero means "use the documented tier-2 floor."
	NormalInterval time.Duration
	// NormalBurst, if > 0, overrides the default-tier (tier2) burst.
	// Zero means "use the documented tier-2 burst."
	NormalBurst int
}

// defaultTierConfigs returns the per-tier pacing floors. Rates sit at
// each tier's documented Slack allowance; burst gives a little head
// room so a short cluster of same-tier calls (e.g. an ask turn's few
// writes) doesn't immediately pace.
//
//	tier1  1/min,   burst 1   (punitive; we map nothing here today)
//	tier2  20/min,  burst 3   (3s between tokens)
//	tier3  50/min,  burst 5   (1.2s between tokens)
//	tier4  100/min, burst 8   (600ms between tokens)
//
// These are the floors Slack guarantees. We deliberately do NOT
// over-budget the way slackdump does (it runs tier3 at 120/min and eats
// the 429s) because we have proactive pacing AND want to minimize 429
// churn — the reactive layer is a correction, not the primary control.
func defaultTierConfigs() [numTiers]tierConfig {
	return [numTiers]tierConfig{
		tier1: {Interval: time.Minute, Burst: 1},
		tier2: {Interval: 3 * time.Second, Burst: 3},
		tier3: {Interval: 1200 * time.Millisecond, Burst: 5},
		tier4: {Interval: 600 * time.Millisecond, Burst: 8},
	}
}

// DefaultLimiterConfig returns production-ready knobs. The zero values
// mean "use the documented per-tier floors" — see defaultTierConfigs.
// Returned empty (rather than echoing the old 1s/5 global values)
// because the per-tier defaults now live in defaultTierConfigs; an
// empty LimiterConfig is the explicit "no override" signal.
func DefaultLimiterConfig() LimiterConfig {
	return LimiterConfig{}
}

// NewLimiter constructs a Limiter.
func NewLimiter(cfg LimiterConfig, logger *slog.Logger, reg prometheus.Registerer) *Limiter {
	if logger == nil {
		logger = slog.Default()
	}

	tiers := defaultTierConfigs()
	// Backward-compat override: NormalInterval/NormalBurst, if set,
	// tune the default tier (tier2). Non-positive means "leave the
	// documented floor in place."
	if cfg.NormalInterval > 0 {
		tiers[defaultTier].Interval = cfg.NormalInterval
	}
	if cfg.NormalBurst > 0 {
		tiers[defaultTier].Burst = cfg.NormalBurst
	}

	l := &Limiter{
		logger:  logger,
		cfg:     cfg,
		metrics: newLimiterMetrics(reg),
	}
	for t := tier(0); t < numTiers; t++ {
		tc := tiers[t]
		l.buckets[t] = rate.NewLimiter(rate.Every(tc.Interval), tc.Burst)
	}
	return l
}

// Wait blocks until a token is available in the method's tier bucket or
// ctx is cancelled. method is the slack-go web API method name (e.g.
// "chat.postMessage"); it picks the bucket via tierFor and is recorded
// as a metric label (alongside the tier) so the dashboard can show wait
// p99 per method and per tier — that's how cross-tier contention shows
// up if it ever bites.
func (l *Limiter) Wait(ctx context.Context, method string) error {
	t := tierFor(method)
	start := time.Now()
	err := l.buckets[t].Wait(ctx)
	l.metrics.observeWait(method, t.String(), time.Since(start))
	return err
}

// Record429 increments the 429 counter. Caller is expected to honor
// Retry-After separately (WithSlackRetry handles this).
func (l *Limiter) Record429(method string) {
	l.metrics.recordResult(method, tierFor(method).String(), "429")
}

// RecordOk records a successful (2xx, ok:true) call.
func (l *Limiter) RecordOk(method string) {
	l.metrics.recordResult(method, tierFor(method).String(), "ok")
}

// RecordOther records a non-rate-limit error (HTTP 5xx, network
// error, etc).
func (l *Limiter) RecordOther(method string) {
	l.metrics.recordResult(method, tierFor(method).String(), "other")
}

// WithSlackRetry is the canonical wait → call → record idiom for any
// slack-go web API invocation. fn returns the API call's error; its
// classification drives:
//
//   - nil  → RecordOk(method), return nil.
//   - 429  → Record429(method), honor Retry-After via context-aware
//     sleep, retry. Loops until ctx cancels or we get a non-429.
//   - other → RecordOther(method), return the error.
//
// l may be nil — that's a no-op pass-through (Wait/Record*
// disabled), useful for tests that don't wire a limiter.
//
// fn is invoked at least once; for 429s it's invoked again after the
// Retry-After sleep. Each goroutine sleeps independently — there is
// no cross-goroutine pause state. The reactive Retry-After obedience
// here is the authority on Slack's real per-method limit; the proactive
// per-tier pacing in Wait is a floor, not a ceiling.
func WithSlackRetry(ctx context.Context, l *Limiter, method string, fn func() error) error {
	for {
		if l != nil {
			if err := l.Wait(ctx, method); err != nil {
				return err
			}
		}
		err := fn()
		switch {
		case err == nil:
			if l != nil {
				l.RecordOk(method)
			}
			return nil
		case isRateLimited(err):
			var rle *slack.RateLimitedError
			_ = errors.As(err, &rle)
			if l != nil {
				l.Record429(method)
			}
			if sleepErr := sleepCtx(ctx, rle.RetryAfter); sleepErr != nil {
				return sleepErr
			}
			continue
		default:
			if l != nil {
				l.RecordOther(method)
			}
			return err
		}
	}
}
