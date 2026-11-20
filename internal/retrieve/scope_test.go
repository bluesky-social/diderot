package retrieve

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
)

func TestProtectedAndUnstampedScopes(t *testing.T) {
	pool, store, emb := setupWorld(t)
	retriever := NewRetriever(pool, emb, store, nil)
	ctx := context.Background()
	const question = "protected ticket contents"
	var protectedID int64
	for index, scope := range []string{events.ScopeZendesk, events.ScopeUnstamped} {
		id := seedEvent(t, pool, store, emb, events.Event{
			Source: "zendesk", ExternalID: fmt.Sprintf("ticket:%d", index),
			OccurredAt: time.Now(), Body: question, Raw: json.RawMessage(`{}`), Scope: scope,
		})
		if scope == events.ScopeZendesk {
			protectedID = id
		}
	}
	for _, scopes := range [][]string{nil, {}, {"", events.ScopeUnstamped}, {events.ScopePublic, events.ScopeInternal}, {events.ScopeZendesk, events.ScopeUnstamped, ""}} {
		want := 0
		if events.ScopeAllowed(events.ScopeZendesk, scopes) {
			want = 1
		}
		result, err := retriever.Retrieve(ctx, Request{Question: question, Filters: Filters{AllowedScopes: scopes}})
		if err != nil || len(result.Hits) != want {
			t.Fatalf("retrieve scopes=%v hits=%v err=%v", scopes, result, err)
		}
		listed, err := retriever.List(ctx, ListRequest{Sources: []string{"zendesk"}, AllowedScopes: scopes})
		if err != nil || len(listed.Hits) != want {
			t.Fatalf("list scopes=%v hits=%v err=%v", scopes, listed, err)
		}
	}
	var chunkID uint64
	if err := pool.QueryRow(ctx, `SELECT id FROM event_chunks WHERE event_id=$1 LIMIT 1`, protectedID).Scan(&chunkID); err != nil {
		t.Fatal(err)
	}
	for _, scopes := range [][]string{nil, {}, {events.ScopePublic}, {events.ScopeUnstamped}} {
		hits, err := retriever.dereference(ctx, []qdrantstore.Hit{{ChunkID: chunkID, EventID: protectedID}}, scopes)
		if err != nil || len(hits) != 0 {
			t.Fatalf("SQL accepted ANN hit outside snapshot: scopes=%v hits=%v err=%v", scopes, hits, err)
		}
	}
}

// Scope filtering integration tests — the Level-0 visibility
// boundary across both retrieval legs (qdrant pre-ANN filter +
// Postgres dereference) and the List path.

func TestRetrieveScopeFiltering(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	ctx := context.Background()

	const question = "what did the legal team decide about the vendor contract"
	pubID := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "CPUB:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "alice", ActorID: "U1",
		Body:  "what did the legal team decide about the vendor contract (public copy)",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopePublic,
	})
	privID := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "CLEGAL:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "bob", ActorID: "U2",
		Body:  "what did the legal team decide about the vendor contract (private copy)",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopeChannel("CLEGAL"),
	})

	find := func(hits []Hit, id int64) bool {
		for _, h := range hits {
			if h.EventID == id {
				return true
			}
		}
		return false
	}

	res, err := r.Retrieve(ctx, Request{Question: question})
	if err != nil {
		t.Fatalf("unrestricted retrieve: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("nil scopes must deny all events; hits=%+v", res.Hits)
	}

	// 2. Public-channel ask: private-channel hit invisible.
	res, err = r.Retrieve(ctx, Request{Question: question, Filters: Filters{
		AllowedScopes: []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CPUB")},
	}})
	if err != nil {
		t.Fatalf("public-scoped retrieve: %v", err)
	}
	if !find(res.Hits, pubID) {
		t.Errorf("public event missing from public-channel ask")
	}
	if find(res.Hits, privID) {
		t.Errorf("LEAK: private-channel event surfaced in a public-channel ask")
	}

	// 3. Ask from the private channel itself: both visible.
	res, err = r.Retrieve(ctx, Request{Question: question, Filters: Filters{
		AllowedScopes: []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CLEGAL")},
	}})
	if err != nil {
		t.Fatalf("private-scoped retrieve: %v", err)
	}
	if !find(res.Hits, pubID) || !find(res.Hits, privID) {
		t.Errorf("ask in the private channel should see public + own-channel events; hits=%+v", res.Hits)
	}
}

