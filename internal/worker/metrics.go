package worker

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// prometheusRegisterer aliases the prometheus interface so callers
// (and tests) can pass nil without importing prometheus just to spell
// the type. Mirrors embedder.Worker.Registry: nil is a valid value
// meaning "register into a throwaway registry."
type prometheusRegisterer = prometheus.Registerer

// metrics holds the per-worker liveness signals. Labeled by worker
// name so one Supervisor's workers share a metric family and Grafana
// can alert per-worker.
//
// The headline signal is lastSuccess: a unix-timestamp gauge per
// worker, so an alert like `time() - diderot_worker_last_success_timestamp_seconds{worker="embedder"} > 900`
// fires when a worker stops succeeding — the durable replacement for
// noticing a stalled worker by eye (the embedder-stall anti-pattern
// in the repo CLAUDE.md). runsTotal{worker,outcome} gives the rate
// view (success vs error) alongside it.
type metrics struct {
	// diderot_worker_runs_total{worker,outcome} — outcome in
	// {success, error}. For a Runner this counts per-tick; for a Func
	// it counts at most once (an early/errored return).
	runsTotal *prometheus.CounterVec
	// diderot_worker_last_success_timestamp_seconds{worker} — unix
	// time of the last successful run. Never set for a long-running
	// Func that hasn't returned (its success is "still running",
	// observable via runsTotal staying flat with no error). Set on
	// every successful Runner tick.
	lastSuccess *prometheus.GaugeVec
	// observer is also told each run's outcome, when the registerer is one.
	observer Observer
}

// Observer is told how a connector's work goes, for its base metrics: a
// connector's metrics registerer is one (settings.Metrics), so a supervisor
// built with it tells it every run's outcome.
type Observer interface {
	Succeeded()
	Failed()
}

func newMetrics(reg prometheus.Registerer) *metrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	observer, _ := reg.(Observer)
	return &metrics{
		observer: observer,
		runsTotal: regCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_worker_runs_total",
			Help: "Worker runs by name and outcome (success/error). Runners count per tick; long-running funcs count an early/errored return.",
		}, []string{"worker", "outcome"})),
		lastSuccess: regGaugeVec(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "diderot_worker_last_success_timestamp_seconds",
			Help: "Unix timestamp of a worker's last successful run. Alert on staleness to catch a silently-stalled worker.",
		}, []string{"worker"})),
	}
}

func (m *metrics) recordSuccess(name string) {
	m.runsTotal.WithLabelValues(name, "success").Inc()
	m.lastSuccess.WithLabelValues(name).Set(float64(time.Now().Unix()))
	if m.observer != nil {
		m.observer.Succeeded()
	}
}

func (m *metrics) recordError(name string) {
	m.runsTotal.WithLabelValues(name, "error").Inc()
	if m.observer != nil {
		m.observer.Failed()
	}
}

// regCounterVec / regGaugeVec register a collector, tolerating a
// duplicate registration by returning the already-registered
// collector. Matches the embedder package's helpers: lets two
// Supervisors (or a re-elected leader rebuilding its workers) share
// one metric family on prometheus.DefaultRegisterer without panicking.
func regCounterVec(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}

func regGaugeVec(reg prometheus.Registerer, v *prometheus.GaugeVec) *prometheus.GaugeVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.GaugeVec)
		}
		panic(err)
	}
	return v
}
