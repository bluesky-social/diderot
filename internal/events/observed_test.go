package events

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type countingObserver struct{ wrote int }

func (o *countingObserver) Wrote() { o.wrote++ }

// TestObservedSinkCountsWhatItWrites: an observed sink tells its observer of
// each event it inserts or changes, not of one it leaves as it was, and the
// sink it came from tells nobody.
func TestObservedSinkCountsWhatItWrites(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	observer := &countingObserver{}
	observed := sink.Observed(observer)
	ctx := context.Background()
	event := Event{
		Scope: ScopeInternal, Source: "note", ExternalID: "notes/a.txt",
		OccurredAt: time.Unix(1700000000, 0), Body: "first",
	}
	upsert := func(s *DBSink, body string) {
		t.Helper()
		event.Body = body
		if _, err := s.Upsert(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	upsert(observed, "first")
	upsert(observed, "first")
	upsert(observed, "second")
	upsert(sink, "third")
	if observer.wrote != 2 {
		t.Fatalf("wrote = %d, want the insert and the change", observer.wrote)
	}
}
