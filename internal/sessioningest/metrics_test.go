package sessioningest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A leader that loses and regains leadership rebuilds its workers in the
// same process; the second construction must share the first's collectors.
func TestNewMetricsReusesRegisteredCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	first := NewMetrics(reg)
	second := NewMetrics(reg)
	second.ChangesTotal.Inc()
	second.PendingSessions.Set(3)
	if got := testutil.ToFloat64(first.ChangesTotal); got != 1 {
		t.Fatalf("changes total = %v, want the re-registered counter to be shared", got)
	}
	if got := testutil.ToFloat64(first.PendingSessions); got != 3 {
		t.Fatalf("pending sessions = %v, want the re-registered gauge to be shared", got)
	}
}
