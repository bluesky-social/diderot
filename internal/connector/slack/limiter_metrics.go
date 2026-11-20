package slack

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// limiterMetrics is the Prometheus surface for the Slack-API
// budget governor.
//
//	diderot_slack_limiter_wait_seconds{method,tier}          histogram
//	diderot_slack_limiter_results_total{method,tier,result}  counter
//
// `method` cardinality is bounded by the slack-go web API methods we
// actually call (~12-15 distinct strings); `tier ∈ {tier1..tier4}`
// (a deterministic function of method, so it adds no real cardinality
// — it's a convenience label for per-tier rollups on the dashboard,
// e.g. "wait p99 for the whole Tier-3 bucket"); `result ∈
// {ok,429,other}`.
type limiterMetrics struct {
	wait    *prometheus.HistogramVec
	results *prometheus.CounterVec
}

func newLimiterMetrics(reg prometheus.Registerer) *limiterMetrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	return &limiterMetrics{
		wait: regLimiterHistogramVec(reg, prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "diderot_slack_limiter_wait_seconds",
				Help: "Time blocked inside Limiter.Wait, per method and tier. Saturation indicator.",
				// Buckets sized for: <1ms (no contention) up to 60s.
				Buckets: prometheus.ExponentialBuckets(0.0005, 4, 10),
			}, []string{"method", "tier"})),
		results: regLimiterCounterVec(reg, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "diderot_slack_limiter_results_total",
				Help: "Per-call result classification fed into the limiter (ok / 429 / other), labeled by slack-go method name and rate-limit tier.",
			}, []string{"method", "tier", "result"})),
	}
}

func (m *limiterMetrics) observeWait(method, tier string, d time.Duration) {
	m.wait.WithLabelValues(method, tier).Observe(d.Seconds())
}

func (m *limiterMetrics) recordResult(method, tier, result string) {
	m.results.WithLabelValues(method, tier, result).Inc()
}

// regLimiter*Vec follow the package's idempotent-register convention
// so a re-registered metric with the same name returns the existing
// collector instead of panicking.
func regLimiterCounterVec(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}

func regLimiterHistogramVec(reg prometheus.Registerer, v *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.HistogramVec)
		}
		panic(err)
	}
	return v
}
