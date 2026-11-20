package qdrantstore

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics wraps the shared prometheus vectors with a fixed collection
// label for this Store's per-op observations.
//
// The vectors themselves are per-Registerer: promauto would panic on
// double-registration if two Store instances targeted the same
// registry (very much the intended deploy shape once more embedders
// land). newMetrics therefore registers each vector via
// reg.Register(...), and treats AlreadyRegistered as a reuse signal
// (pulling the existing collector back out of the error).
type metrics struct {
	collection string
	reqs       *prometheus.CounterVec
	latency    *prometheus.HistogramVec
}

func newMetrics(reg prometheus.Registerer, collection string) *metrics {
	reqs := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "diderot_qdrant_requests_total",
		Help: "Qdrant client calls, labeled by op, collection, and outcome (ok/err).",
	}, []string{"op", "collection", "outcome"})
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "diderot_qdrant_request_duration_seconds",
		Help:    "Qdrant client call latency by op and collection.",
		Buckets: prometheus.ExponentialBucketsRange(0.001, 5, 12),
	}, []string{"op", "collection"})

	return &metrics{
		collection: collection,
		reqs:       registerOrReuseCounter(reg, reqs),
		latency:    registerOrReuseHistogram(reg, latency),
	}
}

// registerOrReuseCounter registers v with reg; if the registry already
// has a collector with the same fqName + labels, reuses that one.
// Any other registration error panics — it's a programming bug
// (incompatible redefinition of the same metric) that should surface
// loudly, not get swallowed.
func registerOrReuseCounter(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}

func registerOrReuseHistogram(reg prometheus.Registerer, v *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.HistogramVec)
		}
		panic(err)
	}
	return v
}

type timer struct {
	m     *metrics
	op    string
	start time.Time
}

func (m *metrics) startTimer(op string) *timer {
	return &timer{m: m, op: op, start: time.Now()}
}

func (t *timer) done(err error) {
	outcome := "ok"
	if err != nil {
		outcome = "err"
	}
	t.m.reqs.WithLabelValues(t.op, t.m.collection, outcome).Inc()
	t.m.latency.WithLabelValues(t.op, t.m.collection).Observe(time.Since(t.start).Seconds())
}
