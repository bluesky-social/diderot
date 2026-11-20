package sessioningest

import (
	"context"
	"errors"
	"testing"

	"github.com/bluesky-social/diderot/internal/events"
)

// TestFeedRebindRetiresFormerOwnersCandidateRow is a regression test for a
// stale session_ingest_candidates row surviving a session rebind.
//
// Trigger: the feed reports the same session_id first under ownerA, then
// under ownerB -- an identity correction or re-attribution at the source.
// bindings.go's bindSessionTx doc comment states the contract: "the feed
// stays the authority for the owner of a session it writes ... as the
// candidate row always followed the feed's owner."
//
// Root cause: session_ingest_candidates is keyed on (owner, client,
// session_id); session_ingest_bindings (the durable authority for a
// session's current owner) is keyed on session_id alone. bindSessionTx
// moves the binding in place, but upsertCandidateTx's INSERT ... ON
// CONFLICT targets the old composite key, which no longer matches the new
// owner, so it INSERTS a second row for ownerB instead of retiring ownerA's.
//
// The test asserts that neither ownerA's row nor the explicit owner-only
// choice ownerA made before the rebind survives it.
func TestFeedRebindRetiresFormerOwnersCandidateRow(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	const (
		sessionID = "ses-rebind-retire"
		client    = "codex"
		ownerA    = "ownera@example.com"
		ownerB    = "ownerb@example.com"
	)

	// The feed first reports the session under ownerA.
	if _, err := store.UpsertChanges(ctx, []SessionChange{change(1, sessionID, ownerA, client)}, admitAll); err != nil {
		t.Fatal(err)
	}
	// ownerA makes an explicit, owner-only privacy choice while they still
	// hold the session.
	service, err := NewPrivacyService(store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	selection := []SessionRef{{Client: client, SessionID: sessionID}}
	if _, err := service.SetSessions(ctx, ownerA, selection, true); err != nil {
		t.Fatal(err)
	}

	// The feed later reports the SAME session_id under ownerB.
	if _, err := store.UpsertChanges(ctx, []SessionChange{change(2, sessionID, ownerB, client)}, admitAll); err != nil {
		t.Fatal(err)
	}

	// session_ingest_bindings (the durable authority) agrees: ownerB owns
	// the session now.
	var boundOwner string
	if err := pool.QueryRow(ctx, `SELECT owner_canonical FROM session_ingest_bindings WHERE session_id=$1`, sessionID).Scan(&boundOwner); err != nil {
		t.Fatal(err)
	}
	if boundOwner != ownerB {
		t.Fatalf("session_ingest_bindings owner = %q, want %q (the feed's latest report)", boundOwner, ownerB)
	}

	// ownerA's row must be retired, not orphaned.
	var staleRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM session_ingest_candidates WHERE session_id=$1 AND owner_canonical=$2
	`, sessionID, ownerA).Scan(&staleRows); err != nil {
		t.Fatal(err)
	}
	if staleRows != 0 {
		t.Fatalf("former owner's candidate row still exists (%d rows) after the feed rebound the session", staleRows)
	}

	// ownerB's new row must not inherit ownerA's explicit privacy choice:
	// privacy_owner_only is unset, and visibility follows deployment policy
	// (internal, from admitAll), not ownerA's owner-only pick.
	var privacyExplicit *bool
	var scope string
	if err := pool.QueryRow(ctx, `
		SELECT privacy_owner_only, visibility_scope FROM session_ingest_candidates
		 WHERE session_id=$1 AND owner_canonical=$2
	`, sessionID, ownerB).Scan(&privacyExplicit, &scope); err != nil {
		t.Fatal(err)
	}
	if privacyExplicit != nil {
		t.Fatalf("new owner's row carried over an explicit privacy choice: privacy_owner_only=%v", *privacyExplicit)
	}
	if scope != events.ScopeInternal {
		t.Fatalf("new owner's row visibility_scope = %q, want %q (deployment policy, not ownerA's choice)", scope, events.ScopeInternal)
	}

	// ownerA's self-service inventory must no longer list the session.
	page, err := service.ListSessions(ctx, ownerA, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range page.Sessions {
		if s.SessionID == sessionID {
			t.Fatalf("former owner's session inventory still lists the rebound session: %+v", s)
		}
	}
}

// TestSetSessionsRejectsFormerOwnerAfterFeedRebindsSession pins the
// cross-owner boundary a stale candidate row would break: once the feed
// moves a session_id to ownerB, ownerA's self-service privacy endpoint
// (PrivacyService.SetSessions, internal/adminapi/session_privacy.go's
// handleMyAgentSessionPrivacy) must reject a call naming that session, and
// ownerB's actual, current session visibility must stay unaffected by
// anything ownerA does.
//
// Contract: AGENTS.md Architecture invariants, "push may never touch a feed
// session or another owner's; the feed stays the authority for its own
// sessions' owners." ownerA's call must fail and leave ownerB's session
// scope unchanged.
func TestSetSessionsRejectsFormerOwnerAfterFeedRebindsSession(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	const (
		sessionID = "ses-rebind-hijack"
		client    = "codex"
		ownerA    = "ownera@example.com"
		ownerB    = "ownerb@example.com"
	)

	if _, err := store.UpsertChanges(ctx, []SessionChange{change(1, sessionID, ownerA, client)}, admitAll); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertChanges(ctx, []SessionChange{change(2, sessionID, ownerB, client)}, admitAll); err != nil {
		t.Fatal(err)
	}

	// Simulate this session's content already in the searchable corpus,
	// scoped "internal" (company-wide), as the publish worker would leave a
	// non-owner-only session.
	const externalID = sessionID + ":session"
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		VALUES ('agent_session', $1, now(), 'content', $2, 'internal')
	`, externalID, `{"event_kind":"native_session"}`); err != nil {
		t.Fatal(err)
	}

	service, err := NewPrivacyService(store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	selection := []SessionRef{{Client: client, SessionID: sessionID}}

	// ownerA no longer owns this session. The self-service privacy API must
	// not accept a change from them for it.
	if _, err := service.SetSessions(ctx, ownerA, selection, true); err == nil {
		t.Fatal("ownerA's privacy call on a session owned (per session_ingest_bindings) by ownerB unexpectedly succeeded")
	} else if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound rejecting ownerA's stale claim, got: %v", err)
	}

	// Regardless of the error returned, ownerB's real, current session must
	// not have had its scope changed by ownerA's call.
	var scope string
	if err := pool.QueryRow(ctx, `SELECT scope FROM events WHERE external_id=$1`, externalID).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != events.ScopeInternal {
		t.Fatalf("ownerA's privacy call changed ownerB's current session scope to %q (hijacked); want unaffected %q",
			scope, events.ScopeInternal)
	}
}

