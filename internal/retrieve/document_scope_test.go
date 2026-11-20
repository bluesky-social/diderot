package retrieve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
)

// seedDocumentChunk writes and indexes a document chunk without running the
// extraction worker.
func seedDocumentChunk(
	t *testing.T,
	pool *pgxpool.Pool,
	store *qdrantstore.Store,
	emb *deterministicEmbedder,
	eventID int64,
	filename string,
	text string,
) int64 {
	t.Helper()
	ctx := context.Background()

	var docID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_documents
			(event_id, source_file_id, occurred_at, filename, media_type, doc_format,
			 extract_model, extracted_at, page_count, pages_extracted)
		SELECT $1, $2, e.occurred_at, $3, 'application/pdf', 'pdf',
		       'test-model', now(), 1, 1
		  FROM events e WHERE e.id = $1
		RETURNING id
	`, eventID, "F-"+filename, filename).Scan(&docID); err != nil {
		t.Fatal(err)
	}

	var chunkID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_chunks
			(event_id, kind, chunk_idx, content, context_prefix, event_document_id,
			 embedded_at, embedder_id, occurred_at)
		VALUES ($1, 'document_text', 0, $2, $3, $4, now(), $5, now())
		RETURNING id
	`, eventID, text, "[document] "+filename, docID, emb.id).Scan(&chunkID); err != nil {
		t.Fatal(err)
	}

	// Index it, carrying the parent event's scope onto the point exactly
	// as the embedder does. A point with an empty scope never matches a
	// scoped search (deny-by-default), so getting this from the DB
	// rather than from a literal is part of what's being tested.
	var (
		source, scope string
		actorID       *string
		occurredAt    time.Time
	)
	if err := pool.QueryRow(ctx, `
		SELECT source, scope, actor_id, occurred_at FROM events WHERE id = $1
	`, eventID).Scan(&source, &scope, &actorID, &occurredAt); err != nil {
		t.Fatal(err)
	}
	vecs, err := emb.Embed(ctx, []string{text})
	if err != nil {
		t.Fatal(err)
	}
	point := qdrantstore.Point{
		ID: uint64(chunkID), Vector: vecs[0],
		EventID: eventID, Source: source, OccurredAt: occurredAt, Scope: scope,
	}
	if actorID != nil {
		point.ActorID = *actorID
	}
	if err := store.Upsert(ctx, []qdrantstore.Point{point}); err != nil {
		t.Fatal(err)
	}
	return chunkID
}

