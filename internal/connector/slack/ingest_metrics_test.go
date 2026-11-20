package slack

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestIngestMetricsObserveIncrementsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newIngestMetrics(reg)
	m.observe(ingestPathSocket)
	m.observe(ingestPathSocket)
	m.observe(ingestPathBackfill)

	got := testutil.ToFloat64(m.total.WithLabelValues(ingestPathSocket))
	if got != 2 {
		t.Errorf("socket count = %v, want 2", got)
	}
	got = testutil.ToFloat64(m.total.WithLabelValues(ingestPathBackfill))
	if got != 1 {
		t.Errorf("backfill count = %v, want 1", got)
	}
}
