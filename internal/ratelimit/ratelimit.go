// Package ratelimit paces one client's requests to the rate its service
// allows. Each client keeps its own rate and its own retry rules; what they
// share is the bucket.
package ratelimit

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Bucket is a token bucket every request of one client waits on. When the
// service asks for a pause (PauseUntil), every caller waits it out before
// taking a token. Goroutine-safe.
type Bucket struct {
	limiter *rate.Limiter

	mu    sync.Mutex
	until time.Time
}

// New returns a full bucket that refills at limit tokens a second and holds
// at most burst.
func New(limit rate.Limit, burst int) *Bucket {
	return &Bucket{limiter: rate.NewLimiter(limit, burst)}
}

// Limit is the bucket's refill rate.
func (b *Bucket) Limit() rate.Limit { return b.limiter.Limit() }

// Wait blocks until a request may go: past any pause, and with a token. A
// pause that starts while it waits for a token holds it too.
func (b *Bucket) Wait(ctx context.Context) error {
	for {
		if delay := time.Until(b.PausedUntil()); delay > 0 {
			if err := sleep(ctx, delay); err != nil {
				return err
			}
		}
		if err := b.limiter.Wait(ctx); err != nil {
			return err
		}
		if !time.Now().Before(b.PausedUntil()) {
			return nil
		}
	}
}

// PauseUntil holds every Wait until t. It never shortens a pause already
// set.
func (b *Bucket) PauseUntil(t time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.After(b.until) {
		b.until = t
	}
}

// PausedUntil is when the latest pause ends; zero if there never was one.
func (b *Bucket) PausedUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.until
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
