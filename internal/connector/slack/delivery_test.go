package slack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeliveryWithinEndsWithItsRun: a use bounded by a delivery ends when
// either its own context or the lending run's does, and a delivery says
// whether its run is still up.
func TestDeliveryWithinEndsWithItsRun(t *testing.T) {
	run, stop := context.WithCancel(context.Background())
	delivery := Delivery{Run: run}
	require.True(t, delivery.Running())

	use, done := delivery.Within(context.Background())
	defer done()
	require.NoError(t, use.Err())
	stop()
	<-use.Done()
	require.False(t, delivery.Running())

	caller, cancelCaller := context.WithCancel(context.Background())
	use, done = Delivery{Run: context.Background()}.Within(caller)
	defer done()
	cancelCaller()
	<-use.Done()

	require.True(t, Delivery{}.Running(), "a delivery without a run context is up")
	use, done = Delivery{}.Within(context.Background())
	done()
	require.ErrorIs(t, use.Err(), context.Canceled)
}
