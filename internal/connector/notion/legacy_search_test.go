package notion

// Test-only compatibility helpers for the pre-2026 Search ingestion tests.
// Production intentionally has no Search-to-ingest path.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

type legacySearchBatch struct {
	Ingested   int
	NextCursor string
	Done       bool
}

func (w *Walker) SearchSince(ctx context.Context, cutoff time.Time) (int, error) {
	cursor := ""
	total := 0
	for {
		batch, err := w.searchPageForTest(ctx, cutoff, cursor)
		if err != nil {
			return total, err
		}
		total += batch.Ingested
		if batch.Done {
			return total, nil
		}
		cursor = batch.NextCursor
	}
}

func (w *Walker) searchPageForTest(ctx context.Context, cutoff time.Time, cursor string) (legacySearchBatch, error) {
	res, err := w.Client.Search(ctx, SearchParams{
		FilterObject: "page", SortDirection: "descending", StartCursor: cursor, PageSize: 100,
	})
	if err != nil {
		return legacySearchBatch{}, err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.Concurrency)
	var mu sync.Mutex
	ingested := 0
	stop := false
	for _, raw := range res.Results {
		var page Page
		if err := json.Unmarshal(raw, &page); err != nil {
			return legacySearchBatch{}, err
		}
		if page.LastEditedTime.Before(cutoff) {
			stop = true
			break
		}
		pageCopy := page
		g.Go(func() error {
			_, err := w.ingestOne(gctx, &pageCopy, nil, nil)
			if errors.Is(err, errSkipped) || isSkipError(err) {
				return nil
			}
			if err != nil {
				return err
			}
			mu.Lock()
			ingested++
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return legacySearchBatch{}, err
	}
	return legacySearchBatch{Ingested: ingested, NextCursor: res.NextCursor, Done: stop || !res.HasMore || res.NextCursor == ""}, nil
}
