package sessioningest

import (
	"testing"
	"time"
)

// TestSchedulePredicateReadsThresholdsEachTime: the thresholds are read for
// each candidate, so the worker follows a change an administrator saves
// while it runs, and the extremes do what the page says: every zero
// summarizes any change at once.
func TestSchedulePredicateReadsThresholdsEachTime(t *testing.T) {
	current := DefaultSummarizePolicy()
	w := &Worker{Cfg: WorkerConfig{Policy: DefaultPolicy(), Summarize: true, Thresholds: func() SummarizePolicy { return current }}}
	now := time.Now()
	// Summarized before, grown a little, changed a minute ago.
	c := Candidate{
		Identity: Identity{Owner: "o", Client: "codex", SessionID: "s"},
		Revision: "sha256:v2", PublishedRevision: "sha256:v1", Complete: true,
		RecordCount: 103, PublishedRecordCount: 100, UserTurnCount: 11, PublishedUserTurns: 10,
		ContentBytes: 2 << 10, PublishedContentBytes: 1 << 10,
		UpdatedAt: now.Add(-time.Minute),
	}
	if w.schedulePredicate(c) {
		t.Fatal("the default thresholds summarize a session that grew little and is not quiet")
	}
	current = noQuietTime()
	if w.schedulePredicate(c) {
		t.Fatal("without the quiet time, it still grew by less than every amount")
	}
	current.MinAddedRecords = 3
	if !w.schedulePredicate(c) {
		t.Fatal("it grew by the records now asked for")
	}
	current = DefaultSummarizePolicy()
	current.MaxPendingFor = 0
	if !w.schedulePredicate(c) {
		t.Fatal("a longest wait of zero summarizes every change at once")
	}
	current = anyChange()
	if !w.schedulePredicate(c) {
		t.Fatal("every zero summarizes any change at once")
	}
}
