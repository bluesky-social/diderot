package scopesync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/bluesky-social/diderot/internal/events"
)

func seedScopeBacklog(t *testing.T, pool *pgxpool.Pool, chunks int) int64 {
	t.Helper()
	eventID, _ := seed(t, pool, "CCATCHUP", false, events.ScopePublic)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at, embedded_at, embedder_id, scope_sync_needed)
		SELECT $1, 'text', chunk_idx, 'extra', now(), now(), 'test', true
		  FROM generate_series(1, $2::int) AS chunk_idx
	`, eventID, chunks-1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE event_chunks SET scope_sync_needed=true WHERE event_id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func TestDrainScopeCatchUpBounds(t *testing.T) {
	for _, chunks := range []int{0, 2, batchSize, batchSize + 1, scopeCatchUpBatches*batchSize + 1} {
		t.Run(fmt.Sprint(chunks), func(t *testing.T) {
			pool := setupDB(t)
			if chunks > 0 {
				seedScopeBacklog(t, pool, chunks)
			}
			fake := &fakeStore{}
			applied, err := service(pool, fake).DrainScopeCatchUp(context.Background())
			want := min(chunks, scopeCatchUpBatches*batchSize)
			if err != nil || applied != want {
				t.Fatalf("applied=%d want=%d err=%v", applied, want, err)
			}
			if len(fake.scopeCalls) != (want+batchSize-1)/batchSize {
				t.Fatalf("calls=%d for %d chunks", len(fake.scopeCalls), want)
			}
			for _, ids := range fake.scopeCalls {
				if len(ids) > batchSize {
					t.Fatalf("oversized call: %d", len(ids))
				}
			}
			var pending int
			if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM event_chunks WHERE scope_sync_needed`).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending != chunks-want {
				t.Fatalf("pending=%d want=%d", pending, chunks-want)
			}
		})
	}
}

func TestDrainScopeCatchUpContinuesAfterRacingScopeChange(t *testing.T) {
	pool := setupDB(t)
	eventID := seedScopeBacklog(t, pool, batchSize)
	fake := &fakeStore{}
	fake.setScope = func(_ []uint64, _ string) error {
		if len(fake.scopeCalls) == 1 {
			_, err := pool.Exec(context.Background(), `UPDATE events SET scope='slack-channel:CCATCHUP' WHERE id=$1`, eventID)
			return err
		}
		return nil
	}
	applied, err := service(pool, fake).DrainScopeCatchUp(context.Background())
	if err != nil || applied != batchSize || len(fake.scopeCalls) != 2 {
		t.Fatalf("applied=%d calls=%d err=%v", applied, len(fake.scopeCalls), err)
	}
	if fake.scopeValues[1] != "slack-channel:CCATCHUP" {
		t.Fatalf("second scope=%q", fake.scopeValues[1])
	}
}

func TestDrainScopeCatchUpStopsOnFailure(t *testing.T) {
	pool := setupDB(t)
	seedScopeBacklog(t, pool, 2*batchSize+1)
	fake := &fakeStore{}
	failure := errors.New("qdrant unavailable")
	fake.setScope = func(_ []uint64, _ string) error {
		if len(fake.scopeCalls) == 2 {
			return failure
		}
		return nil
	}
	applied, err := service(pool, fake).DrainScopeCatchUp(context.Background())
	if !errors.Is(err, failure) || applied != batchSize || len(fake.scopeCalls) != 2 {
		t.Fatalf("applied=%d calls=%d err=%v", applied, len(fake.scopeCalls), err)
	}
	var pending, backedOff, untouched int
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE scope_sync_needed),
		       count(*) FILTER (WHERE scope_sync_attempts=1 AND scope_sync_next_attempt_at > now()),
		       count(*) FILTER (WHERE scope_sync_needed AND scope_sync_attempts=0)
		  FROM event_chunks
	`).Scan(&pending, &backedOff, &untouched); err != nil {
		t.Fatal(err)
	}
	if pending != batchSize+1 || backedOff != batchSize || untouched != 1 {
		t.Fatalf("pending=%d backed_off=%d untouched=%d", pending, backedOff, untouched)
	}
}

func TestDrainScopeCatchUpCancellation(t *testing.T) {
	pool := setupDB(t)
	seedScopeBacklog(t, pool, batchSize+1)
	fake := &fakeStore{}
	service := service(pool, fake)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if testutil.ToFloat64(service.Metrics.scopeApplied) == batchSize {
					cancel()
					return
				}
			}
		}
	}()
	applied, err := service.DrainScopeCatchUp(ctx)
	cancel()
	<-done
	if !errors.Is(err, context.Canceled) || applied != batchSize || len(fake.scopeCalls) != 1 {
		t.Fatalf("applied=%d calls=%d err=%v", applied, len(fake.scopeCalls), err)
	}
	applied, err = service.DrainScopeCatchUp(ctx)
	if !errors.Is(err, context.Canceled) || applied != 0 || len(fake.scopeCalls) != 1 {
		t.Fatalf("canceled context: applied=%d calls=%d err=%v", applied, len(fake.scopeCalls), err)
	}
}

func TestQueueGaugesRefreshIndependently(t *testing.T) {
	pool := setupDB(t)
	seedScopeBacklog(t, pool, 2)
	service := service(pool, &fakeStore{})
	ctx := context.Background()
	if err := service.RefreshGauges(ctx); err != nil {
		t.Fatal(err)
	}
	if pending := testutil.ToFloat64(service.Metrics.scopePending); pending != 2 {
		t.Fatalf("pending=%v", pending)
	}
	if _, err := service.DrainScopeCatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DrainScope(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DrainDeletes(ctx); err != nil {
		t.Fatal(err)
	}
	if pending := testutil.ToFloat64(service.Metrics.scopePending); pending != 2 {
		t.Fatalf("drain refreshed the gauge: %v", pending)
	}
	if err := service.RefreshGauges(ctx); err != nil {
		t.Fatal(err)
	}
	if pending := testutil.ToFloat64(service.Metrics.scopePending); pending != 0 {
		t.Fatalf("explicit refresh pending=%v", pending)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := service.RefreshGauges(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh error=%v", err)
	}
}

func TestEventChunksEventIndexCoversAllKinds(t *testing.T) {
	pool := setupDB(t)
	var valid, full bool
	var definition string
	if err := pool.QueryRow(context.Background(), `
		SELECT indisvalid, indpred IS NULL, pg_get_indexdef(indexrelid)
		  FROM pg_index WHERE indexrelid='idx_event_chunks_event_id'::regclass
	`).Scan(&valid, &full, &definition); err != nil {
		t.Fatal(err)
	}
	if !valid || !full || !strings.Contains(definition, "USING btree (event_id)") {
		t.Fatalf("valid=%v full=%v definition=%s", valid, full, definition)
	}
}
