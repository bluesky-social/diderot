package slack

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// syncMetrics is the SyncWorker's prometheus surface. Held by
// pointer on SyncWorker so a nil reg disables the lot (cheap;
// each method is a no-op if its target is nil).
//
// The diderot dashboard reads from these.
type syncMetrics struct {
	// sweeps_total{outcome=ok|error|preempted}
	//   ok        = sweep completed cleanly, markDone wrote state
	//   error     = sweep returned an error, markError wrote state
	//   preempted = stale-recovery (or another worker) reclaimed
	//               the row mid-sweep; we silently surrendered.
	// Spikes after pod restarts; near-zero in steady state.
	sweeps *prometheus.CounterVec

	// pages_total: one increment per successful conversations.history
	// page returned by Slack. Pair with limiter_wait_seconds to see
	// "are we throttled, or just not finding work."
	pages prometheus.Counter

	// messages_total{outcome,tier}: one increment per message returned
	// by the API after parsing. Splits into:
	//   inserted        — sink wrote a new row
	//   updated         — sink overwrote an existing row (edit)
	//   nochange        — sink short-circuited (idempotent re-ingest)
	//   skipped_subtype — parser returned res.Event == nil
	//   parse_error     — parser/sink returned an error
	// tier is normal|deep (matches the sweep tier for this page).
	// {outcome="inserted",tier="normal"} is the direct socket-miss
	// recovery signal — it counts messages the sweep inserted that
	// live socket-mode missed. {tier="deep"} is bulk new coverage
	// from the wider history window. 5 outcomes × 2 tiers = 10
	// series; fine.
	messages *prometheus.CounterVec

	// claims_total: one increment per ClaimNext that returns a
	// non-nil claim (i.e. work picked up). Pair with idle_ticks.
	claims prometheus.Counter

	// idle_ticks_total: one increment per ClaimNext that returns
	// nil (no eligible row). High idle relative to claims means
	// backlog drained — healthy steady state.
	idleTicks prometheus.Counter

	// blacklisted_total: one increment per ClaimNext that flipped
	// ingest_enabled=false on a blacklisted row. Should be rare
	// (mostly fires when reconcile inserts a new row for a
	// blacklisted channel and the worker reaches it before
	// reconcile-side filtering).
	blacklisted prometheus.Counter

	// sweep_tier_total{tier=normal|deep}: one increment per SweepOne
	// invocation, labelled by the window tier. deep sweeps run the
	// wide (now-7d) history window once per DeepEvery (24h) per
	// channel so old thread roots with new replies re-surface for the
	// gated thread-root harvest; normal sweeps use the 30-min
	// overlap. The ratio should sit near (DeepEvery / sweep-cadence)
	// in steady state — a deep spike means a backlog of channels
	// crossing the cadence boundary at once.
	sweepTier *prometheus.CounterVec

	// sweep_duration_seconds: per-completed-sweep wall-clock
	// duration. Heatmap on the dashboard surfaces noisy outliers
	// (firehose-shaped channels).
	sweepDuration prometheus.Histogram

	// page_latency_seconds: per-API-call wall-clock duration
	// (after limiter, before-to-after the http roundtrip). p99
	// climbing while limiter_wait stays flat means Slack is slow,
	// not us.
	pageLatency prometheus.Histogram
}

func newSyncMetrics(reg prometheus.Registerer) *syncMetrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &syncMetrics{
		sweeps: regSyncCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_slack_sync_sweeps_total",
				Help: "SyncWorker sweep completions by outcome (ok|error|preempted).",
			}, []string{"outcome"})),
		pages: regSyncCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_slack_sync_pages_total",
				Help: "Successful conversations.history pages returned by Slack during SyncWorker sweeps.",
			})),
		messages: regSyncCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_slack_sync_messages_total",
				Help: "Per-message disposition out of the SyncWorker history pass (outcome=inserted|updated|nochange|skipped_subtype|parse_error, tier=normal|deep). {outcome=\"inserted\",tier=\"normal\"} is the direct socket-miss recovery signal.",
			}, []string{"outcome", "tier"})),
		claims: regSyncCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_slack_sync_claims_total",
				Help: "ClaimNext invocations that returned a row to sweep.",
			})),
		idleTicks: regSyncCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_slack_sync_idle_ticks_total",
				Help: "ClaimNext invocations that returned no eligible row (worker slept before retry).",
			})),
		blacklisted: regSyncCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_slack_sync_blacklisted_total",
				Help: "ClaimNext invocations that flipped ingest_enabled=false on a blacklisted row.",
			})),
		sweepTier: regSyncCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_slack_sync_sweep_tier_total",
				Help: "SyncWorker SweepOne invocations by window tier (normal|deep).",
			}, []string{"tier"})),
		sweepDuration: regSyncHistogram(reg, prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "diderot_slack_sync_sweep_duration_seconds",
				Help:    "Wall-clock duration of a SyncWorker sweep (claim → markDone or markError).",
				Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
			})),
		pageLatency: regSyncHistogram(reg, prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "diderot_slack_sync_page_latency_seconds",
				Help:    "Per-conversations.history-page wall-clock latency (after limiter Wait, around the API call).",
				Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
			})),
	}
	return m
}

// regSyncCounterVec / regSyncCounter / regSyncHistogram match the
// idempotent-register pattern. Suffixed because the package-wide
// helpers (regCounter, regTailCounter) already exist for adjacent
// metric shapes.
func regSyncCounterVec(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}

func regSyncCounter(reg prometheus.Registerer, v prometheus.Counter) prometheus.Counter {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(prometheus.Counter)
		}
		panic(err)
	}
	return v
}

func regSyncHistogram(reg prometheus.Registerer, v prometheus.Histogram) prometheus.Histogram {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(prometheus.Histogram)
		}
		panic(err)
	}
	return v
}
