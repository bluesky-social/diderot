package askchat

import "testing"

// TestKeepNewestAndPinnedNegativeCapDoesNotPanic is a deterministic
// regression test for a crash in KeepNewestAndPinned (summarize.go).
//
// Trigger: KeepNewestAndPinned(msgs, cap) with a negative cap and any
// non-empty msgs.
//
// Contract: KeepNewestAndPinned's doc comment describes its behavior
// ("enforces the fetch cap while guaranteeing every pinned message
// survives") with no documented precondition that cap be non-negative, so
// an exported function with no stated precondition should not panic on a
// value its own doc does not rule out. Before the fix, `tailStart :=
// len(all) - cap` exceeded len(all) for cap < 0, and the loop
// `for i := 0; i < tailStart; i++ { if all[i].Pinned ... }` read past the
// end of the slice, panicking with "index out of range".
//
// Expected: a negative cap is treated like cap=0 (keep only pinned
// messages), not a panic.
func TestKeepNewestAndPinnedNegativeCapDoesNotPanic(t *testing.T) {
	msgs := []ThreadMessage{
		{Timestamp: "1", Pinned: true},
		{Timestamp: "2", Pinned: false},
		{Timestamp: "3", Pinned: false},
	}

	got := KeepNewestAndPinned(msgs, -1)

	want := []ThreadMessage{{Timestamp: "1", Pinned: true}}
	if len(got) != len(want) {
		t.Fatalf("KeepNewestAndPinned(msgs, -1) = %#v, want %#v", got, want)
	}
	if got[0].Timestamp != want[0].Timestamp || got[0].Pinned != want[0].Pinned {
		t.Errorf("KeepNewestAndPinned(msgs, -1) = %#v, want %#v", got, want)
	}
}
