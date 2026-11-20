package docs

import (
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// stageHistogramMaxSeconds is the top bucket for per-stage duration.
//
// Must reach perCallTimeout, or every slow extraction lands in +Inf —
// which is precisely where resolution matters, since the measured spread
// is 6.7 s/page mean and 11.3 s/page worst case and a large document can
// legitimately run for minutes. A test pins the relationship.
const stageHistogramMaxSeconds = 1200

// workerMetrics is the document-extraction worker's prometheus
// surface. Deliberately mirrors the caption worker's
// pending/stranded/success/failure/duration shape
// (internal/attachments/files/metrics.go) so dashboard panels can
// be copied rather than re-derived, with a diderot_doc_* prefix.
type workerMetrics struct {
	pending  prometheus.Gauge
	stranded prometheus.Gauge
	partial  prometheus.Gauge

	success   prometheus.Counter
	failures  prometheus.Counter
	transient prometheus.Counter

	chunksEmitted prometheus.Counter
	redactions    prometheus.Counter

	// rejected counts extractions that returned but failed a
	// degeneracy check, keyed by reason. These are settled, not
	// retried, and are a success-path outcome in the sense that the
	// pipeline handled them deliberately — but a rising repetition or
	// refusal rate is exactly what an operator wants alerted on, since
	// it means we're paying for extractions that produce nothing.
	rejected *prometheus.CounterVec

	duration *prometheus.HistogramVec
	stage    *prometheus.HistogramVec

	// ingestedToExtracted measures wall-clock from events.ingested_at
	// to a successful extraction commit, keyed by source.
	ingestedToExtracted *prometheus.HistogramVec

	mu          sync.Mutex
	lastGaugeAt time.Time
}

func newWorkerMetrics(reg prometheus.Registerer) *workerMetrics {
	m := &workerMetrics{
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_doc_extract_pending",
			Help: "event_documents rows awaiting extraction (extracted_at IS NULL, failure_count < 5).",
		}),
		stranded: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_doc_extract_stranded",
			Help: "event_documents rows past the extraction retry limit.",
		}),
		partial: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_doc_extract_partial",
			Help: "Settled documents with usable indexed text but incomplete extraction coverage.",
		}),
		success: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_doc_extract_success_total",
			Help: "Documents successfully extracted (commit landed).",
		}),
		failures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_doc_extract_failures_total",
			Help: "Failed extraction attempts (each bumps failure_count).",
		}),
		transient: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_doc_extract_transient_total",
			Help: "Transient extraction failures — sleep+retry, no failure_count bump.",
		}),
		chunksEmitted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_doc_chunks_emitted_total",
			Help: "document_text event_chunks rows emitted (N per document, page-level).",
		}),
		redactions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_doc_extract_redactions_total",
			Help: "Secrets detected and redacted from extracted document text before chunks landed.",
		}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_doc_extract_rejected_total",
			Help: "Extractions settled without chunks, labeled by reason.",
		}, []string{"reason"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_doc_extract_duration_seconds",
			Help:    "Per-document extraction pipeline duration (fetch + S3 put + model + commit).",
			Buckets: prometheus.ExponentialBucketsRange(1, 300, 8),
		}, []string{"outcome"}),
		stage: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_doc_extract_stage_seconds",
			Help:    "Per-document pipeline stage duration. Labels: stage (fetch|s3_put|extract|commit), outcome (ok|err).",
			Buckets: prometheus.ExponentialBucketsRange(0.05, stageHistogramMaxSeconds, 10),
		}, []string{"stage", "outcome"}),
		ingestedToExtracted: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_doc_ingested_to_extracted_seconds",
			Help:    "Wall-clock latency from events.ingested_at to successful extraction commit.",
			Buckets: prometheus.ExponentialBucketsRange(1, 86400, 12),
		}, []string{"source"}),
	}
	if reg != nil {
		for _, gauge := range []*prometheus.Gauge{&m.pending, &m.stranded, &m.partial} {
			registerOrReuse(reg, gauge)
		}
		for _, counter := range []*prometheus.Counter{
			&m.success, &m.failures, &m.transient, &m.chunksEmitted, &m.redactions,
		} {
			registerOrReuse(reg, counter)
		}
		registerOrReuse(reg, &m.rejected)
		for _, histogram := range []**prometheus.HistogramVec{&m.duration, &m.stage, &m.ingestedToExtracted} {
			registerOrReuse(reg, histogram)
		}
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

func (m *workerMetrics) shouldUpdateGauges() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Since(m.lastGaugeAt) < 5*time.Second {
		return false
	}
	m.lastGaugeAt = time.Now()
	return true
}

// stageTimer scopes one end-to-end duration sample to an outcome
// label. Use as: t := m.startTimer(); t.done("ok").
type stageTimer struct {
	start time.Time
	hv    *prometheus.HistogramVec
}

func (t stageTimer) done(outcome string) {
	t.hv.WithLabelValues(outcome).Observe(time.Since(t.start).Seconds())
}

func (m *workerMetrics) startTimer() stageTimer {
	return stageTimer{start: time.Now(), hv: m.duration}
}

// observeStage records one stage's duration. since is time.Now()
// captured at the start of that stage.
func (m *workerMetrics) observeStage(stage, outcome string, since time.Time) {
	m.stage.WithLabelValues(stage, outcome).Observe(time.Since(since).Seconds())
}
