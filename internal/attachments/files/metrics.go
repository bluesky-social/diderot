package files

import (
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// workerMetrics is the caption worker's prometheus surface.
// Mirrors the embedder's pending/stranded/success/failure/duration
// shape so dashboards can reuse the panel patterns. Metric names
// use the source-agnostic diderot_image_* prefix — the table
// (event_images) and pipeline are source-agnostic too, so the
// metric prefix matches.
type workerMetrics struct {
	captionPending  prometheus.Gauge
	captionStranded prometheus.Gauge

	captionSuccess   prometheus.Counter
	captionFailures  prometheus.Counter
	captionTransient prometheus.Counter

	chunksEmitted prometheus.Counter

	// captionRedactions counts individual secrets redacted from
	// caption text before the chunk was written (per-secret, not
	// per-caption — a caption with 3 leaked tokens increments this
	// by 3). See redact.go.
	captionRedactions prometheus.Counter

	// captionSkipped counts rows stamped captioned_at without a VLM
	// call because captionOne pre-filtered them. Labels: reason
	// (unsupported_format | too_small). Success path, not a failure
	// — these never bump failure_count.
	captionSkipped *prometheus.CounterVec

	captionDuration *prometheus.HistogramVec

	// captionStage records the duration of each sub-stage of the
	// caption pipeline: fetch, s3_put, vlm, commit. Labels are
	// {stage, outcome} where outcome is "ok" or "err". Complements
	// captionDuration (the end-to-end timer): the stage histogram
	// lets operators see which stage is slow without disabling the
	// total. Not observed for short-circuits that never enter the
	// stage sequence.
	captionStage *prometheus.HistogramVec

	// ingestedToCaptioned measures the wall-clock latency from
	// events.ingested_at to the moment the caption commits — the
	// elapsed time the ask caption barrier's 20s deadline bounds.
	// Observed only on a successful commitCaption, keyed by source.
	ingestedToCaptioned *prometheus.HistogramVec

	// Throttle for queue-depth COUNT(*)s. Updates at most every 5s
	// — enough resolution to see a rapidly-growing queue, cheap
	// enough to not pile up COUNT scans.
	mu          sync.Mutex
	lastGaugeAt time.Time
}

func newWorkerMetrics(reg prometheus.Registerer) *workerMetrics {
	m := &workerMetrics{
		captionPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_image_caption_pending",
			Help: "event_images rows awaiting caption (captioned_at IS NULL, failure_count < 5).",
		}),
		captionStranded: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_image_caption_stranded",
			Help: "event_images rows past the caption failure budget.",
		}),
		captionSuccess: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_image_caption_success_total",
			Help: "Images successfully captioned (commit landed).",
		}),
		captionFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_image_caption_failures_total",
			Help: "Failed caption attempts (each bumps failure_count).",
		}),
		captionTransient: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_image_caption_transient_total",
			Help: "Transient caption failures — sleep+retry, no failure_count bump.",
		}),
		chunksEmitted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_image_chunks_emitted_total",
			Help: "image_caption event_chunks rows emitted by the worker (one per captioned image).",
		}),
		captionRedactions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "diderot_image_caption_redactions_total",
			Help: "Secrets detected and redacted from image caption output before the chunk landed.",
		}),
		captionSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_image_caption_skipped_total",
			Help: "Images skipped before a VLM call, stamped captioned_at with no caption text. Labels: reason (unsupported_format|too_small).",
		}, []string{"reason"}),
		captionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_image_caption_duration_seconds",
			Help:    "Per-image caption pipeline duration (fetch + S3 put + VLM + commit).",
			Buckets: prometheus.ExponentialBucketsRange(0.5, 120, 8),
		}, []string{"outcome"}),
		captionStage: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_image_caption_stage_seconds",
			Help:    "Per-image caption pipeline stage duration. Labels: stage (fetch|s3_put|vlm|commit), outcome (ok|err). Use this to identify which stage is slow; the end-to-end total is diderot_image_caption_duration_seconds.",
			Buckets: prometheus.ExponentialBucketsRange(0.05, 60, 8),
		}, []string{"stage", "outcome"}),
		ingestedToCaptioned: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "diderot_image_ingested_to_captioned_seconds",
			Help:    "Wall-clock latency from events.ingested_at to successful caption commit. This is the total elapsed time the ask caption barrier's 20s deadline bounds.",
			Buckets: prometheus.ExponentialBucketsRange(0.5, 300, 10),
		}, []string{"source"}),
	}
	if reg != nil {
		registerOrReuse(reg, &m.captionPending)
		registerOrReuse(reg, &m.captionStranded)
		for _, counter := range []*prometheus.Counter{
			&m.captionSuccess, &m.captionFailures, &m.captionTransient,
			&m.chunksEmitted, &m.captionRedactions,
		} {
			registerOrReuse(reg, counter)
		}
		registerOrReuse(reg, &m.captionSkipped)
		for _, histogram := range []**prometheus.HistogramVec{
			&m.captionDuration, &m.captionStage, &m.ingestedToCaptioned,
		} {
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

// stageTimer scopes a per-iteration duration sample to a single
// outcome label. Use as: t := m.startCaptionTimer(); t.done("ok").
type stageTimer struct {
	start time.Time
	hv    *prometheus.HistogramVec
}

func (t stageTimer) done(outcome string) {
	t.hv.WithLabelValues(outcome).Observe(time.Since(t.start).Seconds())
}

func (m *workerMetrics) startCaptionTimer() stageTimer {
	return stageTimer{start: time.Now(), hv: m.captionDuration}
}

// observeStage records one stage's duration. stage is the pipeline
// step name (fetch, s3_put, vlm, commit); outcome is "ok" or "err".
// since is time.Now() captured at the start of the stage.
func (m *workerMetrics) observeStage(stage, outcome string, since time.Time) {
	m.captionStage.WithLabelValues(stage, outcome).Observe(time.Since(since).Seconds())
}
