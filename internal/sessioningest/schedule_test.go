package sessioningest

import (
	"testing"
	"time"
)

// newScheduleTest builds a Worker with a policy pinned to the default
// threshold values and Summarize enabled.
func newScheduleTest(t *testing.T) *Worker {
	t.Helper()
	if turns := DefaultSummarizePolicy().MinAddedUserTurns; turns != 2 {
		t.Fatalf("minAddedUserTurns = %d, want 2", turns)
	}
	return &Worker{Cfg: WorkerConfig{Policy: DefaultPolicy(), Summarize: true}}
}

// noQuietTime is the default thresholds without the wait for a session to
// go quiet, and anyChange thresholds that summarize every change at once.
func noQuietTime() SummarizePolicy {
	thresholds := DefaultSummarizePolicy()
	thresholds.QuietFor = 0
	return thresholds
}

func anyChange() SummarizePolicy { return SummarizePolicy{} }

func cand(rev string, published string) Candidate {
	return Candidate{
		Identity:          Identity{Owner: "o", Client: "codex", SessionID: "s"},
		Revision:          rev,
		PublishedRevision: published,
		Complete:          true,
		UpdatedAt:         time.Now().Add(-30 * time.Minute),
	}
}

func TestSchedulePredicate_RewriteEpochForcesReindex(t *testing.T) {
	w := newScheduleTest(t)
	hdl := Candidate{
		Identity: Identity{Owner: "o", Client: "codex", SessionID: "s"},
		Revision: "sha256:new", Complete: true, RewriteEpoch: 3,
		PublishedRewrite: 2,
		RecordCount:      5, UserTurnCount: 1,
		PublishedRecordCount: 50, PublishedUserTurns: 20,
		UpdatedAt: time.Now().Add(-30 * time.Minute),
	}
	if !w.schedulePredicate(hdl) {
		t.Error("rewrite_epoch change must force replacement even on shrink")
	}
}

func TestSchedulePredicate_NoNewContentBelowThresholds(t *testing.T) {
	w := newScheduleTest(t)
	// same revision → not scheduled
	hdl := cand("sha256:v1", "sha256:v1")
	if w.schedulePredicate(hdl) {
		t.Error("unchanged revision must not schedule")
	}
	// new revision but never-published + quiet → eligible on first pass
	hdl2 := cand("sha256:v2", "")
	if !w.schedulePredicate(hdl2) {
		t.Error("never-published complete quiet revision must schedule (never-summarized disjunct)")
	}
}

func TestSchedulePredicate_NewUserTurnsOverThreshold(t *testing.T) {
	w := newScheduleTest(t)
	hdl := Candidate{
		Identity: Identity{Owner: "o", Client: "codex", SessionID: "s"},
		Revision: "sha256:v2", Complete: true,
		RecordCount: 10, UserTurnCount: 9,
		PublishedRecordCount: 10, PublishedUserTurns: 5, // +4 >= 2
		UpdatedAt: time.Now().Add(-30 * time.Minute),
	}
	if !w.schedulePredicate(hdl) {
		t.Error("added user turns over threshold should schedule")
	}
}

// A growing session is summarized when it pauses with enough new content,
// never on each push, and a session that stops growing below the thresholds
// still gets its final pass once it has waited MaxPendingFor.
func TestSchedulePredicate_Cadence(t *testing.T) {
	w := newScheduleTest(t)
	policy := DefaultSummarizePolicy()
	now := time.Now()
	longAgo := now.Add(-policy.MaxPendingFor - time.Minute)
	recently := now.Add(-time.Minute)
	quiet := now.Add(-policy.QuietFor - time.Minute)
	grown := func(addedRecords, addedTurns int, addedBytes int64, updatedAt time.Time, firstPending *time.Time) Candidate {
		return Candidate{
			Identity: Identity{Owner: "o", Client: "codex", SessionID: "s"},
			Revision: "sha256:v2", PublishedRevision: "sha256:v1", Complete: true,
			RecordCount: 100 + addedRecords, PublishedRecordCount: 100,
			UserTurnCount: 10 + addedTurns, PublishedUserTurns: 10,
			ContentBytes: 1<<20 + addedBytes, PublishedContentBytes: 1 << 20,
			UpdatedAt: updatedAt, FirstPendingAt: firstPending,
		}
	}
	for _, tc := range []struct {
		name string
		c    Candidate
		want bool
	}{
		{"new content while still being pushed", grown(50, 5, 200<<10, recently, nil), false},
		{"new content after a quiet pause", grown(50, 5, 200<<10, quiet, nil), true},
		{"quiet but below every threshold", grown(3, 1, 1<<10, quiet, &recently), false},
		{"final pass for a session that stopped below the thresholds", grown(3, 1, 1<<10, quiet, &longAgo), true},
		{"pending too long while still being pushed", grown(3, 1, 1<<10, recently, &longAgo), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.schedulePredicate(tc.c); got != tc.want {
				t.Fatalf("schedulePredicate = %v, want %v", got, tc.want)
			}
		})
	}
}
