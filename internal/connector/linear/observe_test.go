package linear

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type countingObserver struct{ succeeded, failed int }

func (o *countingObserver) Succeeded() { o.succeeded++ }
func (o *countingObserver) Failed()    { o.failed++ }

// TestSyncWorkerTellsItsObserver: a pass tells the connector's metrics how
// it went, and one cut short by shutdown says nothing.
func TestSyncWorkerTellsItsObserver(t *testing.T) {
	observer := &countingObserver{}
	w := &SyncWorker{Observer: observer}
	w.observe(nil)
	w.observe(errors.New("linear: HTTP 500"))
	w.observe(fmt.Errorf("incremental walk: %w", context.Canceled))
	require.Equal(t, &countingObserver{succeeded: 1, failed: 1}, observer)
	require.NotPanics(t, func() { (&SyncWorker{}).observe(nil) })
}
