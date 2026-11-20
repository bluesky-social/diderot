package settings

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLive(t *testing.T) {
	var live Live[string]
	_, ok := live.Get()
	require.False(t, ok, "the zero value holds nothing")

	live.Set("first")
	got, ok := live.Get()
	require.True(t, ok)
	require.Equal(t, "first", got)

	live.Set("second")
	got, _ = live.Get()
	require.Equal(t, "second", got, "a later Set replaces the value")

	live.Clear()
	got, ok = live.Get()
	require.False(t, ok)
	require.Empty(t, got, "a cleared handle returns the zero value")
}

func TestLiveConcurrentUse(t *testing.T) {
	var live Live[int]
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 100 {
				live.Set(i)
				live.Get()
				live.With(func(int) {})
				live.Clear()
			}
		})
	}
	wg.Wait()
}

// TestLiveClearWaitsForUses: a use in With holds the value lent, so the
// Clear that ends the lend returns only after the use, which finishes; a use
// that starts after Clear finds nothing, even inside another use.
func TestLiveClearWaitsForUses(t *testing.T) {
	var live Live[string]
	require.False(t, live.With(func(string) { t.Fatal("nothing is lent") }))

	live.Set("run")
	using, release := make(chan struct{}), make(chan struct{})
	var used string
	usesDone := make(chan bool)
	go func() {
		usesDone <- live.With(func(value string) {
			close(using)
			<-release
			used = value
		})
	}()
	<-using
	cleared := make(chan struct{})
	go func() {
		live.Clear()
		close(cleared)
	}()
	require.Eventually(t, func() bool {
		_, ok := live.Get()
		return !ok
	}, time.Second, time.Millisecond, "Clear takes the value back at once")
	require.False(t, live.With(func(string) { t.Fatal("a use after Clear finds nothing") }))
	select {
	case <-cleared:
		t.Fatal("Clear returned while a use held the value")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	require.True(t, <-usesDone)
	<-cleared
	require.Equal(t, "run", used, "the use in progress finishes")
}