// TestDocumentChunkScopeIsolation verifies that a private document is visible
// only to unrestricted retrieval and its owning channel.
func TestDocumentChunkScopeIsolation(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	ctx := context.Background()

	// The distinctive string only the private document contains.
	const question = "subscriber records retention eighteen months escalation legal-ops"
	const privateDocText = "subscriber records retention eighteen months escalation legal-ops contact for court orders"

	privEventID := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "CLEGAL:2.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "chester", ActorID: "U9",
		Body:  "here is the request document",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopeChannel("CLEGAL"),
	})
	docChunkID := seedDocumentChunk(t, pool, store, emb, privEventID, "document.pdf", privateDocText)

	findChunk := func(hits []Hit, id int64) bool {
		for _, h := range hits {
			if h.ChunkID == uint64(id) {
				return true
			}
		}
		return false
	}

	// 1. Admin / unrestricted: the document chunk is retrievable, which
	// proves the rest of the test isn't passing vacuously because the
	// chunk was never indexed.
	res, err := r.Retrieve(ctx, Request{Question: question, Filters: Filters{AllowedScopes: []string{"public", "internal", "slack-channel:CLEGAL"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !findChunk(res.Hits, docChunkID) {
		t.Fatalf("unrestricted retrieve must see the document chunk (else the negative cases are vacuous); hits=%+v", res.Hits)
	}

	// 2. Asked from a DIFFERENT public channel: invisible.
	res, err = r.Retrieve(ctx, Request{Question: question, Filters: Filters{
		AllowedScopes: []string{
			events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CPUBLIC"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if findChunk(res.Hits, docChunkID) {
		t.Error("LEAK: private-channel document text visible from a public channel ask")
	}

	// 3. Asked from a different PRIVATE channel: still invisible
	// (cross-private isolation, not just public-vs-private).
	res, err = r.Retrieve(ctx, Request{Question: question, Filters: Filters{
		AllowedScopes: []string{
			events.ScopePublic, events.ScopeInternal, events.ScopeChannel("COTHERPRIV"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if findChunk(res.Hits, docChunkID) {
		t.Error("LEAK: document text visible from a different private channel")
	}

	// 4. Asked from the owning channel: visible. Without this the test
	// could pass by filtering everything.
	res, err = r.Retrieve(ctx, Request{Question: question, Filters: Filters{
		AllowedScopes: []string{
			events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CLEGAL"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !findChunk(res.Hits, docChunkID) {
		t.Errorf("document text must be retrievable in its own channel; hits=%+v", res.Hits)
	}
}

// TestDocumentChunkCarriesProvenance verifies document attribution metadata.
func TestDocumentChunkCarriesProvenance(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	ctx := context.Background()

	const question = "provenance marker phrase retention eighteen months contract"
	eventID := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "CPROV:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "chester", ActorID: "U9",
		Body:  "see attached",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopePublic,
	})
	docChunkID := seedDocumentChunk(t, pool, store, emb, eventID, "contract.pdf",
		"provenance marker phrase retention eighteen months contract terms")

	res, err := r.Retrieve(ctx, Request{Question: question, Filters: Filters{AllowedScopes: []string{"public", "internal", "slack-channel:CLEGAL"}}})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, h := range res.Hits {
		if h.ChunkID != uint64(docChunkID) {
			continue
		}
		found = true
		if h.Kind != "document_text" {
			t.Errorf("Kind = %q, want document_text", h.Kind)
		}
		if h.DocumentFilename != "contract.pdf" {
			t.Errorf("DocumentFilename = %q, want contract.pdf", h.DocumentFilename)
		}
		if h.DocumentCompleteness != "complete" || h.DocumentPagesExtracted != 1 {
			t.Errorf("document coverage = %q %d/%d, want complete 1/1",
				h.DocumentCompleteness, h.DocumentPagesExtracted, h.DocumentPageCount)
		}
		// The actor is still the sharer — that's correct and useful, but
		// only safe to render alongside the document attribution.
		if h.Actor != "chester" {
			t.Errorf("Actor = %q, want chester (the sharer)", h.Actor)
		}
	}
	if !found {
		t.Fatalf("document chunk not retrieved; hits=%+v", res.Hits)
	}

	// A body-text hit on the same event must NOT claim document
	// provenance, or every message would look like a document.
	for _, h := range res.Hits {
		if h.EventID == eventID && h.ChunkID != uint64(docChunkID) {
			if h.Kind != "text" {
				t.Errorf("body chunk Kind = %q, want text", h.Kind)
			}
			if h.DocumentFilename != "" {
				t.Errorf("body chunk claims document %q", h.DocumentFilename)
			}
		}
	}
}

// TestDocumentChunkReembedPreservesText verifies that a model refresh marks
// extracted text for re-embedding without deleting it.
func TestDocumentChunkReembedPreservesText(t *testing.T) {
	pool, store, emb := setupWorld(t)
	ctx := context.Background()

	eventID := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "CREEMBED:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "alice", ActorID: "U1",
		Body:  "original body",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopePublic,
	})
	docChunkID := seedDocumentChunk(t, pool, store, emb, eventID, "a.pdf",
		"reembed marker text that must survive a model bump")

	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	if _, err := sink.Upsert(ctx, events.Event{
		Source: "slack", ExternalID: "CREEMBED:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "alice", ActorID: "U1",
		Body:  "original body",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopePublic,
		// The reembed path's distinguishing flag.
		ForceUpdate: true,
	}); err != nil {
		t.Fatal(err)
	}

	var content string
	var embeddedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT content, embedded_at FROM event_chunks WHERE id = $1
	`, docChunkID).Scan(&content, &embeddedAt); err != nil {
		t.Fatalf("document chunk gone after a force-update reembed: %v", err)
	}
	if !strings.Contains(content, "reembed marker text") {
		t.Errorf("document text was rewritten: %q", content)
	}
	// embedded_at must be cleared so the (kind-agnostic) embedder claim
	// query re-embeds it with the new model.
	if embeddedAt != nil {
		t.Error("embedded_at not cleared; document vectors would stay on the old model")
	}
}

// TestDocumentChunkUnstampedInvisible verifies deny-by-default scope handling.
func TestDocumentChunkUnstampedInvisible(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	ctx := context.Background()

	const question = "unstamped document retention schedule marker phrase"
	eventID := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "CUNK:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "someone", ActorID: "U3",
		Body:  "attachment on an unstamped event",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopeUnstamped,
	})
	docChunkID := seedDocumentChunk(t, pool, store, emb, eventID,
		"secret.pdf", "unstamped document retention schedule marker phrase inside")

	for _, scopes := range [][]string{
		{events.ScopePublic, events.ScopeInternal},
		{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("CUNK")},
	} {
		res, err := r.Retrieve(ctx, Request{Question: question, Filters: Filters{AllowedScopes: scopes}})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range res.Hits {
			if h.ChunkID == uint64(docChunkID) {
				t.Errorf("LEAK: unstamped document chunk visible with scopes %v", scopes)
			}
		}
	}
}

// TestDocumentChunkSurvivesBodyReupsert verifies that editing a message does
// not delete chunks owned by its documents.
func TestDocumentChunkSurvivesBodyReupsert(t *testing.T) {
	pool, store, emb := setupWorld(t)
	ctx := context.Background()

	eventID := seedEvent(t, pool, store, emb, events.Event{
		Source: "slack", ExternalID: "CEDIT:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "alice", ActorID: "U1",
		Body:  "original body",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopePublic,
	})
	docChunkID := seedDocumentChunk(t, pool, store, emb, eventID, "a.pdf", "document body text here")

	// Re-upsert with edited body text, as an edit or unfurl would.
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	if _, err := sink.Upsert(ctx, events.Event{
		Source: "slack", ExternalID: "CEDIT:1.1",
		OccurredAt: time.Unix(1700000000, 0), Actor: "alice", ActorID: "U1",
		Body:  "edited body with completely different text",
		Raw:   json.RawMessage(`{}`),
		Scope: events.ScopePublic,
	}); err != nil {
		t.Fatal(err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM event_chunks WHERE id = $1)`, docChunkID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("body re-upsert deleted the document_text chunk; the kind-scoped DELETE regressed")
	}
}
