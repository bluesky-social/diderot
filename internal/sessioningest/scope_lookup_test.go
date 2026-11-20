package sessioningest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/events"
)

func TestScopeLookupPreservesSessionGuards(test *testing.T) {
	for _, sessionID := range []string{"01900000-0000-7000-8000-000000000002", `session_%\é`} {
		test.Run(sessionID, func(test *testing.T) {
			store, _, pool := newPublishTestStore(test)
			ctx := test.Context()
			id := Identity{Owner: "owner@example.com", Client: "codex", SessionID: sessionID}
			wantScope := events.ScopeOwner(id.Owner)
			fixtures := []struct {
				name       string
				source     string
				externalID string
				kind       string
				deleted    bool
				scope      string
				wantScope  string
			}{
				{"final", "agent_session", externalID(id), "other", false, "internal", wantScope},
				{"chunk", "agent_session", rangeExternalID(id, 0, 1), chunkEventKind, false, "internal", wantScope},
				{"already scoped", "agent_session", rangeExternalID(id, 1, 2), chunkEventKind, false, wantScope, wantScope},
				{"deleted chunk", "agent_session", rangeExternalID(id, 2, 3), chunkEventKind, true, "internal", "internal"},
				{"wrong kind", "agent_session", rangeExternalID(id, 3, 4), "legacy", false, "internal", "internal"},
				{"missing kind", "agent_session", rangeExternalID(id, 4, 5), "", false, "internal", "internal"},
				{"other session", "agent_session", sessionID + "-extra:records:0:1", chunkEventKind, false, "internal", "internal"},
				{"prefix collision", "agent_session", sessionID + ":records-extra:0:1", chunkEventKind, false, "internal", "internal"},
				{"legacy identity", "agent_session", "owner/codex/" + rangeExternalID(id, 0, 1), chunkEventKind, false, "internal", "internal"},
				{"other source final", "slack", externalID(id), "native_session", false, "public", "public"},
				{"other source chunk", "slack", rangeExternalID(id, 0, 1), chunkEventKind, false, "public", "public"},
			}
			for _, fixture := range fixtures {
				if _, err := pool.Exec(ctx, `
					INSERT INTO events (source, external_id, occurred_at, body, raw, deleted, scope)
					VALUES ($1, $2, now(), 'fixture', jsonb_build_object('event_kind', $3::text), $4, $5)
				`, fixture.source, fixture.externalID, fixture.kind, fixture.deleted, fixture.scope); err != nil {
					test.Fatal(err)
				}
			}
			update := change(1, sessionID, id.Owner, id.Client)
			if _, err := store.upsertScopedAt(ctx, []SessionChange{update}, 1, func(SessionChange) ChangePolicy {
				return ChangePolicy{Admitted: true, VisibilityScope: wantScope}
			}); err != nil {
				test.Fatal(err)
			}
			for _, fixture := range fixtures {
				var scope string
				var deleted bool
				if err := pool.QueryRow(ctx, `SELECT scope, deleted FROM events WHERE source=$1 AND external_id=$2`, fixture.source, fixture.externalID).Scan(&scope, &deleted); err != nil {
					test.Fatal(err)
				}
				if scope != fixture.wantScope || deleted != fixture.deleted {
					test.Errorf("%s: scope=%q deleted=%v, want scope=%q deleted=%v", fixture.name, scope, deleted, fixture.wantScope, fixture.deleted)
				}
			}
			if _, err := pool.Exec(ctx, `UPDATE events SET deleted=true, scope='internal' WHERE source='agent_session' AND external_id=$1`, externalID(id)); err != nil {
				test.Fatal(err)
			}
			update.ChangeSeq = 2
			if _, err := store.upsertScopedAt(ctx, []SessionChange{update}, 2, func(SessionChange) ChangePolicy {
				return ChangePolicy{Admitted: true, VisibilityScope: wantScope}
			}); err != nil {
				test.Fatal(err)
			}
			var deletedFinalScope string
			if err := pool.QueryRow(ctx, `SELECT scope FROM events WHERE source='agent_session' AND external_id=$1`, externalID(id)).Scan(&deletedFinalScope); err != nil {
				test.Fatal(err)
			}
			if deletedFinalScope != "internal" {
				test.Fatalf("deleted final scope=%q, want internal", deletedFinalScope)
			}
		})
	}
}

func TestScopeLookupUsesPrefixIndexWithGenericPlan(test *testing.T) {
	_, _, pool := newPublishTestStore(test)
	ctx := test.Context()
	if _, err := pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope)
		SELECT 'agent_session', md5(ordinal::text) || ':records:0:1', now(), 'fixture',
		       '{"event_kind":"native_session_chunk"}'::jsonb, 'internal'
		  FROM generate_series(1, 20000) AS ordinal;
		ANALYZE events;
	`); err != nil {
		test.Fatal(err)
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Release()
	transaction, err := connection.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if _, err := transaction.Exec(ctx, `SET LOCAL plan_cache_mode=force_generic_plan`); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Prepare(ctx, "scope_lookup_regression", restampSessionScopeSQL); err != nil {
		test.Fatal(err)
	}
	defer func() { _ = connection.Conn().Deallocate(ctx, "scope_lookup_regression") }()
	var planJSON []byte
	if err := transaction.QueryRow(ctx, `
		EXPLAIN (FORMAT JSON) EXECUTE scope_lookup_regression(
		    'owner:test', 'missing:session', 'missing:records:', 'native_session_chunk'
		)
	`).Scan(&planJSON); err != nil {
		test.Fatal(err)
	}
	var plans []struct {
		Plan scopeLookupPlan
	}
	if err := json.Unmarshal(planJSON, &plans); err != nil {
		test.Fatal(err)
	}
	prefixIndexUsed := false
	var checkPlan func(scopeLookupPlan)
	checkPlan = func(plan scopeLookupPlan) {
		if plan.NodeType == "Seq Scan" && plan.RelationName == "events" {
			test.Errorf("scope lookup scans events: %s", planJSON)
		}
		if plan.IndexName == "idx_events_agent_session_external_id_prefix" && strings.Contains(plan.IndexCond, "^@") {
			prefixIndexUsed = true
		}
		for _, child := range plan.Plans {
			checkPlan(child)
		}
	}
	for _, plan := range plans {
		checkPlan(plan.Plan)
	}
	if !prefixIndexUsed {
		test.Fatalf("scope lookup does not use the prefix index: %s", planJSON)
	}
}

type scopeLookupPlan struct {
	NodeType     string            `json:"Node Type"`
	RelationName string            `json:"Relation Name"`
	IndexName    string            `json:"Index Name"`
	IndexCond    string            `json:"Index Cond"`
	Plans        []scopeLookupPlan `json:"Plans"`
}
