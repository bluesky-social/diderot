package slack

import (
	"testing"
	"time"
)

func TestSeenStore_FirstHitFresh(t *testing.T) {
	s := newSeenStore(time.Minute)
	if !s.MarkOrSeen("C1", "1700000600.000100") {
		t.Errorf("first call: want true (fresh), got false")
	}
	if s.MarkOrSeen("C1", "1700000600.000100") {
		t.Errorf("second call same key: want false (duplicate), got true")
	}
}

func TestSeenStore_DifferentKeysIndependent(t *testing.T) {
	s := newSeenStore(time.Minute)
	if !s.MarkOrSeen("C1", "ts1") {
		t.Fatal("first key fresh")
	}
	if !s.MarkOrSeen("C1", "ts2") {
		t.Errorf("different ts same channel: want fresh")
	}
	if !s.MarkOrSeen("C2", "ts1") {
		t.Errorf("different channel same ts: want fresh")
	}
}

func TestSeenStore_TTLExpiry(t *testing.T) {
	s := newSeenStore(50 * time.Millisecond)
	now := time.Now()
	s.now = func() time.Time { return now }

	if !s.MarkOrSeen("C", "ts") {
		t.Fatal("first call")
	}
	if s.MarkOrSeen("C", "ts") {
		t.Fatal("second call: want duplicate")
	}

	// Advance past the TTL.
	now = now.Add(60 * time.Millisecond)

	// Now the entry should have aged out — fresh again.
	if !s.MarkOrSeen("C", "ts") {
		t.Errorf("after TTL: want fresh (re-mark allowed), got duplicate")
	}
}

func TestSeenStore_GCSweep(t *testing.T) {
	s := newSeenStore(50 * time.Millisecond)
	now := time.Now()
	s.now = func() time.Time { return now }

	// Mark a bunch of keys, age them out, mark another → GC
	// should run and shrink the table.
	for i := 0; i < 20; i++ {
		s.MarkOrSeen("C", time.Duration(i).String())
	}
	if got := s.Size(); got != 20 {
		t.Errorf("Size before GC=%d, want 20", got)
	}

	now = now.Add(60 * time.Millisecond)
	// Any subsequent MarkOrSeen triggers gcLocked.
	s.MarkOrSeen("C", "fresh-key")
	if got := s.Size(); got != 1 {
		t.Errorf("Size after GC=%d, want 1 (only the fresh entry)", got)
	}
}

func TestSeenStore_EmptyKeyTreatedAsFresh(t *testing.T) {
	s := newSeenStore(time.Minute)
	// Empty channel.
	if !s.MarkOrSeen("", "ts") {
		t.Errorf("empty channel: want fresh (defensive), got duplicate")
	}
	// Empty ts.
	if !s.MarkOrSeen("C", "") {
		t.Errorf("empty ts: want fresh (defensive), got duplicate")
	}
	// Both empty.
	if !s.MarkOrSeen("", "") {
		t.Errorf("empty/empty: want fresh, got duplicate")
	}
}

func TestSeenStore_DefaultTTL(t *testing.T) {
	s := newSeenStore(0) // sentinel for default
	if s.ttl != 10*time.Minute {
		t.Errorf("default ttl=%v, want 10m", s.ttl)
	}
}
