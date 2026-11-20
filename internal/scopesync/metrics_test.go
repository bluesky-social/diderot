package scopesync

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
	second.deleteApplied.Add(2)
	second.unstamped.Set(5)
	second.scopeBatchSize.Observe(10)
	if got := testutil.ToFloat64(first.deleteApplied); got != 2 {
		t.Fatalf("delete applied = %v, want the re-registered counter to be shared", got)
	}
	if got := testutil.ToFloat64(first.unstamped); got != 5 {
		t.Fatalf("unstamped = %v, want the re-registered gauge to be shared", got)
	}
	if got := testutil.CollectAndCount(reg, "diderot_scope_worker_batch_size"); got != 1 {
		t.Fatalf("batch size histogram series = %d, want one shared histogram", got)
	}
}
