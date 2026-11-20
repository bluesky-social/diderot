package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

type stubImageFetcher struct{}

func (stubImageFetcher) FetchImage(context.Context, string, string) ([]byte, string, error) {
	return []byte("png"), "image/png", nil
}

// TestLentImagesFailTransientlyWhileNothingIsLent: an image claimed just
// before its connector stopped waits for the connector instead of spending
// its failure budget.
func TestLentImagesFailTransientlyWhileNothingIsLent(t *testing.T) {
	var images lentImages
	require.False(t, images.Available())
	_, _, err := images.FetchImage(t.Context(), "F1", "image/png")
	require.True(t, files.IsTransient(err), "err = %v", err)

	release := images.Lend(t.Context(), stubImageFetcher{})
	require.True(t, images.Available())
	data, contentType, err := images.FetchImage(t.Context(), "F1", "image/png")
	require.NoError(t, err)
	require.Equal(t, "png", string(data))
	require.Equal(t, "image/png", contentType)
	release()
	require.False(t, images.Available())
}

// blockingImageFetcher holds each fetch until its context ends.
type blockingImageFetcher struct{ started chan struct{} }

func (f blockingImageFetcher) FetchImage(ctx context.Context, _, _ string) ([]byte, string, error) {
	close(f.started)
	<-ctx.Done()
	return nil, "", ctx.Err()
}

// TestLentImageFetchEndsWithItsRun: a fetch in progress holds the fetcher
// lent, so the run's release waits for it, and the run's end ends it, so a
// stopping run is not stalled; the cut-short fetch fails transiently.
func TestLentImageFetchEndsWithItsRun(t *testing.T) {
	var images lentImages
	run, stop := context.WithCancel(t.Context())
	fetcher := blockingImageFetcher{started: make(chan struct{})}
	release := images.Lend(run, fetcher)
	fetched := make(chan error, 1)
	go func() {
		_, _, err := images.FetchImage(t.Context(), "F1", "image/png")
		fetched <- err
	}()
	<-fetcher.started

	released := make(chan struct{})
	go func() {
		release()
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("release returned while a fetch still held the fetcher")
	case <-time.After(50 * time.Millisecond):
	}

	stop()
	select {
	case err := <-fetched:
		require.True(t, files.IsTransient(err), "err = %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the run's end did not end the fetch")
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("release did not return once the fetch did")
	}
}
