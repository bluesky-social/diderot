package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/bluesky-social/diderot/internal/chunk"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestValidate is a pure-function test; no DB.
func TestValidate(t *testing.T) {
	okEvent := Event{
		Source:     "slack",
		ExternalID: "C123:1700000000.000100",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "hello",
		Scope:      ScopePublic,
	}
	if err := okEvent.validate(); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*Event)
	}{
		{"no source", func(e *Event) { e.Source = "" }},
		{"no external id", func(e *Event) { e.ExternalID = "" }},
		{"zero occurred at", func(e *Event) { e.OccurredAt = time.Time{} }},
		{"no body", func(e *Event) { e.Body = "" }},
		{"no scope", func(e *Event) { e.Scope = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := okEvent
			tc.mut(&e)
			if err := e.validate(); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

// TestRescope: every live event under a prefix takes the new scope in
// place, its chunks are queued for the Qdrant payload repair and keep their
// embeddings, and events outside the prefix or already deleted are left
// alone.
func TestRescope(t *testing.T) {
	pool := setupPool(t)
	ctx := context.Background()
	sink := NewDBSink(pool, nil)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"g/c1/1", "g/c1/2", "g/c10/3", "g/c1/4"} {
		if _, err := sink.Upsert(ctx, Event{Source: "discord", ExternalID: id, OccurredAt: at, Body: "words " + id, Scope: ScopeInternal}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sink.Delete(ctx, "discord", "g/c1/4"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE event_chunks SET embedded_at = now()`); err != nil {
		t.Fatal(err)
	}
	changed, err := sink.Rescope(ctx, "discord", "g/c1/", "corpus:mods")
	if err != nil || changed != 2 {
		t.Fatalf("Rescope = %d, %v; want 2", changed, err)
	}
	rows, err := pool.Query(ctx, `
		SELECT e.external_id, e.scope, bool_and(c.scope_sync_needed), bool_and(c.embedded_at IS NOT NULL)
		  FROM events e JOIN event_chunks c ON c.event_id = e.id
		 GROUP BY e.id, 1, 2 ORDER BY e.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, scope string
		var queued, embedded bool
		if err := rows.Scan(&id, &scope, &queued, &embedded); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s %s queued=%v embedded=%v", id, scope, queued, embedded))
	}
	want := []string{
		"g/c1/1 corpus:mods queued=true embedded=true",
		"g/c1/2 corpus:mods queued=true embedded=true",
		"g/c10/3 internal queued=false embedded=true",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("after Rescope:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if changed, err := sink.Rescope(ctx, "discord", "g/c1/", "corpus:mods"); err != nil || changed != 0 {
		t.Fatalf("a second Rescope = %d, %v; want 0", changed, err)
	}
}

// testDBURL returns the pgx URL for this package's isolated test
// database (a per-package clone of the migrated base; see
// internal/testdb). Skips the test if DIDEROT_TEST_DB_URL isn't set.
func testDBURL(t *testing.T) string {
	t.Helper()
	return testdb.URL(t, "events")
}

// setupPool dials the test database, runs TRUNCATE on the two
// tables we care about (so tests start clean regardless of prior
// runs), and returns the pool + a cleanup func.
func setupPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, testDBURL(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `TRUNCATE events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func TestUpsertInsert(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())

	ctx := context.Background()
	res, err := sink.Upsert(ctx, Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0),
		Actor:      "alice",
		ActorID:    "U1",
		ThreadID:   "T1",
		Title:      "#ops",
		Body:       "k8s cluster is angry",
		URL:        "https://slack.example.com/archives/C1/p1",
		Raw:        json.RawMessage(`{"type":"message"}`),
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if res.WasUpdate {
		t.Fatal("first Upsert should be an INSERT, not UPDATE")
	}
	if res.Chunks != 1 {
		t.Fatalf("short body: got %d chunks, want 1", res.Chunks)
	}
	if res.EventID == 0 {
		t.Fatal("EventID not populated")
	}

	// Row is visible with the fields we inserted.
	var (
		deleted   bool
		updatedAt *time.Time
		body      string
		numChunks int
	)
	err = pool.QueryRow(ctx, `
		SELECT e.deleted, e.updated_at, e.body,
		       (SELECT count(*) FROM event_chunks WHERE event_id = e.id)
		  FROM events e WHERE e.id = $1
	`, res.EventID).Scan(&deleted, &updatedAt, &body, &numChunks)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if deleted {
		t.Error("row should not be deleted after insert")
	}
	if updatedAt != nil {
		t.Error("updated_at should be NULL on insert")
	}
	if body != "k8s cluster is angry" {
		t.Errorf("body mismatch: %q", body)
	}
	if numChunks != 1 {
		t.Errorf("chunk rows: got %d, want 1", numChunks)
	}
}

func TestExactReferenceLookups(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()
	now := time.Unix(1700000000, 351689000)

	slackResult, err := sink.Upsert(ctx, Event{
		Scope: ScopeChannel("C_SAME"), Source: "slack", ExternalID: "C_SAME:1700000000.123456",
		OccurredAt: now, Body: "same channel", Raw: json.RawMessage(`{"type":"message"}`),
	})
	if err != nil {
		t.Fatalf("slack upsert: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_message_details (event_id, channel_id, occurred_at)
		VALUES ($1, 'C_SAME', $2)
	`, slackResult.EventID, now)
	if err != nil {
		t.Fatalf("slack details: %v", err)
	}
	legacySlack, err := sink.Upsert(ctx, Event{
		Scope: ScopeChannel("C_LEGACY"), Source: "slack", ExternalID: "C_LEGACY:1700000000.123457",
		OccurredAt: now, Body: "legacy exact message", URL: "https://slack.example/legacy",
		Raw: json.RawMessage(`{"type":"message"}`),
	})
	if err != nil {
		t.Fatalf("legacy slack upsert: %v", err)
	}

	linearResult, err := sink.Upsert(ctx, Event{
		Scope: ScopeInternal, Source: "linear", ExternalID: "linear-uuid-1", OccurredAt: now,
		Title: "ENG-770: exact issue", Body: "issue body", Raw: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("linear upsert: %v", err)
	}
	sessionID := "550e8400-e29b-41d4-a716-446655440000"
	sessionResult, err := sink.Upsert(ctx, Event{
		Scope: ScopeInternal, Source: "agent_session",
		ExternalID: "owner/codex/" + sessionID + ":session", SessionID: sessionID,
		RecordStart: 0, RecordEnd: 4, OccurredAt: now, Body: "session body", Raw: json.RawMessage(`{"owner":"owner"}`),
	})
	if err != nil {
		t.Fatalf("session upsert: %v", err)
	}

	chunkID := int64(0)
	if err := pool.QueryRow(ctx, `SELECT id FROM event_chunks WHERE event_id = $1`, linearResult.EventID).Scan(&chunkID); err != nil {
		t.Fatalf("chunk lookup: %v", err)
	}

	if got, err := sink.SlackEventID(ctx, "C_SAME", "1700000000.123456"); err != nil || got != slackResult.EventID {
		t.Fatalf("SlackEventID = %d, %v; want %d", got, err, slackResult.EventID)
	}
	if got, err := sink.SlackEventID(ctx, "C_LEGACY", "1700000000.123457"); err != nil || got != legacySlack.EventID {
		t.Fatalf("legacy SlackEventID = %d, %v; want %d", got, err, legacySlack.EventID)
	}
	if source, url, body, err := sink.SlackEventContent(ctx, legacySlack.EventID); err != nil || source != "slack" || url != "https://slack.example/legacy" || body != "legacy exact message" {
		t.Fatalf("legacy SlackEventContent = %q, %q, %q, %v", source, url, body, err)
	}
	if got, err := sink.LinearEventID(ctx, "eng-770"); err != nil || got != linearResult.EventID {
		t.Fatalf("LinearEventID = %d, %v; want %d", got, err, linearResult.EventID)
	}
	if got, err := sink.SessionEventID(ctx, sessionID); err != nil || got != sessionResult.EventID {
		t.Fatalf("SessionEventID = %d, %v; want %d", got, err, sessionResult.EventID)
	}
	if got, err := sink.EventIDReference(ctx, linearResult.EventID); err != nil || got != linearResult.EventID {
		t.Fatalf("EventIDReference = %d, %v; want %d", got, err, linearResult.EventID)
	}
	if got, err := sink.ChunkEventID(ctx, chunkID); err != nil || got != linearResult.EventID {
		t.Fatalf("ChunkEventID = %d, %v; want %d", got, err, linearResult.EventID)
	}

	_, err = sink.Upsert(ctx, Event{
		Scope: ScopeInternal, Source: "linear", ExternalID: "linear-uuid-2", OccurredAt: now,
		Title: "ENG-770: duplicate", Body: "duplicate", Raw: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("duplicate linear upsert: %v", err)
	}
	if _, err := sink.LinearEventID(ctx, "ENG-770"); !errors.Is(err, ErrReferenceAmbiguous) {
		t.Fatalf("duplicate LinearEventID error = %v, want ErrReferenceAmbiguous", err)
	}
	if _, err := sink.SlackEventID(ctx, "C_SAME", "1700000000.123457"); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("missing SlackEventID error = %v, want ErrEventNotFound", err)
	}
}

func TestLinearTeamKeys(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()
	now := time.Now().UTC()

	for i, ev := range []struct {
		source string
		title  string
	}{
		{source: "linear", title: "ENG-770: rotate keys"},
		{source: "linear", title: "ENG-771: second issue on the same team"},
		{source: "linear", title: "app2-5: lowercase identifier"},
		{source: "linear", title: "Untitled issue without an identifier"},
		{source: "linear", title: "GONE-1: deleted issue"},
		{source: "slack", title: "SHA-256: not a linear event"},
	} {
		externalID := fmt.Sprintf("team-key-%d", i)
		if _, err := sink.Upsert(ctx, Event{
			Scope: ScopeInternal, Source: ev.source, ExternalID: externalID,
			OccurredAt: now, Title: ev.title, Body: "body", Raw: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("upsert %q: %v", ev.title, err)
		}
		if strings.HasPrefix(ev.title, "GONE-") {
			if _, err := sink.Delete(ctx, ev.source, externalID); err != nil {
				t.Fatalf("delete %q: %v", ev.title, err)
			}
		}
	}

	keys, err := sink.LinearTeamKeys(ctx)
	if err != nil {
		t.Fatalf("LinearTeamKeys: %v", err)
	}
	slices.Sort(keys)
	if want := []string{"APP2", "ENG"}; !slices.Equal(keys, want) {
		t.Fatalf("LinearTeamKeys = %v, want %v", keys, want)
	}
}

func TestUpsertUpdate(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())

	ctx := context.Background()
	insert, err := sink.Upsert(ctx, Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "v1 text",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	var oldChunkID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM event_chunks WHERE event_id = $1`, insert.EventID,
	).Scan(&oldChunkID); err != nil {
		t.Fatalf("select old chunk id: %v", err)
	}

	// Edit: same (source, external_id), new body.
	update, err := sink.Upsert(ctx, Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "v2 text with more words",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !update.WasUpdate {
		t.Fatal("second Upsert should be WasUpdate=true")
	}
	if update.EventID != insert.EventID {
		t.Fatalf("event id changed on edit: %d -> %d", insert.EventID, update.EventID)
	}

	// Old chunk row is gone; a new one with a higher id exists.
	var (
		newBody   string
		newChunks int
		updatedAt *time.Time
	)
	err = pool.QueryRow(ctx, `
		SELECT e.body, e.updated_at,
		       (SELECT count(*) FROM event_chunks WHERE event_id = e.id)
		  FROM events e WHERE e.id = $1
	`, update.EventID).Scan(&newBody, &updatedAt, &newChunks)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if newBody != "v2 text with more words" {
		t.Errorf("body not updated: %q", newBody)
	}
	if updatedAt == nil {
		t.Error("updated_at should be non-NULL after an edit")
	}
	if newChunks != 1 {
		t.Errorf("expected 1 new chunk, got %d", newChunks)
	}

	// Old chunk id is no longer present.
	var exists bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM event_chunks WHERE id = $1)`, oldChunkID,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check old chunk: %v", err)
	}
	if exists {
		t.Error("old chunk row should have been deleted on edit")
	}
}

func TestUpsertChunksLongBody(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())

	// Long body: chunker should emit >1 chunks and we should see them
	// all land in the table with monotonic chunk_idx.
	body := strings.Repeat("word ", 1000) // ~5000 runes
	ctx := context.Background()
	res, err := sink.Upsert(ctx, Event{
		Scope:      ScopePublic,
		Source:     "ssh",
		ExternalID: "session-42",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       body,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if res.Chunks < 2 {
		t.Fatalf("long body: got %d chunks, want >=2", res.Chunks)
	}
	// Expected count matches chunker output.
	want := len(chunk.Chunk(body))
	if res.Chunks != want {
		t.Errorf("chunk count mismatch: got %d, want %d", res.Chunks, want)
	}

	// chunk_idx starts at 0 and is contiguous.
	rows, err := pool.Query(ctx,
		`SELECT chunk_idx FROM event_chunks WHERE event_id = $1 ORDER BY chunk_idx`,
		res.EventID,
	)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	var idxs []int
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			t.Fatalf("scan: %v", err)
		}
		idxs = append(idxs, i)
	}
	for i, v := range idxs {
		if v != i {
			t.Errorf("chunk_idx[%d] = %d, want %d", i, v, i)
		}
	}
}

func TestDeleteExistingTombstones(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())

	ctx := context.Background()
	ins, err := sink.Upsert(ctx, Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0),
		Title:      "#ops",
		Body:       strings.Repeat("word ", 500),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := sink.Delete(ctx, "slack", "C1:1.1")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !res.Existed {
		t.Fatal("Delete should report Existed=true")
	}
	if res.EventID != ins.EventID {
		t.Fatalf("Delete.EventID = %d, want %d", res.EventID, ins.EventID)
	}
	if res.Chunks != ins.Chunks {
		t.Errorf("Delete.Chunks = %d, want %d", res.Chunks, ins.Chunks)
	}

	// Row is tombstoned, chunk rows are gone.
	var (
		deleted   bool
		body      string
		title     *string
		numChunks int
	)
	err = pool.QueryRow(ctx, `
		SELECT e.deleted, e.body, e.title,
		       (SELECT count(*) FROM event_chunks WHERE event_id = e.id)
		  FROM events e WHERE e.id = $1
	`, ins.EventID).Scan(&deleted, &body, &title, &numChunks)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if !deleted {
		t.Error("row should be deleted=true")
	}
	if body != "" {
		t.Errorf("tombstoned body should be '', got %q", body)
	}
	if title != nil {
		t.Errorf("tombstoned title should be NULL, got %q", *title)
	}
	if numChunks != 0 {
		t.Errorf("expected 0 chunks after tombstone, got %d", numChunks)
	}
}

func TestDeleteMissingIsNoOp(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())

	ctx := context.Background()
	res, err := sink.Delete(ctx, "slack", "does-not-exist")
	if err != nil {
		t.Fatalf("Delete(missing): %v", err)
	}
	if res.Existed {
		t.Error("Delete of a missing event should report Existed=false")
	}
	if res.EventID != 0 || res.Chunks != 0 {
		t.Errorf("missing delete should be zero-valued, got %+v", res)
	}
}

func TestDeleteInTxFollowsCallerCommitOrRollback(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()
	res, err := sink.Upsert(ctx, Event{
		Scope: ScopeInternal, Source: "notion", ExternalID: "page-atomic",
		OccurredAt: time.Now(), Body: "still visible",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin rollback tx: %v", err)
	}
	if _, _, err := sink.DeleteInTx(ctx, tx, "notion", "page-atomic"); err != nil {
		t.Fatalf("DeleteInTx rollback: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	var deleted bool
	if err := pool.QueryRow(ctx, `SELECT deleted FROM events WHERE id = $1`, res.EventID).Scan(&deleted); err != nil {
		t.Fatalf("read after rollback: %v", err)
	}
	if deleted {
		t.Fatal("caller rollback did not roll back tombstone")
	}

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin commit tx: %v", err)
	}
	_, postCommit, err := sink.DeleteInTx(ctx, tx, "notion", "page-atomic")
	if err != nil {
		t.Fatalf("DeleteInTx commit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	postCommit(ctx)
	if err := pool.QueryRow(ctx, `SELECT deleted FROM events WHERE id = $1`, res.EventID).Scan(&deleted); err != nil {
		t.Fatalf("read after commit: %v", err)
	}
	if !deleted {
		t.Fatal("caller commit did not persist tombstone")
	}
}

func TestUpsertEmbeddingTextChunks(t *testing.T) {
	// EmbeddingText (when set) is what the chunker sees, not Body.
	// Verify by giving Body a single short string and EmbeddingText a
	// 5000-char string that should split into multiple chunks. The
	// resulting chunk contents should match EmbeddingText, not Body.
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	embedText := strings.Repeat("opensearch migration ", 250)
	res, err := sink.Upsert(ctx, Event{
		Scope:         ScopePublic,
		Source:        "agent_session",
		ExternalID:    "abc:0:5",
		OccurredAt:    time.Unix(1700000000, 0),
		Body:          "human-readable narrative",
		EmbeddingText: embedText,
		SessionID:     "abc",
		RecordStart:   0,
		RecordEnd:     5,
		Raw:           json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Chunks < 2 {
		t.Errorf("Chunks = %d, want at least 2 (EmbeddingText is long)", res.Chunks)
	}
	// Chunk content should be derived from EmbeddingText, not Body.
	var firstChunk string
	if err := pool.QueryRow(ctx,
		`SELECT content FROM event_chunks WHERE event_id = $1 ORDER BY chunk_idx LIMIT 1`,
		res.EventID,
	).Scan(&firstChunk); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstChunk, "opensearch") {
		t.Errorf("first chunk content = %q, expected EmbeddingText slice", firstChunk[:min(len(firstChunk), 80)])
	}
	// events.body still holds the human-readable narrative for
	// retrieval display.
	var body string
	if err := pool.QueryRow(ctx,
		`SELECT body FROM events WHERE id = $1`, res.EventID,
	).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if body != "human-readable narrative" {
		t.Errorf("events.body = %q, want narrative", body)
	}
	// Range fields are persisted.
	var sid string
	var ms, me int
	if err := pool.QueryRow(ctx,
		`SELECT session_id, record_start, record_end FROM events WHERE id = $1`, res.EventID,
	).Scan(&sid, &ms, &me); err != nil {
		t.Fatal(err)
	}
	if sid != "abc" || ms != 0 || me != 5 {
		t.Errorf("range fields = (%q, %d, %d), want (abc, 0, 5)", sid, ms, me)
	}
}

func TestUpsertNoSessionLeavesRangeFieldsNull(t *testing.T) {
	// Non-session sources: SessionID empty, record_start/record_end should
	// land as SQL NULL even if the int fields are zero-valued.
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	res, err := sink.Upsert(ctx, Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:2.2",
		OccurredAt: time.Unix(1700000001, 0),
		Body:       "hi",
		Raw:        json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var sid, et *string
	var ms, me *int
	if err := pool.QueryRow(ctx, `
		SELECT session_id, embedding_text, record_start, record_end
		FROM events WHERE id = $1`, res.EventID,
	).Scan(&sid, &et, &ms, &me); err != nil {
		t.Fatal(err)
	}
	if sid != nil || et != nil || ms != nil || me != nil {
		t.Errorf("range/embedding fields should all be NULL for non-agent_session, got sid=%v et=%v ms=%v me=%v",
			sid, et, ms, me)
	}
}

func TestUpsertContextPrefixStored(t *testing.T) {
	// The ContextPrefix field plumbs through to each chunk row's
	// context_prefix column. Every chunk produced by a single Upsert
	// shares the same prefix (the chunker doesn't know about it; it's
	// set at ingest time and uniform across the event).
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())

	ctx := context.Background()
	prefix := "Slack reply in #eng-ops to thread \"k8s cluster is angry\":"
	res, err := sink.Upsert(ctx, Event{
		Scope:         ScopePublic,
		Source:        "slack",
		ExternalID:    "C1:1.1",
		OccurredAt:    time.Unix(1700000000, 0),
		Body:          strings.Repeat("word ", 1000),
		ContextPrefix: prefix,
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if res.Chunks < 2 {
		t.Fatalf("expected multi-chunk body; got %d", res.Chunks)
	}

	rows, err := pool.Query(ctx,
		`SELECT context_prefix FROM event_chunks WHERE event_id = $1 ORDER BY chunk_idx`,
		res.EventID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s *string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == nil {
			t.Error("context_prefix should be non-NULL when set on Event")
		} else {
			got = append(got, *s)
		}
	}
	for i, p := range got {
		if p != prefix {
			t.Errorf("chunk %d: got %q, want %q", i, p, prefix)
		}
	}
}

func TestUpsertEmptyContextPrefixStoredAsNull(t *testing.T) {
	// "" ContextPrefix (the default) must land as SQL NULL, not ''.
	// Matters because the embedder treats NULL/empty identically but
	// the column semantics are "no prefix known" -- not "empty prefix
	// explicitly supplied." Distinguishing lets us surface "never-
	// prefixed rows" via a simple WHERE context_prefix IS NULL in
	// ops queries.
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())

	ctx := context.Background()
	res, err := sink.Upsert(ctx, Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	var s *string
	err = pool.QueryRow(ctx,
		`SELECT context_prefix FROM event_chunks WHERE event_id = $1`,
		res.EventID,
	).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Errorf("empty prefix should store as NULL, got %q", *s)
	}
}

func TestUpsertIdempotentOnUnchangedContent(t *testing.T) {
	// Backfill relies on re-runs being cheap. A re-Upsert of a
	// row whose content is byte-identical to the stored row must:
	//  - return NoChange=true, WasUpdate=false, same EventID
	//  - NOT bump updated_at
	//  - NOT delete + reinsert chunk rows (same ids as before)
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	ev := Event{
		Scope:         ScopePublic,
		Source:        "slack",
		ExternalID:    "C1:1.1",
		OccurredAt:    time.Unix(1700000000, 0),
		Actor:         "alice",
		ActorID:       "U1",
		ThreadID:      "T1",
		Title:         "",
		Body:          "hello world",
		URL:           "https://x/1",
		Raw:           json.RawMessage(`{"k":"v"}`),
		ContextPrefix: "Slack message in #ops:",
	}
	first, err := sink.Upsert(ctx, ev)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.NoChange || first.WasUpdate {
		t.Fatalf("first upsert should be an insert; got %+v", first)
	}

	var origChunkID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM event_chunks WHERE event_id = $1`,
		first.EventID,
	).Scan(&origChunkID); err != nil {
		t.Fatal(err)
	}

	// Re-upsert with identical content.
	second, err := sink.Upsert(ctx, ev)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !second.NoChange {
		t.Errorf("second upsert should NoChange=true; got %+v", second)
	}
	if second.WasUpdate {
		t.Error("second upsert should WasUpdate=false")
	}
	if second.EventID != first.EventID {
		t.Errorf("EventID drifted: %d -> %d", first.EventID, second.EventID)
	}

	// updated_at must still be NULL (not bumped by the no-op).
	var updatedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT updated_at FROM events WHERE id = $1`, first.EventID,
	).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt != nil {
		t.Errorf("updated_at should be NULL on no-op, got %v", updatedAt)
	}

	// Chunk row must be the same id (no delete+reinsert).
	var newChunkID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM event_chunks WHERE event_id = $1`, first.EventID,
	).Scan(&newChunkID); err != nil {
		t.Fatal(err)
	}
	if newChunkID != origChunkID {
		t.Errorf("chunk churn: id %d -> %d", origChunkID, newChunkID)
	}
}

func TestUpsertRawOnlyUpdatePreservesChunks(t *testing.T) {
	pool := setupPool(t)
	reg := prometheus.NewRegistry()
	sink := NewDBSink(pool, reg)
	ctx := context.Background()

	base := Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:raw-only",
		OccurredAt: time.Unix(1700000000, 0),
		Actor:      "alice",
		ActorID:    "U1",
		Body:       "the searchable text is unchanged",
		Raw:        json.RawMessage(`{"type":"message","text":"the searchable text is unchanged"}`),
	}
	first, err := sink.Upsert(ctx, base)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	var originalChunkID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM event_chunks WHERE event_id = $1`, first.EventID,
	).Scan(&originalChunkID); err != nil {
		t.Fatal(err)
	}

	// Model Slack's socket-vs-history shape mismatch: raw changes, but
	// every searchable and Qdrant-payload field remains identical.
	rawOnly := base
	rawOnly.Raw = json.RawMessage(`{"type":"message_changed","message":{"text":"the searchable text is unchanged"}}`)
	second, err := sink.Upsert(ctx, rawOnly)
	if err != nil {
		t.Fatalf("raw-only upsert: %v", err)
	}
	if !second.WasUpdate || second.NoChange {
		t.Fatalf("raw-only upsert should update the event row; got %+v", second)
	}
	if second.Chunks != 0 {
		t.Fatalf("raw-only upsert inserted %d chunks, want 0", second.Chunks)
	}

	var (
		currentChunkID int64
		rawMatches     bool
		updatedAt      *time.Time
	)
	if err := pool.QueryRow(ctx, `
		SELECT c.id, e.raw = $2::jsonb, e.updated_at
		  FROM events e
		  JOIN event_chunks c ON c.event_id = e.id
		 WHERE e.id = $1
	`, first.EventID, rawOnly.Raw).Scan(&currentChunkID, &rawMatches, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if currentChunkID != originalChunkID {
		t.Errorf("raw-only update churned chunk id %d -> %d", originalChunkID, currentChunkID)
	}
	if !rawMatches {
		t.Error("raw-only update did not persist the latest raw envelope")
	}
	if updatedAt == nil {
		t.Error("raw-only update did not bump events.updated_at")
	}

	if got := testutil.ToFloat64(sink.metrics.chunksInserted.WithLabelValues("slack")); got != 1 {
		t.Errorf("chunks inserted metric = %v, want 1", got)
	}
	if got := testutil.ToFloat64(sink.metrics.chunksDeleted.WithLabelValues("slack", "edit")); got != 0 {
		t.Errorf("chunks deleted metric = %v, want 0", got)
	}

	third, err := sink.Upsert(ctx, rawOnly)
	if err != nil {
		t.Fatalf("identical third upsert: %v", err)
	}
	if !third.NoChange || third.WasUpdate {
		t.Fatalf("identical third upsert should be a no-op; got %+v", third)
	}
}

func TestUpsertForceUpdateBypassesNoChange(t *testing.T) {
	// ForceUpdate=true triggers the UPDATE branch even when the
	// events-row content is byte-identical. Used by reembed.
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	base := Event{
		Scope:  ScopePublic,
		Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0), Body: "hello world",
	}
	first, err := sink.Upsert(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if first.WasUpdate || first.NoChange {
		t.Fatalf("first should be insert; got %+v", first)
	}
	origChunkID := func() int64 {
		var id int64
		_ = pool.QueryRow(ctx, `SELECT id FROM event_chunks WHERE event_id = $1`, first.EventID).Scan(&id)
		return id
	}()

	// Identical content without ForceUpdate -> NoChange.
	plain, err := sink.Upsert(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if !plain.NoChange {
		t.Fatalf("identical re-upsert should NoChange; got %+v", plain)
	}

	// Identical content WITH ForceUpdate -> WasUpdate, new chunks.
	forced := base
	forced.ForceUpdate = true
	got, err := sink.Upsert(ctx, forced)
	if err != nil {
		t.Fatal(err)
	}
	if !got.WasUpdate {
		t.Fatalf("force upsert should WasUpdate=true; got %+v", got)
	}
	if got.NoChange {
		t.Error("force upsert should NoChange=false")
	}
	var newChunkID int64
	_ = pool.QueryRow(ctx, `SELECT id FROM event_chunks WHERE event_id = $1`, first.EventID).Scan(&newChunkID)
	if newChunkID == origChunkID {
		t.Error("force upsert should have rewritten chunks; same id survived")
	}
}

func TestUpsertUpdateOnChangedContent(t *testing.T) {
	// Counterpart to the no-op test: an actual edit (changed body)
	// must WasUpdate=true and rewrite chunks.
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	base := Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "v1",
	}
	first, err := sink.Upsert(ctx, base)
	if err != nil {
		t.Fatal(err)
	}

	edit := base
	edit.Body = "v2 edited"
	second, err := sink.Upsert(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	if !second.WasUpdate {
		t.Fatal("edit should WasUpdate=true")
	}
	if second.NoChange {
		t.Fatal("edit should NoChange=false")
	}
	if second.EventID != first.EventID {
		t.Errorf("EventID drifted: %d -> %d", first.EventID, second.EventID)
	}

	// Body is the new value.
	var body string
	_ = pool.QueryRow(ctx, `SELECT body FROM events WHERE id=$1`, first.EventID).Scan(&body)
	if body != "v2 edited" {
		t.Errorf("body not updated: %q", body)
	}
}

func TestUpsertUpdatePreservesImageCaptionChunks(t *testing.T) {
	// Regression: re-chunking on an edit must delete only the body
	// (kind='text') chunks, NOT the out-of-band image_caption chunks
	// the caption worker wrote. A blanket delete orphaned the image
	// (captioned_at set, chunk_id NULL) so the worker never re-picked
	// it — every edit / unfurl re-crawl / tail-sweep re-touch silently
	// dropped a captioned image from retrieval.
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	base := Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:2.2",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "here is a screenshot",
	}
	wantOccurredAt := base.OccurredAt.Add(2 * time.Hour)
	first, err := sink.Upsert(ctx, base)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the caption worker: attach an image_caption chunk to
	// this event, exactly as commitCaption does out-of-band.
	var captionChunkID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
		VALUES ($1, 'image_caption', 0, '[Image]\nA terminal screenshot.', now())
		RETURNING id
	`, first.EventID).Scan(&captionChunkID); err != nil {
		t.Fatalf("seed image_caption chunk: %v", err)
	}

	// Now edit the body — this is the UPDATE path that re-chunks.
	edit := base
	edit.Body = "here is a screenshot (edited to add detail)"
	edit.OccurredAt = wantOccurredAt
	second, err := sink.Upsert(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	if !second.WasUpdate {
		t.Fatal("edit should WasUpdate=true")
	}

	// The image_caption chunk must survive the re-chunk, and its
	// denormalized recency must follow the mutable parent event.
	var (
		captionSurvives   bool
		captionOccurredAt time.Time
	)
	if err := pool.QueryRow(ctx,
		`SELECT true, occurred_at FROM event_chunks WHERE id = $1 AND kind = 'image_caption'`,
		captionChunkID,
	).Scan(&captionSurvives, &captionOccurredAt); err != nil {
		t.Fatal(err)
	}
	if !captionSurvives {
		t.Fatal("image_caption chunk was deleted by body re-chunk (regression: blanket delete)")
	}
	if !captionOccurredAt.Equal(wantOccurredAt) {
		t.Errorf("preserved image_caption occurred_at = %v, want parent recency %v", captionOccurredAt, wantOccurredAt)
	}

	// The body (kind='text') chunks must reflect the new content.
	var bodyChunk string
	if err := pool.QueryRow(ctx,
		`SELECT content FROM event_chunks WHERE event_id = $1 AND kind = 'text' ORDER BY chunk_idx LIMIT 1`,
		first.EventID,
	).Scan(&bodyChunk); err != nil {
		t.Fatal(err)
	}
	if bodyChunk != edit.Body {
		t.Errorf("body chunk not regenerated: got %q, want %q", bodyChunk, edit.Body)
	}
}

func TestUpsertForceUpdateInvalidatesImageCaptionEmbedding(t *testing.T) {
	// Companion to the edit-preservation test: on a ForceUpdate
	// (embedder-model-refresh reembed), the preserved image_caption
	// chunk must survive AND have its embedded_at nulled so the
	// kind-agnostic embedder re-claims and re-embeds it with the new
	// model. Otherwise a model bump leaves caption vectors on the stale
	// model — a silently mixed-model corpus.
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	base := Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C1:3.3",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "screenshot thread",
	}
	first, err := sink.Upsert(ctx, base)
	if err != nil {
		t.Fatal(err)
	}

	// Caption worker attaches an ALREADY-EMBEDDED image_caption chunk.
	var captionChunkID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, embedded_at, embedder_id, occurred_at, queued_at)
		VALUES ($1, 'image_caption', 0, '[Image]\nA screenshot.', now(), 'self:test-embed:v1', now(),
		        now() - interval '3 days')
		RETURNING id
	`, first.EventID).Scan(&captionChunkID); err != nil {
		t.Fatalf("seed embedded image_caption chunk: %v", err)
	}

	// ForceUpdate with identical body — the reembed trigger.
	force := base
	force.ForceUpdate = true
	second, err := sink.Upsert(ctx, force)
	if err != nil {
		t.Fatal(err)
	}
	if !second.WasUpdate {
		t.Fatal("ForceUpdate should WasUpdate=true even on identical body")
	}

	// Caption chunk survives, and its embedded_at is now NULL so the
	// embedder will re-claim it.
	var (
		exists     bool
		embeddedAt *time.Time
	)
	if err := pool.QueryRow(ctx,
		`SELECT true, embedded_at FROM event_chunks WHERE id = $1 AND kind = 'image_caption'`,
		captionChunkID,
	).Scan(&exists, &embeddedAt); err != nil {
		t.Fatalf("caption chunk should still exist: %v", err)
	}
	if embeddedAt != nil {
		t.Errorf("ForceUpdate should NULL image_caption embedded_at (got %v) so the embedder re-embeds it", embeddedAt)
	}
	// It waits from now, not from when the caption was first written.
	var queuedNow bool
	if err := pool.QueryRow(ctx,
		`SELECT queued_at > now() - interval '1 minute' FROM event_chunks WHERE id = $1`,
		captionChunkID).Scan(&queuedNow); err != nil {
		t.Fatal(err)
	}
	if !queuedNow {
		t.Error("a chunk put back to be embedded again should be queued as of now")
	}
}

func TestUpsertRecencyMetadataPreservesEmbeddings(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		delta time.Duration
	}{
		{name: "latest_activity", delta: 2 * time.Hour},
		{name: "historical_timestamp_correction", delta: -2 * time.Hour},
		{name: "revision_only"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			pool := setupPool(t)
			sink := NewDBSink(pool, prometheus.NewRegistry())
			ctx := context.Background()
			originalTime := time.Unix(1700000000, 0).UTC()
			embeddedAt := originalTime.Add(time.Hour)
			base := Event{Scope: ScopeInternal, Source: "agent_session", ExternalID: "stable-range",
				OccurredAt: originalTime, Body: "display summary", EmbeddingText: "unchanged embedding text",
				ContextPrefix: "stable context", Raw: json.RawMessage(`{"revision":"v1"}`)}
			first, err := sink.Upsert(ctx, base)
			if err != nil {
				t.Fatal(err)
			}
			var originalID int64
			if err := pool.QueryRow(ctx, `UPDATE event_chunks SET embedded_at=$2,embedder_id='existing-model' WHERE event_id=$1 RETURNING id`, first.EventID, embeddedAt).Scan(&originalID); err != nil {
				t.Fatal(err)
			}
			updated := base
			updated.OccurredAt = originalTime.Add(scenario.delta)
			updated.Raw = json.RawMessage(`{"revision":"v2"}`)
			second, err := sink.Upsert(ctx, updated)
			if err != nil {
				t.Fatal(err)
			}
			if !second.WasUpdate || second.NoChange || second.Chunks != 0 || second.EventID != first.EventID {
				t.Errorf("metadata update=%+v, want same event and no inserted chunks", second)
			}
			var currentID int64
			var eventTime, chunkTime time.Time
			var embedded *time.Time
			var model, revision, prefix string
			var pending bool
			if err := pool.QueryRow(ctx, `SELECT c.id,e.occurred_at,c.occurred_at,c.embedded_at,coalesce(c.embedder_id,''),e.raw->>'revision',c.context_prefix,c.scope_sync_needed
				FROM events e JOIN event_chunks c ON c.event_id=e.id WHERE e.id=$1`, first.EventID).Scan(&currentID, &eventTime, &chunkTime, &embedded, &model, &revision, &prefix, &pending); err != nil {
				t.Fatal(err)
			}
			if currentID != originalID || embedded == nil || !embedded.Equal(embeddedAt) || model != "existing-model" || prefix != base.ContextPrefix {
				t.Errorf("metadata update lost existing embedding: id %d -> %d, embedded=%v model=%q prefix=%q", originalID, currentID, embedded, model, prefix)
			}
			if !eventTime.Equal(updated.OccurredAt) || !chunkTime.Equal(updated.OccurredAt) || revision != "v2" {
				t.Errorf("metadata not updated: event/chunk time=%s/%s revision=%q", eventTime, chunkTime, revision)
			}
			if pending != (scenario.delta != 0) {
				t.Errorf("payload sync pending=%v, want %v", pending, scenario.delta != 0)
			}
			var deleted int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM qdrant_delete_queue WHERE point_id=$1`, originalID).Scan(&deleted); err != nil {
				t.Fatal(err)
			}
			if deleted != 0 {
				t.Errorf("metadata-only update queued old vector for deletion")
			}
		})
	}
}

func TestUpsertOccurredAtUpdates(t *testing.T) {
	// Re-emit with a later occurred_at + identical body must be
	// treated as an UPDATE: the events row's occurred_at column
	// moves forward. This is what session ingest relies on for its
	// `:session` row's "last activity" semantics — the same row
	// gets re-pushed every worker tick with an updated timestamp.
	// Without occurred_at in the dirty-check + SET list, this would
	// be a no-op and `list --session-roots ORDER BY occurred_at
	// DESC` would order by birth-time instead of activity-time.
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	t0 := time.Unix(1700000000, 0).UTC()
	base := Event{
		Scope:      ScopePublic,
		Source:     "agent_session",
		ExternalID: "abc:session",
		OccurredAt: t0,
		Actor:      "alice@example.com",
		Body:       "session-level summary",
	}
	first, err := sink.Upsert(ctx, base)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Re-emit with same body but later occurred_at.
	t1 := t0.Add(2 * time.Hour)
	bumped := base
	bumped.OccurredAt = t1
	second, err := sink.Upsert(ctx, bumped)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if !second.WasUpdate {
		t.Fatalf("expected WasUpdate=true on occurred_at bump; got %+v", second)
	}
	if second.NoChange {
		t.Fatalf("expected NoChange=false on occurred_at bump; got %+v", second)
	}
	if second.EventID != first.EventID {
		t.Errorf("EventID drifted: %d -> %d", first.EventID, second.EventID)
	}

	// occurred_at on the row must reflect the new value.
	var got time.Time
	if err := pool.QueryRow(ctx,
		`SELECT occurred_at FROM events WHERE id = $1`, first.EventID,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(t1) {
		t.Errorf("occurred_at = %v, want %v", got, t1)
	}

	// Re-emitting with the SAME occurred_at should NoChange (no-op
	// short-circuit still works when nothing actually changed).
	third, err := sink.Upsert(ctx, bumped)
	if err != nil {
		t.Fatalf("third upsert: %v", err)
	}
	if !third.NoChange {
		t.Errorf("identical re-emit should NoChange=true; got %+v", third)
	}
}

func TestUpsertThenUndeleteRevives(t *testing.T) {
	// Design invariant: ON CONFLICT DO UPDATE sets deleted=false. A
	// Slack edit on a message we previously deleted effectively
	// un-tombstones. Verifies the UPDATE clause includes that.
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())

	ctx := context.Background()
	if _, err := sink.Upsert(ctx, Event{
		Scope:  ScopePublic,
		Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0), Body: "first",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := sink.Delete(ctx, "slack", "C1:1.1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	revive, err := sink.Upsert(ctx, Event{
		Scope:  ScopePublic,
		Source: "slack", ExternalID: "C1:1.1",
		OccurredAt: time.Unix(1700000000, 0), Body: "revived",
	})
	if err != nil {
		t.Fatalf("revive upsert: %v", err)
	}
	if !revive.WasUpdate {
		t.Fatal("revive should be WasUpdate=true (same row re-used)")
	}
	var (
		deleted bool
		body    string
	)
	err = pool.QueryRow(ctx,
		`SELECT deleted, body FROM events WHERE id = $1`,
		revive.EventID,
	).Scan(&deleted, &body)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if deleted {
		t.Error("revived event should have deleted=false")
	}
	if body != "revived" {
		t.Errorf("revived body mismatch: %q", body)
	}
}

func TestStoredBody(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	// A notion event: title + rendered page markdown in body.
	notionRes, err := sink.Upsert(ctx, Event{
		Scope:      ScopeInternal,
		Source:     "notion",
		ExternalID: "page-uuid-1",
		OccurredAt: time.Unix(1700000000, 0),
		Actor:      "alex@example.com",
		Title:      "Cluster runbook",
		Body:       "## Step 1\nDrain the node.\n## Step 2\nCordon it.",
		URL:        "https://www.notion.so/page-uuid-1",
	})
	if err != nil {
		t.Fatalf("notion Upsert: %v", err)
	}

	t.Run("returns source, title, and body", func(t *testing.T) {
		stored, err := sink.StoredBody(ctx, notionRes.EventID)
		if err != nil {
			t.Fatalf("StoredBody: %v", err)
		}
		if stored.Source != "notion" || stored.Title != "Cluster runbook" {
			t.Errorf("stored = %+v, want the notion page and its title", stored)
		}
		if !strings.Contains(stored.Body, "Drain the node") {
			t.Errorf("body = %q, want it to contain the page markdown", stored.Body)
		}
	})

	t.Run("missing event returns ErrEventNotFound", func(t *testing.T) {
		_, err := sink.StoredBody(ctx, 999999)
		if !errors.Is(err, ErrEventNotFound) {
			t.Errorf("err = %v, want ErrEventNotFound", err)
		}
	})

	t.Run("empty title returns empty string, not error", func(t *testing.T) {
		// Notion title is nullable; a titleless page must still expand.
		res, err := sink.Upsert(ctx, Event{
			Scope:      ScopeInternal,
			Source:     "notion",
			ExternalID: "page-uuid-2",
			OccurredAt: time.Unix(1700000002, 0),
			Body:       "Database: Feedback · Pain: Medium",
		})
		if err != nil {
			t.Fatalf("titleless notion Upsert: %v", err)
		}
		stored, err := sink.StoredBody(ctx, res.EventID)
		if err != nil {
			t.Fatalf("StoredBody: %v", err)
		}
		if stored.Title != "" {
			t.Errorf("title = %q, want empty", stored.Title)
		}
		if !strings.Contains(stored.Body, "Feedback") {
			t.Errorf("body = %q, want the property preface", stored.Body)
		}
	})
}

// TestExternalIDsListsASourcesLiveEvents: a source's events that are not
// deleted, and no other source's.
func TestExternalIDsListsASourcesLiveEvents(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := t.Context()
	for _, e := range []Event{
		{Source: "notes", ExternalID: "a.md"},
		{Source: "notes", ExternalID: "dir/b.md"},
		{Source: "notes", ExternalID: "gone.md"},
		{Source: "other", ExternalID: "c.md"},
	} {
		e.Scope, e.OccurredAt, e.Body = ScopeInternal, time.Unix(1700000000, 0), "text"
		if _, err := sink.Upsert(ctx, e); err != nil {
			t.Fatalf("Upsert %s: %v", e.ExternalID, err)
		}
	}
	if _, err := sink.Delete(ctx, "notes", "gone.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ids, err := sink.ExternalIDs(ctx, "notes")
	if err != nil {
		t.Fatalf("ExternalIDs: %v", err)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"a.md", "dir/b.md"}) {
		t.Errorf("ids = %v, want the live notes events", ids)
	}
	ids, err = sink.ExternalIDs(ctx, "missing")
	if err != nil || len(ids) != 0 {
		t.Errorf("ids, err = %v, %v, want none", ids, err)
	}
}

// TestIngestLagObservedOnInsertOnly verifies that
// diderot_events_ingest_lag_seconds is observed only on genuine
// INSERTs, not on updates or no-ops. This guards the branch logic
// added in UpsertWithSideWrite — the metric is load-bearing for
// latency analysis and must not be polluted by re-ingest noise.
func TestIngestLagObservedOnInsertOnly(t *testing.T) {
	pool := setupPool(t)
	reg := prometheus.NewRegistry()
	sink := NewDBSink(pool, reg)
	ctx := context.Background()

	baseEvent := Event{
		Scope:      ScopePublic,
		Source:     "slack",
		ExternalID: "C99:lag-test",
		OccurredAt: time.Unix(1700000000, 0),
		Body:       "original body",
	}

	lagMetricName := "diderot_events_ingest_lag_seconds"

	// Step 1: fresh INSERT — one observation must land.
	_, err := sink.Upsert(ctx, baseEvent)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if n := testutil.CollectAndCount(sink.metrics.ingestLag, lagMetricName); n == 0 {
		t.Fatal("expected ingest_lag observation on INSERT; got none")
	}

	// Step 2: no-change re-ingest — observation count must not increase.
	beforeCount := testutil.CollectAndCount(sink.metrics.ingestLag, lagMetricName)
	noChange, err := sink.Upsert(ctx, baseEvent)
	if err != nil {
		t.Fatalf("no-change upsert: %v", err)
	}
	if !noChange.NoChange {
		t.Skip("expected NoChange on re-ingest of identical event; precondition failed")
	}
	if n := testutil.CollectAndCount(sink.metrics.ingestLag, lagMetricName); n != beforeCount {
		t.Errorf("ingest_lag observed on NoChange: count changed %d → %d", beforeCount, n)
	}

	// Step 3: UPDATE (different body) — observation count must still not
	// increase (ingested_at is immutable after the initial INSERT).
	beforeCount = testutil.CollectAndCount(sink.metrics.ingestLag, lagMetricName)
	edit := baseEvent
	edit.Body = "edited body with new content"
	upd, err := sink.Upsert(ctx, edit)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !upd.WasUpdate {
		t.Skip("expected WasUpdate on body change; precondition failed")
	}
	if n := testutil.CollectAndCount(sink.metrics.ingestLag, lagMetricName); n != beforeCount {
		t.Errorf("ingest_lag observed on UPDATE: count changed %d → %d", beforeCount, n)
	}
}

func TestAgentSessionRange_CanonicalRecordRangeExpansion(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	// A native session-ingest event carries the canonical identity in
	// raw (owner/client/revision) alongside the session_id/msg range.
	nativeRes, err := sink.Upsert(ctx, Event{
		Scope:       ScopeInternal,
		Source:      "agent_session",
		ExternalID:  "a@x/codex/ses_1:session",
		OccurredAt:  time.Unix(1700000000, 0),
		Actor:       "a@x",
		Title:       "session ses_1",
		Body:        "summary",
		SessionID:   "ses_1",
		RecordStart: 0,
		RecordEnd:   40,
		Raw:         json.RawMessage(`{"owner":"a@x","client":"codex","revision":"sha256:abc","rewrite_epoch":2}`),
	})
	if err != nil {
		t.Fatalf("native Upsert: %v", err)
	}

	r, err := sink.AgentSessionRange(ctx, nativeRes.EventID)
	if err != nil {
		t.Fatalf("AgentSessionRange: %v", err)
	}
	// The canonical native record-range expansion must resolve the FULL
	// identity + revision + half-open bounds: owner, client, session_id,
	// revision, start, end.
	if r.Owner != "a@x" || r.Client != "codex" || r.SessionID != "ses_1" {
		t.Errorf("identity = (%q,%q,%q), want (a@x,codex,ses_1)",
			r.Owner, r.Client, r.SessionID)
	}
	if r.Revision != "sha256:abc" {
		t.Errorf("revision = %q, want sha256:abc", r.Revision)
	}
	if r.Start != 0 || r.End != 40 {
		t.Errorf("range = [%d,%d), want [0,40)", r.Start, r.End)
	}
}

func TestAgentSessionRange_LegacyEventKeepsLegacyRange(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	// A legacy agent_session event (no canonical identity in raw) —
	// the existing expand path must keep resolving session + range and
	// leave identity/revision empty so expand falls back to the legacy
	// transcript-range endpoint.
	legacyRes, err := sink.Upsert(ctx, Event{
		Scope:       ScopePublic,
		Source:      "agent_session",
		ExternalID:  "legacy:1",
		OccurredAt:  time.Unix(1700000000, 0),
		Actor:       "a@x",
		Title:       "legacy session",
		Body:        "legacy body",
		SessionID:   "legacy-ses",
		RecordStart: 2,
		RecordEnd:   9,
		Raw:         json.RawMessage(`{"unrelated":true}`),
	})
	if err != nil {
		t.Fatalf("legacy Upsert: %v", err)
	}

	r, err := sink.AgentSessionRange(ctx, legacyRes.EventID)
	if err != nil {
		t.Fatalf("AgentSessionRange: %v", err)
	}
	if r.SessionID != "legacy-ses" || r.Start != 2 || r.End != 9 {
		t.Errorf("legacy range = (%q,[%d,%d)), want (legacy-ses,[2,9))",
			r.SessionID, r.Start, r.End)
	}
	if r.Owner != "" || r.Client != "" || r.Revision != "" {
		t.Errorf("legacy event should carry no canonical identity, got %+v", r)
	}
}

func TestDeletePattern_TombstonesPrefixInOneTx(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	upsert := func(ext, raw string) {
		t.Helper()
		if _, err := sink.Upsert(ctx, Event{
			Scope:      ScopeInternal,
			Source:     "agent_session",
			ExternalID: ext,
			OccurredAt: time.Unix(1700000000, 0),
			Body:       "body " + ext,
			Raw:        json.RawMessage(raw),
		}); err != nil {
			t.Fatal(err)
		}
	}
	upsert("a@x/codex/ses_1:records:0:1", `{"event_kind":"native_session_chunk"}`)
	upsert("a@x/codex/ses_1:records:1:2", `{"event_kind":"native_session_chunk"}`)
	upsert("a@x/codex/ses_1:records:2:3", `{"event_kind":"native_session_chunk"}`)
	upsert("a@x/codex/ses_1:session", `{}`)                      // final event: untouched
	upsert("a@x/codex/ses_other:records:0:1", `{}`)              // sibling prefix: untouched
	upsert("a@x/codex/ses_1:records_legacy:0:1", `{"legacy":1}`) // legacy id lookalike: untouched

	res, err := sink.DeletePattern(ctx, "agent_session", "a@x/codex/ses_1:records:", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Existed || res.Events != 3 || res.Chunks != 3 {
		t.Fatalf("DeletePattern result = %+v, want 3 events tombstoned", res)
	}
	assertDeleted := func(ext string, want bool) {
		t.Helper()
		var deleted bool
		var body, title *string
		if err := pool.QueryRow(ctx, `SELECT deleted, body, title FROM events WHERE external_id=$1`, ext).
			Scan(&deleted, &body, &title); err != nil {
			t.Fatal(err)
		}
		if deleted != want {
			t.Fatalf("%s deleted = %v, want %v", ext, deleted, want)
		}
		if want && (*body != "" || title != nil) {
			t.Fatalf("%s tombstone left body=%q title=%v", ext, *body, title)
		}
		if want {
			var chunks int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE event_id = (SELECT id FROM events WHERE external_id=$1)`, ext).Scan(&chunks); err != nil {
				t.Fatal(err)
			}
			if chunks != 0 {
				t.Fatalf("%s tombstone left %d chunk rows", ext, chunks)
			}
		}
	}
	assertDeleted("a@x/codex/ses_1:records:0:1", true)
	assertDeleted("a@x/codex/ses_1:records:1:2", true)
	assertDeleted("a@x/codex/ses_1:records:2:3", true)
	assertDeleted("a@x/codex/ses_1:session", false)
	assertDeleted("a@x/codex/ses_other:records:0:1", false)
	assertDeleted("a@x/codex/ses_1:records_legacy:0:1", false)

	// Idempotent second run: nothing left to tombstone.
	res2, err := sink.DeletePattern(ctx, "agent_session", "a@x/codex/ses_1:records:", "")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Existed {
		t.Fatalf("second DeletePattern should be a no-op, got %+v", res2)
	}
}

func TestDeletePattern_EscapesLikeMetaCharacters(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	// Session ids may contain LIKE metacharacters; the prefix must be
	// matched literally, never as a wildcard.
	ids := []string{
		"a@x/codex/s_1:records:0:1",
		"a@x/codex/sX1:records:0:1", // '_' as wildcard would alias s_1
		"a@x/codex/100%:records:0:1",
	}
	for _, ext := range ids {
		if _, err := sink.Upsert(ctx, Event{
			Scope:      ScopeInternal,
			Source:     "agent_session",
			ExternalID: ext,
			OccurredAt: time.Unix(1700000000, 0),
			Body:       "body",
			Raw:        json.RawMessage(`{"event_kind":"native_session_chunk"}`),
		}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := sink.DeletePattern(ctx, "agent_session", "a@x/codex/s_1:records:", "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Existed || res.Events != 1 {
		t.Fatalf("DeletePattern escaped result = %+v, want exactly the s_1 row", res)
	}
	var victimDeleted bool
	if err := pool.QueryRow(ctx, `SELECT deleted FROM events WHERE external_id=$1`, "a@x/codex/s_1:records:0:1").Scan(&victimDeleted); err != nil {
		t.Fatal(err)
	}
	if !victimDeleted {
		t.Fatal("s_1 row should have been tombstoned by the exact literal prefix")
	}
	query := `SELECT deleted FROM events WHERE external_id=$1`
	var untouched bool
	if err := pool.QueryRow(ctx, query, "a@x/codex/sX1:records:0:1").Scan(&untouched); err != nil {
		t.Fatal(err)
	}
	if untouched {
		t.Fatal("sX1 row was tombstoned by an unescaped _ prefix")
	}
	if err := pool.QueryRow(ctx, query, "a@x/codex/100%:records:0:1").Scan(&untouched); err != nil {
		t.Fatal(err)
	}
	if untouched {
		t.Fatal("100% row was tombstoned by an unescaped % prefix")
	}
}

// TestUpsertConcurrentFirstInsertBothSucceed covers the documented
// contract (package comment: "Upsert is the only write entry point.
// Conflict on UNIQUE(source, external_id) means 'this is an edit'";
// AGENTS.md: "The sink is idempotent on (source, external_id)").
//
// Trigger: two writers race to be the FIRST to insert a row for a
// brand-new (source, external_id) — e.g. a Slack message delivered as
// both a `message` and an `app_mention` envelope, each ingested by its
// own goroutine. Writer A's INSERT of that key is open but not yet
// committed when writer B's Upsert begins; B's own INSERT blocks on the
// unique index until A commits.
//
// Before the fix: B's single-statement CTE took its snapshot before A
// committed. Once A committed and B unblocked, B's `ins` CTE correctly
// saw the conflict and did nothing, but its `existing` fallback SELECT
// was bound to that same pre-commit snapshot and also saw nothing, so
// upsertTx returned "events: upsert produced neither inserted nor
// existing id" instead of treating A's row as the existing one.
//
// We don't use sink.Upsert for writer A because a plain conflicting
// INSERT held open in a manually-controlled transaction is sufficient to
// create the race, and it keeps the forced interleaving (wait for B to
// actually block, via pg_locks, before committing A) simple and
// deterministic rather than timing-based.
func TestUpsertConcurrentFirstInsertBothSucceed(t *testing.T) {
	pool := setupPool(t)
	ctx := context.Background()
	sink := NewDBSink(pool, prometheus.NewRegistry())
	const source, extID = "slack", "C1:race-first-insert"

	connA, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn A: %v", err)
	}
	defer connA.Release()
	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()

	if _, err := txA.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw, scope, deleted)
		VALUES ($1, $2, now(), 'writer A body', '{}', 'public', false)
	`, source, extID); err != nil {
		t.Fatalf("writer A insert: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := sink.Upsert(ctx, Event{
			Source:     source,
			ExternalID: extID,
			OccurredAt: time.Unix(1700000000, 0),
			Body:       "writer B body",
			Scope:      ScopePublic,
		})
		done <- err
	}()

	// Wait until writer B's statement is actually blocked on A's
	// uncommitted row before releasing A, so the interleaving is forced
	// rather than hoped for.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `
			SELECT count(*)
			  FROM pg_locks l
			  JOIN pg_stat_activity a ON a.pid = l.pid
			 WHERE NOT l.granted
			   AND a.datname = current_database()
		`).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer B never reached the lock-wait state; test setup assumption broken")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writer B's Upsert, racing writer A's concurrent first insert of the same new "+
				"key, returned an error instead of resolving to an update (the sink's documented "+
				"idempotent-upsert contract on (source, external_id)): %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("writer B's Upsert hung after writer A committed")
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source=$1 AND external_id=$2`,
		source, extID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 events row for %s/%s, got %d", source, extID, n)
	}
}

// TestUpsertConcurrentFirstInsertSessionSource is the same race as
// TestUpsertConcurrentFirstInsertBothSucceed, on an agent_session row
// (the shape internal/sessioningest's PublishSession depends on: "two
// workers publish once" assumes a concurrent first-ever Upsert of the
// same (source, external_id) never errors). It exists alongside that
// test because it was reported and reproduced independently against the
// session-ingest call path, and it pins down the row-count-stays-one
// outcome explicitly rather than only "no error".
func TestUpsertConcurrentFirstInsertSessionSource(t *testing.T) {
	pool := setupPool(t)
	sink := NewDBSink(pool, prometheus.NewRegistry())
	ctx := context.Background()

	const source, externalID = "agent_session", "race:session-first-insert"

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()

	// Transaction 1 reserves the row's unique key but does not commit yet.
	if _, err := tx1.Exec(ctx, `
		INSERT INTO events
		    (source, external_id, occurred_at, actor, actor_id, thread_id,
		     title, body, url, raw, embedding_text, session_id,
		     record_start, record_end, actor_canonical, scope, deleted)
		VALUES ($1, $2, now(), NULL, NULL, NULL, '', 'seed', NULL, '{}', NULL, NULL,
		        NULL, NULL, '', 'internal', false)
	`, source, externalID); err != nil {
		t.Fatalf("seed insert (tx1): %v", err)
	}

	// A second, independent caller concurrently upserts the identical
	// (source, external_id) for the first time, e.g. two racing
	// session-ingest workers both publishing a brand-new session.
	ev := Event{
		Source: source, ExternalID: externalID, OccurredAt: time.Now(),
		Body: "worker B", ActorCanonical: "", Scope: ScopeInternal,
	}
	done := make(chan error, 1)
	go func() {
		_, upsertErr := sink.Upsert(ctx, ev)
		done <- upsertErr
	}()

	// Wait until worker B's backend is actually blocked on tx1's lock
	// (polling pg_locks — no sleep-and-hope), then let tx1 through.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `
			SELECT count(*)
			  FROM pg_locks l
			  JOIN pg_stat_activity a ON a.pid = l.pid
			 WHERE NOT l.granted
			   AND a.datname = current_database()
			   AND a.pid <> pg_backend_pid()
		`).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker B's upsert never blocked on tx1's lock; harness assumption broke")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit tx1: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("worker B's Upsert of the same (source, external_id) worker A just committed "+
			"should succeed (it must either have inserted, or now see worker A's committed row "+
			"as \"existing\"); got error instead: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE source=$1 AND external_id=$2`,
		source, externalID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 events row for %s/%s, got %d", source, externalID, n)
	}
}
