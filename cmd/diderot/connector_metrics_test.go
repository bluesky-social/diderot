package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// workingNotes is notesSource with a Run that writes one note through its
// sink on a worker of its own, then gives up.
type workingNotes struct{ notesSource }

func (workingNotes) Run(ctx context.Context, deps source.Deps, _ settings.Resolved) error {
	sup := worker.NewSupervisor(deps.Logger, deps.Metrics)
	wrote := make(chan struct{})
	sup.AddRunner(worker.Runner{Name: "notes-scan", Interval: time.Hour, BootTick: true, Run: func(ctx context.Context) error {
		defer close(wrote)
		_, err := deps.Sink.Upsert(ctx, events.Event{
			Scope: events.ScopeInternal, Source: "note", ExternalID: "notes/metrics.txt", OccurredAt: time.Now().UTC(), Body: "counted",
		})
		return err
	}})
	scanCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		sup.Run(scanCtx)
		close(done)
	}()
	<-wrote
	stop()
	<-done
	return errors.New("the folder is gone")
}

// TestSourceRunCountsItsBaseMetrics: a source written only against the
// interface gets its base metrics, labelled by connector, from the deps a
// run is handed: what it writes, its workers' successes, and a run that
// ends with an error.
func TestSourceRunCountsItsBaseMetrics(t *testing.T) {
	pool := ingestTestPool(t, "cmd_connector_metrics")
	reg := prometheus.NewRegistry()
	metrics := settings.NewConnectorMetrics(reg).For("notes")
	runner := sourceRunner{Source: workingNotes{}, deps: source.Deps{
		Pool: pool, Sink: events.NewDBSink(pool, prometheus.NewRegistry()).Observed(metrics), Logger: discardLogger(), Metrics: metrics,
	}.For("notes")}
	before := time.Now().Unix()
	require.EqualError(t, runner.Run(t.Context(), settings.Resolved{}), "the folder is gone")

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP diderot_connector_errors_total Times a connector's work failed, or its run ended with an error.
# TYPE diderot_connector_errors_total counter
diderot_connector_errors_total{connector="notes"} 1
# HELP diderot_connector_events_written_total Events a connector's run inserted into the corpus or changed there.
# TYPE diderot_connector_events_written_total counter
diderot_connector_events_written_total{connector="notes"} 1
`), "diderot_connector_errors_total", "diderot_connector_events_written_total"))
	families, err := reg.Gather()
	require.NoError(t, err)
	var lastSuccess float64
	for _, family := range families {
		if family.GetName() == "diderot_connector_last_success_timestamp_seconds" {
			lastSuccess = family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	require.GreaterOrEqual(t, lastSuccess, float64(before), "the worker's tick succeeded")
}
