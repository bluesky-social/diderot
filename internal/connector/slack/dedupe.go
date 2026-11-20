package slack

import (
	"sync"
	"time"
)

// seenStore is the in-memory dedupe for socket-mode event redelivery.
// Keyed on the slack ts ("channel_id:message_ts") so a single
// @-mention event that Slack delivers multiple times collapses to
// one ingest + one ask turn.
//
// Slack socket-mode redelivers events when the consumer doesn't
// ack within a 3s window. Without this store, every retry is
// processed as a fresh user turn: duplicate replies, duplicate
// model spend, confusing UX. With it, redeliveries observed inside
// the TTL window are dropped at the consumer boundary with a loud
// log + metric so the operational signal is visible.
//
// TTL is 10 minutes — generous bound on Slack's redelivery window
// (the in-connection 3s timeout retries fire within a few seconds;
// the longer-tail "Delayed Events" replay can fire up to ~30 min
// later, but typically much sooner).
//
// The store is goroutine-safe; the consumer's event loop is the
// single writer in practice but the dedupe check runs from
// goroutines that may outlive the event-loop tick (ack happens
// before the handler runs), so a mutex is necessary.
//
// Bounded growth: every entry expires by TTL, and we sweep on each
// Mark call (amortized cost). Production volumes (a few mentions
// per minute) keep the table tiny.
type seenStore struct {
	mu      sync.Mutex
	entries map[string]time.Time // key = "channel:ts"
	ttl     time.Duration
	now     func() time.Time // injectable for tests
}

// newSeenStore constructs a store with the given TTL. Pass 0 for
// the default (10 minutes).
func newSeenStore(ttl time.Duration) *seenStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &seenStore{
		entries: make(map[string]time.Time),
		ttl:     ttl,
		now:     time.Now,
	}
}

// MarkOrSeen returns true if (channel, ts) is freshly recorded
// (caller should process the event), false if it's a duplicate
// already inside the TTL window (caller should drop + log).
//
// Single-call check-and-set under the mutex avoids a TOCTOU race
// where two concurrent retries could both succeed.
func (s *seenStore) MarkOrSeen(channel, ts string) bool {
	if channel == "" || ts == "" {
		// Defensive: don't dedupe events with no key. Caller
		// should treat as a fresh event (the alternative is to
		// silently drop legitimate events with malformed
		// envelopes).
		return true
	}
	key := channel + ":" + ts
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.gcLocked(now)

	if _, ok := s.entries[key]; ok {
		return false
	}
	s.entries[key] = now
	return true
}

// Seen reports whether (channel, ts) is recorded inside the TTL window,
// without recording it.
func (s *seenStore) Seen(channel, ts string) bool {
	if s == nil || channel == "" || ts == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(s.now())
	_, ok := s.entries[channel+":"+ts]
	return ok
}

// gcLocked drops expired entries. Caller must hold s.mu.
//
// Bounded by len(entries); since entries are sweep-cleared
// every call after expiration, the steady state is O(active
// rate × TTL) which at our volumes is a handful of entries.
func (s *seenStore) gcLocked(now time.Time) {
	cutoff := now.Add(-s.ttl)
	for k, t := range s.entries {
		if t.Before(cutoff) {
			delete(s.entries, k)
		}
	}
}

// Size returns the current entry count. Test hook.
func (s *seenStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
