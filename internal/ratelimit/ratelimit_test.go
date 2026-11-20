package ratelimit

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestWaitPacesToTheRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bucket := New(rate.Every(time.Second), 2)
		start := time.Now()
		for range 4 {
			require.NoError(t, bucket.Wait(context.Background()))
		}
		// Two tokens from the full bucket, then one a second.
		require.Equal(t, 2*time.Second, time.Since(start))
	})
}

func TestPauseHoldsEveryWaitAndNeverShortens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bucket := New(rate.Inf, 1)
		start := time.Now()
		bucket.PauseUntil(start.Add(5 * time.Second))
		bucket.PauseUntil(start.Add(time.Second))
		require.Equal(t, start.Add(5*time.Second), bucket.PausedUntil())

		done := make(chan error, 2)
		for range 2 {
			go func() { done <- bucket.Wait(context.Background()) }()
		}
		time.Sleep(5*time.Second - time.Nanosecond)
		synctest.Wait()
		require.Empty(t, done, "a waiter went before the pause ended")
		time.Sleep(time.Nanosecond)
		require.NoError(t, <-done)
		require.NoError(t, <-done)
	})
}

func TestPauseDuringTokenWaitHoldsTheWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bucket := New(rate.Every(time.Second), 1)
		require.NoError(t, bucket.Wait(context.Background()))
		start := time.Now()
		done := make(chan error, 1)
		go func() { done <- bucket.Wait(context.Background()) }()
		synctest.Wait()
		// The waiter is blocked on the next token; a pause set now holds it
		// past the token.
		bucket.PauseUntil(start.Add(3 * time.Second))
		require.NoError(t, <-done)
		require.GreaterOrEqual(t, time.Since(start), 3*time.Second)
	})
}

func TestWaitReturnsWhenCancelledDuringAPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bucket := New(rate.Inf, 1)
		bucket.PauseUntil(time.Now().Add(time.Hour))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- bucket.Wait(ctx) }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestNeverPausedIsZero(t *testing.T) {
	require.True(t, New(rate.Inf, 1).PausedUntil().IsZero())
}
