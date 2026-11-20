package qdrantstore

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestNewMetricsMultipleStores verifies that constructing two Store
// metric blocks against the same registry works (a second Store would
// panic under plain promauto registration). Both reuse the existing
// collectors.
func TestNewMetricsMultipleStores(t *testing.T) {
	reg := prometheus.NewRegistry()
	m1 := newMetrics(reg, "coll_a")
	m2 := newMetrics(reg, "coll_b")

	// Both should have non-nil, compatible vectors that share the
	// underlying collector across instances.
	if m1.reqs != m2.reqs {
		t.Fatalf("expected same reqs CounterVec across Store metric instances")
	}
	if m1.latency != m2.latency {
		t.Fatalf("expected same latency HistogramVec across Store metric instances")
	}

	// Both should still populate distinct label series.
	m1.reqs.WithLabelValues("search", "coll_a", "ok").Inc()
	m2.reqs.WithLabelValues("search", "coll_b", "ok").Inc()
}