func TestRetrieveScopeDereferenceBackstop(t *testing.T) {
	// The qdrant payload is a denormalized copy that can lag the
	// authoritative events.scope (sweep cadence). Simulate the lag:
	// seed as public (payload says public), then flip the Postgres
	// scope to a private channel WITHOUT patching the payload. The
	// dereference clause must drop the hit even though qdrant let
	// it through.
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	ctx := context.Background()

	const question = "incident retro for the database outage last tuesday"
	id := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "CX:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "alice", ActorID: "U1",
		Body:  "incident retro for the database outage last tuesday",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopePublic,
	})
	if _, err := pool.Exec(ctx,
		`UPDATE events SET scope = $1 WHERE id = $2`,
		events.ScopeChannel("CX"), id); err != nil {
		t.Fatalf("flip scope: %v", err)
	}

	res, err := r.Retrieve(ctx, Request{Question: question, Filters: Filters{
		AllowedScopes: []string{events.ScopePublic, events.ScopeInternal},
	}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	for _, h := range res.Hits {
		if h.EventID == id {
			t.Fatalf("LEAK: stale qdrant payload let a now-private event through the dereference backstop")
		}
	}
}

func TestRetrieveScopeSetScopePatch(t *testing.T) {
	// Store.SetScope is the sweep's qdrant half: patch the payload,
	// no re-embed. After patching, the vector leg itself excludes
	// the point (not just the dereference backstop).
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	ctx := context.Background()

	const question = "quarterly budget planning spreadsheet discussion"
	id := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "CY:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "alice", ActorID: "U1",
		Body:  "quarterly budget planning spreadsheet discussion",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopePublic,
	})
	// Flip Postgres AND patch the payload (full sweep behavior).
	if _, err := pool.Exec(ctx,
		`UPDATE events SET scope = $1 WHERE id = $2`,
		events.ScopeChannel("CY"), id); err != nil {
		t.Fatalf("flip scope: %v", err)
	}
	var chunkIDs []uint64
	rows, err := pool.Query(ctx, `SELECT id FROM event_chunks WHERE event_id = $1`, id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			t.Fatal(err)
		}
		chunkIDs = append(chunkIDs, uint64(cid))
	}
	rows.Close()
	if err := store.SetScope(ctx, chunkIDs, events.ScopeChannel("CY")); err != nil {
		t.Fatalf("SetScope: %v", err)
	}

	// Public ask: gone. Own-channel ask: visible.
	res, err := r.Retrieve(ctx, Request{Question: question, Filters: Filters{
		AllowedScopes: []string{events.ScopePublic},
	}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("patched point still visible to public ask: %+v", res.Hits)
	}
	res, err = r.Retrieve(ctx, Request{Question: question, Filters: Filters{
		AllowedScopes: []string{events.ScopePublic, events.ScopeChannel("CY")},
	}})
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].EventID != id {
		t.Errorf("patched point should be visible to own-channel ask; hits=%+v", res.Hits)
	}
}

func TestListScopeFiltering(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "CPUB:1", OccurredAt: now,
		Actor: "a", Body: "public note", Scope: events.ScopePublic,
	})
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "CLEGAL:1", OccurredAt: now,
		Actor: "b", Body: "private legal note", Scope: events.ScopeChannel("CLEGAL"),
	})
	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "CLOST:1", OccurredAt: now,
		Actor: "c", Body: "missed the stamp", Scope: events.ScopeUnstamped,
	})

	// Public-channel ask: only the public row.
	res, err := r.List(ctx, ListRequest{
		Sources:       []string{"slack"},
		AllowedScopes: []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CPUB")},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].ExternalID != "CPUB:1" {
		t.Errorf("public-channel list should return exactly the public row; hits=%+v", res.Hits)
	}

	// Legal-channel ask: public + own private. Unstamped never.
	res, err = r.List(ctx, ListRequest{
		Sources:       []string{"slack"},
		AllowedScopes: []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CLEGAL")},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Hits) != 2 {
		t.Errorf("legal-channel list: got %d hits, want 2 (public + own); hits=%+v", len(res.Hits), res.Hits)
	}
	for _, h := range res.Hits {
		if h.ExternalID == "CLOST:1" {
			t.Errorf("LEAK: unstamped row surfaced")
		}
	}

	res, err = r.List(ctx, ListRequest{Sources: []string{"slack"}})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Hits) != 0 {
		t.Errorf("nil scopes list: got %d hits, want 0", len(res.Hits))
	}
}
