package settings

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestConnectorMetricsAreLabelledByConnector: each connector's Metrics count
// its own base metrics, a later leader term shares the registered ones, and
// the zero Metrics count nothing.
func TestConnectorMetricsAreLabelledByConnector(t *testing.T) {
	reg := prometheus.NewRegistry()
	first := NewConnectorMetrics(reg)
	notes := first.For("notes")
	notes.Wrote()
	notes.Wrote()
	notes.Failed()
	before := time.Now().Unix()
	NewConnectorMetrics(reg).For("pages").Succeeded()

	require.Equal(t, 2.0, testutil.ToFloat64(first.written.WithLabelValues("notes")))
	require.Equal(t, 1.0, testutil.ToFloat64(first.errors.WithLabelValues("notes")))
	require.Zero(t, testutil.ToFloat64(first.errors.WithLabelValues("pages")))
	require.GreaterOrEqual(t, testutil.ToFloat64(first.lastSuccess.WithLabelValues("pages")), float64(before),
		"a later term's metrics are the same series")

	require.NoError(t, notes.Register(prometheus.NewCounter(prometheus.CounterOpts{Name: "notes_own_total", Help: "A source's own metric."})))
	families, err := reg.Gather()
	require.NoError(t, err)
	var names []string
	for _, family := range families {
		names = append(names, family.GetName())
	}
	require.Contains(t, names, "notes_own_total", "a connector's own metrics register where its base ones do")

	feed := NewConnectorMetrics(reg).For("feed")
	require.Equal(t, 1, testutil.CollectAndCount(first.lastSuccess), "a connector that never ran has no series")
	require.Equal(t, 1, testutil.CollectAndCount(first.written))
	feed.Started()
	feed.Failed()
	require.Equal(t, 2, testutil.CollectAndCount(first.lastSuccess),
		"a connector that runs but never succeeds has a last success, of zero")
	require.Equal(t, 2, testutil.CollectAndCount(first.written))
	require.Zero(t, testutil.ToFloat64(first.lastSuccess.WithLabelValues("feed")))
	notes.Started()
	require.Equal(t, 2.0, testutil.ToFloat64(first.written.WithLabelValues("notes")), "starting again keeps the counts")

	var zero Metrics
	require.NotPanics(t, func() {
		zero.Started()
		zero.Wrote()
		zero.Succeeded()
		zero.Failed()
		require.NoError(t, zero.Register(prometheus.NewCounter(prometheus.CounterOpts{Name: "throwaway_total", Help: "x"})))
	})
}
