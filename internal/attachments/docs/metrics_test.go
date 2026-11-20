package docs

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A leader that loses and regains leadership rebuilds its document worker in
// the same process; the second construction must share the first's
// collectors.
func TestNewWorkerMetricsReusesRegisteredCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	first := newWorkerMetrics(reg)
	second := newWorkerMetrics(reg)
	second.success.Inc()
	second.partial.Set(4)
	if got := testutil.ToFloat64(first.success); got != 1 {
		t.Fatalf("success = %v, want the re-registered counter to be shared", got)
	}
	if got := testutil.ToFloat64(first.partial); got != 4 {
		t.Fatalf("partial = %v, want the re-registered gauge to be shared", got)
	}
}