// TestSetSessionsIgnoresCandidateDisagreeingWithBinding is a defense-in-depth
// test, independent of the retire-on-rebind step above: even if a
// session_ingest_candidates row for ownerA exists that disagrees with
// session_ingest_bindings (a restore, or any write-path bug),
// applySessionPrivacyTx must not act on it. The row here is inserted
// directly, bypassing every normal write path, specifically to prove the
// privacy write doesn't trust owner_canonical/client/session_id alone.
func TestSetSessionsIgnoresCandidateDisagreeingWithBinding(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	const (
		sessionID = "ses-binding-guard"
		client    = "codex"
		ownerA    = "ownera@example.com"
		ownerB    = "ownerb@example.com"
	)

	// The binding says ownerB owns this session.
	if _, err := store.UpsertChanges(ctx, []SessionChange{change(1, sessionID, ownerB, client)}, admitAll); err != nil {
		t.Fatal(err)
	}

	// A candidate row for ownerA that disagrees with the binding.
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates
		  (owner, client, session_id, owner_canonical, revision, complete, published_revision, visibility_scope)
		VALUES ($1, $2, $3, $1, 'sha256:stale', true, 'sha256:stale', 'internal')
	`, ownerA, client, sessionID); err != nil {
		t.Fatal(err)
	}

	const externalID = sessionID + ":session"
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		VALUES ('agent_session', $1, now(), 'content', $2, 'internal')
	`, externalID, `{"event_kind":"native_session"}`); err != nil {
		t.Fatal(err)
	}

	service, err := NewPrivacyService(store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	selection := []SessionRef{{Client: client, SessionID: sessionID}}
	if _, err := service.SetSessions(ctx, ownerA, selection, true); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SetSessions from ownerA, whom the binding disagrees with, = %v, want %v", err, ErrSessionNotFound)
	}

	var scope string
	if err := pool.QueryRow(ctx, `SELECT scope FROM events WHERE external_id=$1`, externalID).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != events.ScopeInternal {
		t.Fatalf("ownerA's rejected call still changed the scope to %q", scope)
	}
}

