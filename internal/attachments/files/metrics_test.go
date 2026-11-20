package files

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A leader that loses and regains leadership rebuilds its caption worker in
// the same process; the second construction must share the first's
// collectors.
func TestNewWorkerMetricsReusesRegisteredCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	first := newWorkerMetrics(reg)
	second := newWorkerMetrics(reg)
	second.captionSuccess.Inc()
	second.captionSkipped.WithLabelValues("too_small").Inc()
	if got := testutil.ToFloat64(first.captionSuccess); got != 1 {
		t.Fatalf("caption success = %v, want the re-registered counter to be shared", got)
	}
	if got := testutil.ToFloat64(first.captionSkipped.WithLabelValues("too_small")); got != 1 {
		t.Fatalf("caption skipped = %v, want the re-registered vector to be shared", got)
	}
}
