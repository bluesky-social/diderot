package notion

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type countingObserver struct{ succeeded, failed int }

func (o *countingObserver) Succeeded() { o.succeeded++ }
func (o *countingObserver) Failed()    { o.failed++ }

// TestSyncWorkerTellsItsObserver: a search or walk tells the connector's
// metrics how it went; a partial walk counts as failed, and one cut short
// by shutdown says nothing.
func TestSyncWorkerTellsItsObserver(t *testing.T) {
	observer := &countingObserver{}
	w := &SyncWorker{Observer: observer}
	w.observe(nil)
	w.observe(&partialReconcileError{failed: 1, total: 2})
	w.observe(context.Canceled)
	w.observe(errors.New("notion: HTTP 500"))
	require.Equal(t, &countingObserver{succeeded: 1, failed: 2}, observer)
	require.NotPanics(t, func() { (&SyncWorker{}).observe(nil) })
}