// TestReconcileCanonicalEventScopesIgnoresCandidateDisagreeingWithBinding is
// a defense-in-depth test: a session_ingest_candidates row for a former
// owner that disagrees with session_ingest_bindings must not drive
// ReconcileCanonicalEventScopes to restamp the current owner's events to the
// former owner's scope. The row is inserted directly, bypassing every
// normal write path, to isolate this guard from the rebind fix.
func TestReconcileCanonicalEventScopesIgnoresCandidateDisagreeingWithBinding(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()

	id := Identity{Owner: "ownerb@example.com", Client: "codex", SessionID: "ses-scope-guard"}
	h.seedCandidate(t, id, "sha256:v1", 1, 1)
	h.publish(t, id, "sha256:v1", 1, []SummaryRecord{{
		Identity: id, RecordStart: 0, RecordEnd: 1,
		ContentHash: "h1", Summary: "range", ModelID: "map-model",
	}})
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// A former owner's candidate row for the same session_id, disagreeing
	// with the binding (ownerB), pinned owner-only.
	staleOwner := "ownera@example.com"
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates (
		    owner, client, session_id, revision, complete,
		    published_revision, visibility_scope, owner_canonical, privacy_owner_only
		) VALUES ($1, $2, $3, 'sha256:stale', true, 'sha256:stale', $4, $1, true)
	`, staleOwner, id.Client, id.SessionID, events.ScopeOwner(staleOwner)); err != nil {
		t.Fatal(err)
	}

	// Widen the events to what ownerB's own candidate says they should be:
	// internal.
	if _, err := h.pool.Exec(ctx, `UPDATE events SET scope='internal' WHERE source='agent_session'`); err != nil {
		t.Fatal(err)
	}

	repaired, err := h.store.ReconcileCanonicalEventScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 0 {
		t.Fatalf("reconcile repaired %d events using a candidate that disagrees with the binding, want 0", repaired)
	}

	var scope string
	if err := h.pool.QueryRow(ctx, `SELECT scope FROM events WHERE external_id=$1`, externalID(id)).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != events.ScopeInternal {
		t.Fatalf("session scope = %q, want %q (the stale owner-only candidate must not have restamped it)", scope, events.ScopeInternal)
	}
}

// TestReapplyPolicyIgnoresCandidateDisagreeingWithBinding is a
// defense-in-depth test: ReapplyPolicy must not decide, or act on, a
// candidate row whose owner disagrees with session_ingest_bindings. Acting
// on one would tombstone the real session's published ledger and queue its
// corpus for deletion under the former owner's identity (reconcileAdmissionTx
// + the corpus-tombstone sweep delete by session_id alone). The row is
// inserted directly, bypassing every normal write path, to isolate this
// guard from the rebind fix.
func TestReapplyPolicyIgnoresCandidateDisagreeingWithBinding(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	const (
		sessionID  = "ses-reapply-guard"
		client     = "codex"
		ownerB     = "ownerb@example.com"
		staleOwner = "ownera@example.com"
	)

	// The binding says ownerB owns this session; its candidate is active
	// and already matches the policy decision below.
	if _, err := store.UpsertChanges(ctx, []SessionChange{change(1, sessionID, ownerB, client)}, admitAll); err != nil {
		t.Fatal(err)
	}

	// A former owner's candidate row that disagrees with the binding,
	// active and pinned owner-only, published.
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates (
		    owner, client, session_id, revision, complete,
		    published_revision, visibility_scope, owner_canonical, privacy_owner_only
		) VALUES ($1, $2, $3, 'sha256:stale', true, 'sha256:stale', $4, $1, true)
	`, staleOwner, client, sessionID, events.ScopeOwner(staleOwner)); err != nil {
		t.Fatal(err)
	}

	// The policy excludes the former owner's identity but leaves ownerB's
	// alone: if the stale row were visited, this would be a changed
	// decision (active -> excluded).
	decide := func(ch SessionChange) ChangePolicy {
		if ch.Owner == staleOwner {
			return ChangePolicy{Admitted: false}
		}
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}

	batch, err := store.ReapplyPolicy(ctx, Identity{}, 100, decide)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Checked != 1 || batch.Changed != 0 {
		t.Fatalf("batch = %+v, want only ownerB's binding-matching candidate checked and nothing changed", batch)
	}

	// The stale row must not have been excluded or queued for corpus
	// removal: that removal deletes the shared events by session_id alone,
	// which would destroy ownerB's current session.
	var excluded, tombstonePending bool
	if err := pool.QueryRow(ctx, `
		SELECT excluded_at IS NOT NULL, corpus_tombstone_pending IS NOT NULL
		  FROM session_ingest_candidates WHERE owner=$1 AND client=$2 AND session_id=$3
	`, staleOwner, client, sessionID).Scan(&excluded, &tombstonePending); err != nil {
		t.Fatal(err)
	}
	if excluded || tombstonePending {
		t.Fatalf("stale candidate was acted on by ReapplyPolicy: excluded=%v corpus_tombstone_pending=%v", excluded, tombstonePending)
	}
}

