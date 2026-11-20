package slack

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Per-path ingest observability metrics. The four paths are the
// distinct ways a Slack message reaches events.DBSink.Upsert:
//
//	socket      — live MessageEvent from socket-mode (Consumer)
//	sync        — channel sweep via SyncWorker
//	thread_sync — thread reply sweep via ThreadWorker
//	backfill    — operator-driven Backfill.Run (admin API)
//
// Ingest-lag (ingested_at - occurred_at) is tracked source-wide in
// `diderot_events_ingest_lag_seconds{source}` at the DBSink layer
// (internal/events/metrics.go). That metric covers all sources and
// is labeled by source type, not by which Slack ingest path was used
// — so it subsumes the old per-path lag histogram here.
const (
	ingestPathSocket     = "socket"
	ingestPathSync       = "sync"
	ingestPathThreadSync = "thread_sync"
	ingestPathBackfill   = "backfill"
)

// IngestMetrics is the per-path ingest observability surface. Built
// once per process and wired to both the Consumer (socket path) and
// the Backfill engine (sync / thread_sync / backfill paths).
// Exported so cmd/diderot's Slack source (slack_connector.go) can
// construct a single instance and pass it to both wirings.
type IngestMetrics = ingestMetrics

// NewIngestMetrics constructs a per-path ingest metrics handle.
// reg may be nil for tests that don't need a registered metric.
func NewIngestMetrics(reg prometheus.Registerer) *ingestMetrics {
	return newIngestMetrics(reg)
}

type ingestMetrics struct {
	total *prometheus.CounterVec
}

func newIngestMetrics(reg prometheus.Registerer) *ingestMetrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	return &ingestMetrics{
		total: regIngestCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_slack_ingest_total",
				Help: "Slack events successfully written to the sink, labeled by ingest path (socket/sync/thread_sync/backfill). Ingest lag is in diderot_events_ingest_lag_seconds{source=slack} at the sink layer.",
			}, []string{"path"})),
	}
}

// observe records a successful sink write on `path`. Increments the
// per-path counter. Source-level ingest lag is measured at the DBSink
// layer (diderot_events_ingest_lag_seconds{source=slack}) rather than
// here so it covers all sources uniformly — the per-path histogram was
// removed in favor of that metric.
func (m *ingestMetrics) observe(path string) {
	m.total.WithLabelValues(path).Inc()
}

func regIngestCounterVec(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}
