package sessioningest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
)

func TestPrivacyServiceRestampsOnlySelectedHistoryAndPreservesFutureUpdates(t *testing.T) {
	h := newChunkHarness(t)
	ctx := context.Background()
	selected := Identity{Owner: " Selected@Example.com\t", Client: "codex", SessionID: "ses_selected"}
	canonicalSelected := "selected@example.com"
	sibling := Identity{Owner: selected.Owner, Client: "claude", SessionID: "ses_sibling"}
	other := Identity{Owner: "other@example.com", Client: "codex", SessionID: "ses_other"}
	for _, id := range []Identity{selected, sibling, other} {
		h.seedCandidate(t, id, "sha256:v1", 1, 1)
		h.publish(t, id, "sha256:v1", 1, []SummaryRecord{{
			Identity: id, RecordStart: 0, RecordEnd: 1,
			ContentHash: "h1", Summary: "range", ModelID: "map-model",
		}})
	}
	if _, err := h.ep.ReconcileChunkEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		VALUES ('slack', 'unrelated', now(), 'slack', '{}', 'public')
	`); err != nil {
		t.Fatal(err)
	}
	service, err := NewPrivacyService(h.store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	selection := []SessionRef{{Client: selected.Client, SessionID: selected.SessionID}}
	result, err := service.SetSessions(ctx, canonicalSelected, selection, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionUpdates != 1 || result.CandidateUpdates != 1 || result.EventUpdates != 2 {
		t.Fatalf("restamp result = %+v, want 1 candidate and 2 events", result)
	}
	wantScope := events.ScopeOwner(canonicalSelected)
	var selectedWrong, siblingWrong, otherWrong, slackWrong int
	if err := h.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE source='agent_session' AND raw->>'owner'=$1 AND raw->>'session_id'=$4 AND scope<>$3),
		  count(*) FILTER (WHERE source='agent_session' AND raw->>'owner'=$1 AND raw->>'session_id'=$5 AND scope<>'internal'),
		  count(*) FILTER (WHERE source='agent_session' AND raw->>'owner'=$2 AND scope<>'internal'),
		  count(*) FILTER (WHERE source='slack' AND scope<>'public')
		FROM events
	`, selected.Owner, other.Owner, wantScope, selected.SessionID, sibling.SessionID).Scan(&selectedWrong, &siblingWrong, &otherWrong, &slackWrong); err != nil {
		t.Fatal(err)
	}
	if selectedWrong != 0 || siblingWrong != 0 || otherWrong != 0 || slackWrong != 0 {
		t.Fatalf("wrong scopes selected=%d sibling=%d other=%d slack=%d", selectedWrong, siblingWrong, otherWrong, slackWrong)
	}

	updated := change(3, selected.SessionID, selected.Owner, selected.Client)
	if _, err := h.store.upsertScopedAt(ctx, []SessionChange{updated}, 3, func(SessionChange) ChangePolicy {
		return ChangePolicy{Admitted: true, VisibilityScope: events.ScopeInternal}
	}); err != nil {
		t.Fatal(err)
	}
	candidate, err := h.store.LoadCandidate(ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.VisibilityScope != wantScope {
		t.Fatalf("updated candidate scope = %q, want %q", candidate.VisibilityScope, wantScope)
	}
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE source='agent_session' AND raw->>'owner'=$1 AND raw->>'session_id'=$2
		   AND deleted=false AND scope<>$3
	`, selected.Owner, selected.SessionID, wantScope).Scan(&selectedWrong); err != nil {
		t.Fatal(err)
	}
	if selectedWrong != 0 {
		t.Fatalf("%d selected events lost owner-only scope after source update", selectedWrong)
	}

	if _, err := service.SetSessions(ctx, canonicalSelected, selection, false); err != nil {
		t.Fatal(err)
	}
	var activeWrong int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM events
		 WHERE source='agent_session' AND raw->>'owner'=$1 AND raw->>'session_id'=$2
		   AND deleted=false AND scope<>'internal'
	`, selected.Owner, selected.SessionID).Scan(&activeWrong); err != nil {
		t.Fatal(err)
	}
	if activeWrong != 0 {
		t.Fatalf("%d historical events remained owner-only after opt-out", activeWrong)
	}
	var audits int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM session_privacy_audit WHERE owner=$1`, canonicalSelected).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 2 {
		t.Fatalf("privacy audit rows = %d, want 2", audits)
	}
}

func TestPrivacyServiceRejectsInvalidSelections(t *testing.T) {
	store, _ := newTestStore(t)
	service, err := NewPrivacyService(store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, sessions := range [][]SessionRef{
		nil,
		{{Client: " ", SessionID: "session"}},
		{{Client: "codex", SessionID: "session"}, {Client: "codex", SessionID: "session"}},
	} {
		_, err := service.SetSessions(context.Background(), "owner@example.com", sessions, true)
		if !errors.Is(err, ErrInvalidSessionSelection) {
			t.Fatalf("SetSessions(%+v) error = %v, want ErrInvalidSessionSelection", sessions, err)
		}
	}
}

func TestPrivacyServiceRejectsCrossOwnerSelectionAtomically(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	mine := change(1, "mine", "me@example.com", "codex")
	theirs := change(2, "theirs", "other@example.com", "codex")
	if _, err := store.UpsertChanges(ctx, []SessionChange{mine, theirs}, admitAll); err != nil {
		t.Fatal(err)
	}
	service, err := NewPrivacyService(store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.SetSessions(ctx, mine.Owner, []SessionRef{
		{Client: mine.Client, SessionID: mine.SessionID},
		{Client: theirs.Client, SessionID: theirs.SessionID},
	}, true)
	if err == nil {
		t.Fatal("cross-owner selection unexpectedly succeeded")
	}
	page, err := service.ListSessions(ctx, mine.Owner, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].OwnerOnly || page.Sessions[0].PrivacyExplicit {
		t.Fatalf("valid selection was not rolled back: %+v", page.Sessions)
	}
}

func TestPrivacyServicePerSessionChoiceOverridesDeploymentDefault(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	owner := "selected@example.com"
	one := change(1, "one", owner, "codex")
	two := change(2, "two", owner, "codex")
	ownerScope := events.ScopeOwner(owner)
	upsert := func(changes []SessionChange, cursor int64) {
		t.Helper()
		if _, err := store.upsertScopedAt(ctx, changes, cursor, func(SessionChange) ChangePolicy {
			return ChangePolicy{Admitted: true, VisibilityScope: ownerScope}
		}); err != nil {
			t.Fatal(err)
		}
	}
	upsert([]SessionChange{one, two}, 2)
	service, err := NewPrivacyService(store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetSessions(ctx, owner, []SessionRef{{Client: one.Client, SessionID: one.SessionID}}, false); err != nil {
		t.Fatal(err)
	}
	one.ChangeSeq = 3
	two.ChangeSeq = 4
	upsert([]SessionChange{one, two}, 4)
	page, err := service.ListSessions(ctx, owner, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]OwnerSession, len(page.Sessions))
	for _, session := range page.Sessions {
		states[session.SessionID] = session
	}
	if states[one.SessionID].OwnerOnly || !states[one.SessionID].PrivacyExplicit {
		t.Fatalf("explicit internal session = %+v", states[one.SessionID])
	}
	if !states[two.SessionID].OwnerOnly || states[two.SessionID].PrivacyExplicit {
		t.Fatalf("deployment-default owner-only session = %+v", states[two.SessionID])
	}
}

func TestPrivacyServiceListsOnlyTitleForCurrentSummary(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	change := change(1, "titled", "me@example.com", "codex")
	if _, err := store.UpsertChanges(ctx, []SessionChange{change}, admitAll); err != nil {
		t.Fatal(err)
	}
	summary := SessionSummaryDoc{
		Identity: Identity{Owner: change.Owner, Client: change.Client, SessionID: change.SessionID},
		Revision: "sha256:one", InputDigest: "sha256:input-one", Narrative: "First narrative",
	}
	if err := store.UpsertSessionSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE session_ingest_summaries
		   SET title='Repair privacy controls', title_input_digest=input_digest,
		       title_attempted_at=now(), title_failure_count=3, title_last_error='old failure'
		 WHERE owner=$1 AND client=$2 AND session_id=$3
	`, change.Owner, change.Client, change.SessionID); err != nil {
		t.Fatal(err)
	}
	service, err := NewPrivacyService(store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.ListSessions(ctx, change.Owner, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].Title != "Repair privacy controls" {
		t.Fatalf("current title page = %+v", page)
	}

	summary.Revision = "sha256:two"
	summary.InputDigest = "sha256:input-two"
	summary.Narrative = "Second narrative"
	if err := store.UpsertSessionSummary(ctx, summary); err != nil {
		t.Fatal(err)
	}
	page, err = service.ListSessions(ctx, change.Owner, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 || page.Sessions[0].Title != "" {
		t.Fatalf("stale title was exposed: %+v", page)
	}
	var attempted *time.Time
	var failures int
	var lastError *string
	if err := pool.QueryRow(ctx, `
		SELECT title_attempted_at, title_failure_count, title_last_error
		  FROM session_ingest_summaries
		 WHERE owner=$1 AND client=$2 AND session_id=$3
	`, change.Owner, change.Client, change.SessionID).Scan(&attempted, &failures, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempted != nil || failures != 0 || lastError != nil {
		t.Fatalf("replacement did not reset title retry state: attempted=%v failures=%d error=%v", attempted, failures, lastError)
	}
}

func TestPrivacyServiceListsOnlyOwnerWithPagination(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	changes := []SessionChange{
		change(1, "mine-old", "me@example.com", "codex"),
		change(2, "theirs", "other@example.com", "codex"),
		change(3, "mine-new", "me@example.com", "claude"),
	}
	if _, err := store.UpsertChanges(ctx, changes, admitAll); err != nil {
		t.Fatal(err)
	}
	service, err := NewPrivacyService(store, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ListSessions(ctx, "me@example.com", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Sessions) != 1 || first.Sessions[0].SessionID != "mine-new" || first.NextBefore != 3 {
		t.Fatalf("first page = %+v", first)
	}
	second, err := service.ListSessions(ctx, "me@example.com", 1, first.NextBefore)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Sessions) != 1 || second.Sessions[0].SessionID != "mine-old" || second.NextBefore != 0 {
		t.Fatalf("second page = %+v", second)
	}
}