// The tests below cover a candidate with NO binding row at all. A candidate
// written before the bindings table existed was never backfilled, and that
// is a supported state (bindings.go's bindSessionTx handles "an id the feed
// wrote before bindings existed" explicitly, and the cursor never moves
// back over such sessions). The binding guards must therefore require no
// DISAGREEING binding, not a matching one: requiring a match would lock
// every such session out of self-service privacy, policy reapplication, and
// scope-drift repair. Likewise the stale-candidate retire step must run on
// a binding's first insert as well as on a rebind, since bindSessionTx
// reports a legacy session's first owner correction as an insert (there is
// no prior binding to call a rebind).

// TestSetSessionsAllowsLegacyCandidateWithNoBindingRow: the owner of a
// session ingested before the bindings table existed must still be able to
// set its privacy through the self-service endpoint. A guard of "a matching
// binding exists" fails here because no binding row exists at all: the
// UPDATE's join never matches and SetSessions returns ErrSessionNotFound
// for the owner's own session.
func TestSetSessionsAllowsLegacyCandidateWithNoBindingRow(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	const (
		sessionID = "ses-legacy-privacy"
		client    = "codex"
		owner     = "ownera@example.com"
	)

	// A legacy candidate row: written directly, bypassing every normal
	// write path, with no session_ingest_bindings row for this
	// session_id -- what a pre-bindings-table session looks like.
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates (
		    owner, client, session_id, owner_canonical, revision, complete,
		    published_revision, visibility_scope
		) VALUES ($1, $2, $3, $1, 'sha256:v1', true, 'sha256:v1', 'internal')
	`, owner, client, sessionID); err != nil {
		t.Fatal(err)
	}

	service, err := NewPrivacyService(store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	selection := []SessionRef{{Client: client, SessionID: sessionID}}
	if _, err := service.SetSessions(ctx, owner, selection, true); err != nil {
		t.Fatalf("owner's privacy call on their own legacy (no-binding-row) session failed: %v", err)
	}

	var scope string
	var explicit bool
	if err := pool.QueryRow(ctx, `
		SELECT visibility_scope, privacy_owner_only FROM session_ingest_candidates
		 WHERE owner=$1 AND client=$2 AND session_id=$3
	`, owner, client, sessionID).Scan(&scope, &explicit); err != nil {
		t.Fatal(err)
	}
	if scope != events.ScopeOwner(owner) || !explicit {
		t.Fatalf("legacy session privacy = (scope=%q, owner_only=%v), want (%q, true)", scope, explicit, events.ScopeOwner(owner))
	}
}

// TestReapplyPolicyReachesLegacyCandidateWithNoBindingRow: an administrator
// policy change (e.g. excluding a principal) must still reach a session
// ingested before the bindings table existed. A guard of "a matching
// binding exists" fails here because the listing's EXISTS join excludes
// every candidate with no binding row, so ReapplyPolicy would silently skip
// legacy sessions forever: content that should be tombstoned would stay
// visible to the whole organization.
func TestReapplyPolicyReachesLegacyCandidateWithNoBindingRow(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	const (
		sessionID = "ses-legacy-reapply"
		client    = "codex"
		owner     = "ownera@example.com"
	)

	if _, err := pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates (
		    owner, client, session_id, owner_canonical, change_seq, visibility_scope
		) VALUES ($1, $2, $3, $1, 1, 'internal')
	`, owner, client, sessionID); err != nil {
		t.Fatal(err)
	}

	decide := func(SessionChange) ChangePolicy { return ChangePolicy{Admitted: false} }

	batch, err := store.ReapplyPolicy(ctx, Identity{}, 100, decide)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Checked != 1 || batch.Changed != 1 {
		t.Fatalf("batch = %+v, want the legacy no-binding-row candidate checked and excluded", batch)
	}

	var excluded, tombstonePending bool
	if err := pool.QueryRow(ctx, `
		SELECT excluded_at IS NOT NULL, corpus_tombstone_pending IS NOT NULL
		  FROM session_ingest_candidates WHERE owner=$1 AND client=$2 AND session_id=$3
	`, owner, client, sessionID).Scan(&excluded, &tombstonePending); err != nil {
		t.Fatal(err)
	}
	if !excluded || !tombstonePending {
		t.Fatalf("legacy no-binding-row candidate was not acted on by ReapplyPolicy: excluded=%v corpus_tombstone_pending=%v", excluded, tombstonePending)
	}
}

