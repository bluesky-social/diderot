package sessioningest

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the prometheus surface. Namespace matches diderot's
// convention (diderot_sessioningest_*).
type Metrics struct {
	ChangesTotal            prometheus.Counter
	RevisionsFetched        prometheus.Counter
	RevisionsPublished      prometheus.Counter
	SummaryReusesTotal      prometheus.Counter
	PendingSessions         prometheus.Gauge
	BackoffSessions         prometheus.Gauge
	QuarantinedSessions     prometheus.Gauge
	LastDiscoverySuccess    prometheus.Gauge
	IndexErrorsTotal        prometheus.Counter
	FeedErrorsTotal         prometheus.Counter
	FeedChangesSkippedTotal *prometheus.CounterVec
}

// NewMetrics builds a Metrics that registers into reg. reg may be nil
// (tests) — the metrics go to a throwaway registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	m := &Metrics{
		ChangesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_sessioningest_changes_total",
			Help: "Session change rows durably drained from the session feed's change feed.",
		}),
		RevisionsFetched: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_sessioningest_revisions_fetched_total",
			Help: "Sanitized Bsky Session v1 revisions fetched from the session feed.",
		}),
		RevisionsPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_sessioningest_revisions_published_total",
			Help: "Revisions canonically published to the corpus (first-time or replacement).",
		}),
		SummaryReusesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_sessioningest_summary_reuses_total",
			Help: "Session revisions published by reusing an exact summary-input digest without inference.",
		}),
		PendingSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_sessioningest_pending_sessions",
			Help: "Genuinely-pending candidate sessions observed but not yet published (excludes quarantined).",
		}),
		BackoffSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_sessioningest_backoff_sessions",
			Help: "Pending sessions whose current revision failed indexing and is waiting out its retry backoff.",
		}),
		QuarantinedSessions: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_sessioningest_quarantined_sessions",
			Help: "Sessions whose current revision failed permanently (e.g. over the byte cap) and is parked until a new revision or manual release.",
		}),
		LastDiscoverySuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_sessioningest_last_tick_success_seconds",
			Help: "Unix timestamp of the last successful session-change discovery pass. Alert on staleness.",
		}),
		IndexErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_sessioningest_index_errors_total",
			Help: "Per-session revision publish/validation failures.",
		}),
		FeedErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_sessioningest_feed_errors_total",
			Help: "Change-feed drain failures.",
		}),
		FeedChangesSkippedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_sessioningest_feed_changes_skipped_total",
			Help: "Feed-page changes skipped because of a deterministic, permanent per-change refusal (writer/format binding conflict, invalid decided visibility scope, or a change whose own data failed validation before reaching the database); the rest of the page still committed and the cursor still advanced. See the reason label (skipReason) for which.",
		}, []string{"reason"}),
	}
	for _, counter := range []*prometheus.Counter{
		&m.ChangesTotal, &m.RevisionsFetched, &m.RevisionsPublished, &m.SummaryReusesTotal,
		&m.IndexErrorsTotal, &m.FeedErrorsTotal,
	} {
		registerOrReuse(reg, counter)
	}
	registerOrReuse(reg, &m.FeedChangesSkippedTotal)
	for _, gauge := range []*prometheus.Gauge{
		&m.PendingSessions, &m.BackoffSessions, &m.QuarantinedSessions, &m.LastDiscoverySuccess,
	} {
		registerOrReuse(reg, gauge)
	}
	return m
}

// registerOrReuse registers *c, or points it at the collector already
// registered under the same name: a leader that loses and regains
// leadership rebuilds its workers, and with them these metrics, in the
// same process.
func registerOrReuse[T prometheus.Collector](reg prometheus.Registerer, c *T) {
	if err := reg.Register(*c); err != nil {
		var already prometheus.AlreadyRegisteredError
		if !errors.As(err, &already) {
			panic(err)
		}
		*c = already.ExistingCollector.(T)
	}
}
