package embedder

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// workerMetrics tracks embedder throughput + latency + outcome.
// Granular-enough to distinguish "vLLM is slow" from "queue is
// empty" from "we're hitting the transient-retry path hard."
type workerMetrics struct {
	// diderot_embedder_batches_total{outcome}
	// outcome in {ok, transient_retry, terminal_err, poison}
	batches *prometheus.CounterVec
	// diderot_embedder_batch_duration_seconds — end-to-end for one
	// batch including any transient retries.
	batchDuration prometheus.Histogram
	// diderot_embedder_batch_size — distribution of rows per batch
	// (fetcher always pulls BatchSize but if the tail of the queue
	// has fewer rows we see partial batches).
	batchSize prometheus.Histogram
	// diderot_embedder_chunks_embedded_total — rows that landed in
	// qdrant + got embedded_at stamped. The ground-truth "work
	// done" counter.
	chunksEmbedded prometheus.Counter
	// diderot_embedder_poison_rows_total — rows that hit
	// failure_count >= 5 during this process' lifetime. Dashboard
	// alerts on a rising rate here (not at the absolute count,
	// which fluctuates as the reclaim SQL runs).
	poisonRows prometheus.Counter
	// diderot_embedder_queue_pending gauge — snapshot of
	// event_chunks rows with embedded_at IS NULL AND
	// failure_count < 5. Updated by the fetcher every iteration.
	queuePending prometheus.Gauge
	// diderot_embedder_queue_stranded gauge — snapshot of rows
	// with failure_count >= 5. Non-zero for more than a few minutes
	// means we have a real problem waiting for operator attention.
	queueStranded prometheus.Gauge
}

func newWorkerMetrics(reg prometheus.Registerer) *workerMetrics {
	return &workerMetrics{
		batches: regEmbCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_embedder_batches_total",
			Help: "Embed batches processed, labeled by outcome (ok/transient_retry/terminal_err/poison).",
		}, []string{"outcome"})),
		batchDuration: regEmbHistogram(reg, prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "diderot_embedder_batch_duration_seconds",
			Help:    "End-to-end latency of one embed batch (incl. transient retries + qdrant upsert + Postgres update).",
			Buckets: prometheus.ExponentialBucketsRange(0.05, 60, 12),
		})),
		batchSize: regEmbHistogram(reg, prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "diderot_embedder_batch_size",
			Help:    "Rows per embed batch actually sent to vLLM.",
			Buckets: []float64{1, 2, 4, 8, 16, 32, 64, 128},
		})),
		chunksEmbedded: regEmbCounter(reg, prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_embedder_chunks_embedded_total",
			Help: "Chunks successfully embedded + written to qdrant + stamped in Postgres.",
		})),
		poisonRows: regEmbCounter(reg, prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_embedder_poison_rows_total",
			Help: "Per-row embed failures that bumped failure_count to >= 5.",
		})),
		queuePending: regEmbGauge(reg, prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_embedder_queue_pending",
			Help: "Snapshot of event_chunks rows awaiting embed (NULL embedded_at, failure_count < 5).",
		})),
		queueStranded: regEmbGauge(reg, prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_embedder_queue_stranded",
			Help: "Snapshot of event_chunks rows stranded at failure_count >= 5. Zero = healthy.",
		})),
	}
}

type batchTimer struct {
	m     *workerMetrics
	start time.Time
}

func (m *workerMetrics) startBatch() *batchTimer {
	return &batchTimer{m: m, start: time.Now()}
}

func (t *batchTimer) done(outcome string) {
	t.m.batches.WithLabelValues(outcome).Inc()
	t.m.batchDuration.Observe(time.Since(t.start).Seconds())
}

func regEmbCounter(reg prometheus.Registerer, v prometheus.Counter) prometheus.Counter {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(prometheus.Counter)
		}
		panic(err)
	}
	return v
}

func regEmbCounterVec(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}

func regEmbHistogram(reg prometheus.Registerer, v prometheus.Histogram) prometheus.Histogram {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(prometheus.Histogram)
		}
		panic(err)
	}
	return v
}

func regEmbGauge(reg prometheus.Registerer, v prometheus.Gauge) prometheus.Gauge {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(prometheus.Gauge)
		}
		panic(err)
	}
	return v
}
