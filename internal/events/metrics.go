package events

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type sinkMetrics struct {
	upsert         *prometheus.CounterVec   // diderot_events_upsert_total{source, outcome}
	upsertDur      *prometheus.HistogramVec // diderot_events_upsert_duration_seconds{source}
	delete         *prometheus.CounterVec   // diderot_events_delete_total{source, outcome}
	deleteDur      *prometheus.HistogramVec // diderot_events_delete_duration_seconds{source}
	chunksInserted *prometheus.CounterVec   // diderot_events_chunks_inserted_total{source}
	chunksDeleted  *prometheus.CounterVec   // diderot_events_chunks_deleted_total{source, reason}

	// ingestLag measures ingested_at - occurred_at for genuine INSERT
	// operations: how stale an event was when it first arrived. Not
	// observed on updates or no-ops because ingested_at is immutable after
	// the initial write and the lag is only
	// meaningful on the first sighting of a (source, external_id) pair.
	ingestLag *prometheus.HistogramVec // diderot_events_ingest_lag_seconds{source}
}

func newSinkMetrics(reg prometheus.Registerer) *sinkMetrics {
	return &sinkMetrics{
		upsert: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_events_upsert_total",
			Help: "Event upsert attempts labeled by source and outcome (ok/err/invalid).",
		}, []string{"source", "outcome"})),
		upsertDur: regHistogram(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_events_upsert_duration_seconds",
			Help:    "Time to commit an event upsert, including durable point-delete queueing.",
			Buckets: prometheus.ExponentialBucketsRange(0.001, 5, 12),
		}, []string{"source"})),
		delete: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_events_delete_total",
			Help: "Event tombstone attempts labeled by source and outcome.",
		}, []string{"source", "outcome"})),
		deleteDur: regHistogram(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_events_delete_duration_seconds",
			Help:    "Time to commit an event tombstone.",
			Buckets: prometheus.ExponentialBucketsRange(0.001, 5, 12),
		}, []string{"source"})),
		chunksInserted: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_events_chunks_inserted_total",
			Help: "Event chunk rows inserted (sum of chunks-per-event across all upserts).",
		}, []string{"source"})),
		chunksDeleted: regCounter(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_events_chunks_deleted_total",
			Help: "Event chunk rows deleted during edit/tombstone, labeled by reason.",
		}, []string{"source", "reason"})),
		ingestLag: regHistogram(reg, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_events_ingest_lag_seconds",
			Help:    "Difference between ingested_at and occurred_at on genuine INSERT operations — how stale the event was when it first arrived. Not observed on updates or no-ops (ingested_at is immutable after initial write).",
			Buckets: prometheus.ExponentialBucketsRange(0.5, 300, 10),
		}, []string{"source"})),
	}
}

type upsertTimer struct {
	m      *sinkMetrics
	source string
	start  time.Time
}

func (m *sinkMetrics) startUpsertTimer(source string) *upsertTimer {
	return &upsertTimer{m: m, source: source, start: time.Now()}
}

func (t *upsertTimer) done(outcome string) {
	t.m.upsert.WithLabelValues(t.source, outcome).Inc()
	t.m.upsertDur.WithLabelValues(t.source).Observe(time.Since(t.start).Seconds())
}

type deleteTimer struct {
	m      *sinkMetrics
	source string
	start  time.Time
}

func (m *sinkMetrics) startDeleteTimer(source string) *deleteTimer {
	return &deleteTimer{m: m, source: source, start: time.Now()}
}

func (t *deleteTimer) done(outcome string) {
	t.m.delete.WithLabelValues(t.source, outcome).Inc()
	t.m.deleteDur.WithLabelValues(t.source).Observe(time.Since(t.start).Seconds())
}

func regCounter(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}

func regHistogram(reg prometheus.Registerer, v *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.HistogramVec)
		}
		panic(err)
	}
	return v
}