// TestReconcileCanonicalEventScopesRepairsLegacyCandidateWithNoBindingRow: an
// owner-only session ingested before the bindings table existed must still
// have its scope-drift repaired. A guard of "a matching binding exists"
// fails here because the UPDATE's EXISTS join excludes every candidate
// with no binding row, so a widened event would stay widened (readable
// organization-wide) until the session's next feed change, if any.
func TestReconcileCanonicalEventScopesRepairsLegacyCandidateWithNoBindingRow(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	const (
		sessionID = "ses-legacy-scope-repair"
		client    = "codex"
		owner     = "ownera@example.com"
	)

	if _, err := pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates (
		    owner, client, session_id, owner_canonical, revision, complete,
		    published_revision, visibility_scope, privacy_owner_only
		) VALUES ($1, $2, $3, $1, 'sha256:v1', true, 'sha256:v1', $4, true)
	`, owner, client, sessionID, events.ScopeOwner(owner)); err != nil {
		t.Fatal(err)
	}

	const externalID = sessionID + ":session"
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		VALUES ('agent_session', $1, now(), 'content', $2, 'internal')
	`, externalID, `{"event_kind":"native_session"}`); err != nil {
		t.Fatal(err)
	}

	repaired, err := store.ReconcileCanonicalEventScopes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repaired != 1 {
		t.Fatalf("reconcile repaired %d events, want 1 (a legacy owner-only candidate with no binding row must still repair scope drift)", repaired)
	}

	var scope string
	if err := pool.QueryRow(ctx, `SELECT scope FROM events WHERE external_id=$1`, externalID).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if scope != events.ScopeOwner(owner) {
		t.Fatalf("session scope = %q, want %q (the legacy owner-only candidate must have restamped it)", scope, events.ScopeOwner(owner))
	}
}

// TestFeedWriteRetiresLegacyStaleCandidateOnFirstBindingInsert: a session
// ingested before the bindings table existed, under ownerA, that the feed
// later reports under ownerB. bindSessionTx sees no existing binding row
// for this session_id, so it INSERTS one (Inserted=true) rather than
// reporting a rebind (Rebound=true, which only an already-bound session can
// get). Gating the stale-candidate retire step on Rebound alone misses
// exactly this case, and the one-time migration (20261202130000) cannot
// catch it after the fact because it runs once, at upgrade, before this
// write ever happens.
func TestFeedWriteRetiresLegacyStaleCandidateOnFirstBindingInsert(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	const (
		sessionID = "ses-legacy-rebind"
		client    = "codex"
		ownerA    = "ownera@example.com"
		ownerB    = "ownerb@example.com"
	)

	// A legacy candidate row for ownerA, with no session_ingest_bindings
	// row for this session_id yet.
	if _, err := pool.Exec(ctx, `
		INSERT INTO session_ingest_candidates (
		    owner, client, session_id, owner_canonical, change_seq, visibility_scope
		) VALUES ($1, $2, $3, $1, 1, 'internal')
	`, ownerA, client, sessionID); err != nil {
		t.Fatal(err)
	}

	// The feed now reports the SAME session_id under ownerB: the first
	// binding write for this session_id, so bindSessionTx inserts rather
	// than rebinds.
	if _, err := store.UpsertChanges(ctx, []SessionChange{change(2, sessionID, ownerB, client)}, admitAll); err != nil {
		t.Fatal(err)
	}

	var boundOwner string
	if err := pool.QueryRow(ctx, `SELECT owner_canonical FROM session_ingest_bindings WHERE session_id=$1`, sessionID).Scan(&boundOwner); err != nil {
		t.Fatal(err)
	}
	if boundOwner != ownerB {
		t.Fatalf("session_ingest_bindings owner = %q, want %q (the feed's latest report)", boundOwner, ownerB)
	}

	var staleRows int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM session_ingest_candidates WHERE session_id=$1 AND owner_canonical=$2
	`, sessionID, ownerA).Scan(&staleRows); err != nil {
		t.Fatal(err)
	}
	if staleRows != 0 {
		t.Fatalf("legacy former owner's candidate row still exists (%d rows) after the feed's first binding write moved the session to a new owner", staleRows)
	}
}
