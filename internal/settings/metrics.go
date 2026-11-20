package settings

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ConnectorMetrics are the base metrics every source and automation has,
// labelled by connector:
//
//	diderot_connector_events_written_total{connector}
//	diderot_connector_last_success_timestamp_seconds{connector}
//	diderot_connector_errors_total{connector}
type ConnectorMetrics struct {
	reg         prometheus.Registerer
	written     *prometheus.CounterVec
	lastSuccess *prometheus.GaugeVec
	errors      *prometheus.CounterVec
}

// NewConnectorMetrics registers the base metrics on reg, or finds them
// there when an earlier leader term registered them.
func NewConnectorMetrics(reg prometheus.Registerer) *ConnectorMetrics {
	return &ConnectorMetrics{
		reg: reg,
		written: register(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_connector_events_written_total",
			Help: "Events a connector's run inserted into the corpus or changed there.",
		}, []string{"connector"})),
		lastSuccess: register(reg, prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "diderot_connector_last_success_timestamp_seconds",
			Help: "Unix time a connector's work last finished without an error. Alert on staleness to catch a connector that stopped working.",
		}, []string{"connector"})),
		errors: register(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_connector_errors_total",
			Help: "Times a connector's work failed, or its run ended with an error.",
		}, []string{"connector"})),
	}
}

func register[C prometheus.Collector](reg prometheus.Registerer, collector C) C {
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(C)
		}
		panic(err)
	}
	return collector
}

// For returns the named connector's Metrics.
func (m *ConnectorMetrics) For(name string) Metrics {
	return Metrics{reg: m.reg, connector: name, base: m}
}

// Metrics registers a connector's own metrics, and counts its base ones. A
// worker supervisor built with it counts its workers' runs, and a sink
// observed by it counts the events it writes, so a connector counts them by
// using both. The zero value registers into a throwaway registry and counts
// nothing.
type Metrics struct {
	reg       prometheus.Registerer
	connector string
	base      *ConnectorMetrics
}

func (m Metrics) registerer() prometheus.Registerer {
	if m.reg == nil {
		return prometheus.NewRegistry()
	}
	return m.reg
}

func (m Metrics) Register(c prometheus.Collector) error   { return m.registerer().Register(c) }
func (m Metrics) MustRegister(cs ...prometheus.Collector) { m.registerer().MustRegister(cs...) }
func (m Metrics) Unregister(c prometheus.Collector) bool  { return m.registerer().Unregister(c) }

// Started makes the connector's base series exist, at zero until its work
// counts in them, as its run starts: an alert on a stale last success then
// sees a running connector that has never succeeded, and no series exists
// for a connector that never ran.
func (m Metrics) Started() {
	if m.base != nil {
		m.base.written.WithLabelValues(m.connector)
		m.base.lastSuccess.WithLabelValues(m.connector)
		m.base.errors.WithLabelValues(m.connector)
	}
}

// Wrote counts an event the connector inserted or changed.
func (m Metrics) Wrote() {
	if m.base != nil {
		m.base.written.WithLabelValues(m.connector).Inc()
	}
}

// Succeeded records that some of the connector's work finished without an
// error.
func (m Metrics) Succeeded() {
	if m.base != nil {
		m.base.lastSuccess.WithLabelValues(m.connector).Set(float64(time.Now().Unix()))
	}
}

// Failed counts some of the connector's work that failed.
func (m Metrics) Failed() {
	if m.base != nil {
		m.base.errors.WithLabelValues(m.connector).Inc()
	}
}
