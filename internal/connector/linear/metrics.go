package linear

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// syncMetrics holds all Prometheus instruments for the linear sync worker.
type syncMetrics struct {
	// Sync tick duration and count.
	syncDuration *prometheus.HistogramVec // {outcome: ok|err}

	// Upsert outcomes per write path.
	syncUpserts *prometheus.CounterVec // {outcome: inserted|updated|no_change|err}

	// Visibility sweep deletions.
	visibilityDeletions     *prometheus.CounterVec // {outcome: ok|err}
	visibilitySweepFailures prometheus.Counter

	// Visibility sweep pre-delete recheck: candidates spared because the
	// point-query re-verification found them still ingestible.
	visibilityFalsePositives prometheus.Counter

	// Guard refusals and guard-triggered deletions of previously-ingested events.
	guardRefusals  *prometheus.CounterVec // {reason: private_team|excluded_team}
	guardDeletions *prometheus.CounterVec // {outcome: ok|err}

	// GraphQL complexity consumed.
	complexityConsumed prometheus.Counter
}

type syncTimer struct {
	m     *syncMetrics
	start time.Time
}

func (t *syncTimer) done(outcome string) {
	t.m.syncDuration.WithLabelValues(outcome).Observe(time.Since(t.start).Seconds())
}

func (m *syncMetrics) startSyncTimer() *syncTimer {
	return &syncTimer{m: m, start: time.Now()}
}

func (m *syncMetrics) observeComplexity(ci ComplexityInfo) {
	if ci.RequestedTokens > 0 {
		m.complexityConsumed.Add(float64(ci.RequestedTokens))
	}
}

func newSyncMetrics(reg prometheus.Registerer) *syncMetrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	return &syncMetrics{
		syncDuration: regHistogramVec(reg, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "diderot_linear_sync_duration_seconds",
				Help:    "Duration of linear incremental sync ticks.",
				Buckets: prometheus.DefBuckets,
			}, []string{"outcome"})),

		syncUpserts: regCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_linear_sync_upserts_total",
				Help: "Linear issues upserted by outcome (inserted, updated, no_change, err).",
			}, []string{"outcome"})),

		visibilityDeletions: regCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_linear_visibility_deletions_total",
				Help: "Linear events deleted by the visibility sweep.",
			}, []string{"outcome"})),

		visibilitySweepFailures: regCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_linear_visibility_sweep_failures_total",
				Help: "Number of visibility sweep ticks that failed entirely. Alert on this — a stuck sweep widens the public→private exposure window.",
			})),

		visibilityFalsePositives: regCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_linear_visibility_false_positives_total",
				Help: "Delete candidates from the visibility sweep that the pre-delete point-query recheck found still ingestible (spared, not deleted). A nonzero count means the sweep's walk disagreed with a point query — worth investigating as a possible pagination-consistency bug, but no data was lost since the candidate was spared.",
			})),

		guardRefusals: regCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_linear_guard_refusals_total",
				Help: "Linear issues refused at ingest time by the privacy guard.",
			}, []string{"reason"})),

		guardDeletions: regCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_linear_guard_deletions_total",
				Help: "Previously-ingested linear events deleted because the issue is now ineligible (team went private or was excluded). A non-zero count means a team visibility change was caught by the incremental loop before the hourly visibility sweep.",
			}, []string{"outcome"})),

		complexityConsumed: regCounter(reg, prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "diderot_linear_graphql_complexity_total",
				Help: "Total GraphQL complexity points consumed by all linear API calls.",
			})),
	}
}

// Registration helpers that handle AlreadyRegistered gracefully (so tests
// can use prometheus.DefaultRegisterer across multiple test runs).

func regCounterVec(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}

func regCounter(reg prometheus.Registerer, v prometheus.Counter) prometheus.Counter {
	if err := reg.Register(v); err != nil {
		if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			return already.ExistingCollector.(prometheus.Counter)
		}
		panic(err)
	}
	return v
}

func regHistogramVec(reg prometheus.Registerer, v *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := reg.Register(v); err != nil {
		if already, ok := errors.AsType[prometheus.AlreadyRegisteredError](err); ok {
			return already.ExistingCollector.(*prometheus.HistogramVec)
		}
		panic(err)
	}
	return v
}
