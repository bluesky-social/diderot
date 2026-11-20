package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestCommentOverflowFetchFailureDoesNotStoreTruncatedIssue is the
// deterministic regression test for: when an issue has more than 50
// comments (the inline GraphQL page size) and the follow-up fetch for the
// rest fails, the issue must not be written with only its first 50
// comments as if that were the complete, current body.
//
// Trigger: one incremental sync page returns a single issue ("overflow-1")
// with 50 inline comments and hasNextPage=true. The follow-up
// IssueComments request (fetchRemainingComments) fails with a non-retried
// HTTP 400 — a deterministic stand-in for any failure of that follow-up
// call (a transient network error, a non-2xx status).
//
// Contract: a failed comment-overflow fetch must not store a truncated
// issue as if it were complete. The next sync must retry the whole
// fetch, so the issue is not written at all this tick.
//
// Before the fix: runIssuePage logs the fetchRemainingComments error,
// counts it in batch.TotalErrors, and still calls upsertIssue with only
// the first 50 comments — the issue is stored, permanently missing
// everything past comment #50, because the sink's NoChange short-circuit
// makes a later re-ingest of the same (still-incomplete) content a no-op.
func TestCommentOverflowFetchFailureDoesNotStoreTruncatedIssue(t *testing.T) {
	databaseURL := linearTestDBURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE linear_sync SET last_sync_at = NULL WHERE id = 1`); err != nil {
		t.Fatalf("reset watermark: %v", err)
	}

	const issueID = "overflow-1"
	issue := buildTestIssue(issueID, "ENG-500", "Issue with many comments", false, "ENG")
	issue["comments"] = map[string]any{
		"nodes":    makeTestComments(1, 50),
		"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "c50"},
	}

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// The incremental sync page: one issue, 50 inline comments,
			// more available.
			_, _ = w.Write(mustMarshal(map[string]any{
				"data": map[string]any{
					"issues": map[string]any{
						"nodes":    []any{issue},
						"pageInfo": map[string]any{"hasNextPage": false},
					},
				},
			}))
			return
		}
		// The overflow comments fetch: a non-retried failure.
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	w := buildTestSyncWorker(t, srv, pool)
	// The tick's error is not asserted here; this test is about what
	// gets stored, not about whether the tick reports itself as failed.
	_ = w.runSync(ctx)

	var count int
	var body string
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(MAX(body), '') FROM events WHERE source = 'linear' AND external_id = $1 AND NOT deleted`,
		issueID).Scan(&count, &body); err != nil {
		t.Fatalf("query: %v", err)
	}

	// Expected: the issue is not written at all this tick (count=0), so
	// the next tick retries the full comment fetch instead of leaving a
	// truncated body in place.
	if count != 0 {
		t.Fatalf("issue %q stored with count=%d, want 0 (comment-overflow fetch failed; the issue must be skipped, not written truncated); body=%q",
			issueID, count, body)
	}
}

// makeTestComments builds n GraphQL comment nodes numbered start..start+n-1.
func makeTestComments(start, n int) []any {
	out := make([]any, n)
	for i := 0; i < n; i++ {
		id := start + i
		out[i] = map[string]any{
			"id":        fmt.Sprintf("c%d", id),
			"body":      fmt.Sprintf("Comment body %d", id),
			"createdAt": "2026-01-01T00:00:00Z",
			"updatedAt": "2026-01-01T00:00:00Z",
			"user":      map[string]any{"id": "u1", "name": "Commenter", "email": "c@example.com"},
		}
	}
	return out
}

func TestDryRunNoCounts(t *testing.T) {
	url := testdb.URL(t, "linear_backfill")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Reset events.
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Two issues in different teams.
	issue1 := buildTestIssue("dry-001", "ENG-1", "Task one", false, "ENG")
	issue2 := buildTestIssue("dry-002", "APP-1", "Task two", false, "APP")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{issue1, issue2},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	provider := NewStaticTokenProvider(syntheticToken)
	client := NewClient(provider)
	client.httpClient = srv.Client()
	client.httpClient.Transport = &rewriteTransport{
		base:   srv.Client().Transport,
		target: srv.URL,
	}
	sink := events.NewDBSink(pool, reg)
	worker := NewSyncWorker(pool, client, sink, nil, reg)

	runner := NewBackfillRunner(client, worker)
	result, err := runner.Run(ctx, BackfillOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Run dry-run: %v", err)
	}

	if result.TotalIssues != 2 {
		t.Errorf("TotalIssues=%d, want 2", result.TotalIssues)
	}

	// In dry-run mode, nothing should be written to the DB.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("dry-run wrote %d events to DB, want 0", count)
	}

	// Team volumes should be populated.
	if len(result.TeamVolumes) != 2 {
		t.Errorf("TeamVolumes=%d, want 2", len(result.TeamVolumes))
	}
	teamKeys := make(map[string]bool)
	for _, tv := range result.TeamVolumes {
		teamKeys[tv.TeamKey] = true
	}
	if !teamKeys["ENG"] {
		t.Errorf("ENG team not in dry-run report; got %v", result.TeamVolumes)
	}
	if !teamKeys["APP"] {
		t.Errorf("APP team not in dry-run report; got %v", result.TeamVolumes)
	}
}

func TestWriteBackfillUpserts(t *testing.T) {
	url := testdb.URL(t, "linear_backfill_write")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Reset.
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE linear_sync SET backfill_cursor = NULL WHERE id = 1`); err != nil {
		t.Fatalf("reset cursor: %v", err)
	}

	issue1 := buildTestIssue("bf-001", "ENG-10", "Backfill issue", false, "ENG")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{issue1},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	provider := NewStaticTokenProvider(syntheticToken)
	client := NewClient(provider)
	client.httpClient = srv.Client()
	client.httpClient.Transport = &rewriteTransport{
		base:   srv.Client().Transport,
		target: srv.URL,
	}
	sink := events.NewDBSink(pool, reg)
	worker := NewSyncWorker(pool, client, sink, nil, reg)
	runner := NewBackfillRunner(client, worker)

	result, err := runner.Run(ctx, BackfillOptions{DryRun: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if result.TotalIssues != 1 {
		t.Errorf("TotalIssues=%d, want 1", result.TotalIssues)
	}
	if result.TotalErrors != 0 {
		t.Errorf("TotalErrors=%d, want 0", result.TotalErrors)
	}

	// Issue should be stored.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'bf-001'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("event count=%d, want 1", count)
	}

	// Cursor should be cleared after successful run.
	var cursor *string
	if err := pool.QueryRow(ctx, `SELECT backfill_cursor FROM linear_sync WHERE id = 1`).Scan(&cursor); err != nil {
		t.Fatalf("cursor check: %v", err)
	}
	if cursor != nil {
		t.Errorf("cursor should be NULL after successful backfill, got %q", *cursor)
	}
}

// TestProjectScopedBackfill verifies that a project-scoped backfill:
//   - passes the projectID variable in the GraphQL request
//   - only upserts issues returned by the (mocked) project-filtered API
//   - does NOT touch the cursor in linear_sync (skips cursor persistence)
func TestProjectScopedBackfill(t *testing.T) {
	url := testdb.URL(t, "linear_project_backfill")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Reset state.
	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset events: %v", err)
	}
	// Set a known cursor that must NOT be touched by a project-scoped run.
	if _, err := pool.Exec(ctx, `UPDATE linear_sync SET backfill_cursor = 'sentinel-cursor' WHERE id = 1`); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	t.Cleanup(func() {
		// Clean up sentinel so other tests start clean. Use a fresh context
		// since the test context is already cancelled at cleanup time.
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		if _, err := pool.Exec(cleanCtx, `UPDATE linear_sync SET backfill_cursor = NULL WHERE id = 1`); err != nil {
			t.Logf("cleanup cursor: %v", err)
		}
	})

	const targetProjectID = "proj-0000-0000-0000-000000000001"

	// The mock server returns one issue matching the project and one that does
	// not. In a real run the API would only return the filtered one; the mock
	// returns both to prove diderot stores whatever the API hands back (filter
	// enforcement is on Linear's side, not duplicated client-side).
	// For this test we just return the project-matched issue.
	projectIssue := buildTestIssueWithProject("proj-issue-001", "ENG-20", "Project issue", false, "ENG", targetProjectID)

	var gotProjectID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Capture the variables sent by the client.
		var body struct {
			Variables map[string]any `json:"variables"`
		}
		if err := decodeBody(r, &body); err == nil {
			if v, ok := body.Variables["projectID"]; ok {
				gotProjectID, _ = v.(string)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{projectIssue},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	provider := NewStaticTokenProvider(syntheticToken)
	client := NewClient(provider)
	client.httpClient = srv.Client()
	client.httpClient.Transport = &rewriteTransport{
		base:   srv.Client().Transport,
		target: srv.URL,
	}
	sink := events.NewDBSink(pool, reg)
	worker := NewSyncWorker(pool, client, sink, nil, reg)
	runner := NewBackfillRunner(client, worker)

	result, err := runner.Run(ctx, BackfillOptions{
		DryRun:    false,
		ProjectID: targetProjectID,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The projectID must have been sent in the request variables.
	if gotProjectID != targetProjectID {
		t.Errorf("projectID in request=%q, want %q", gotProjectID, targetProjectID)
	}

	if result.TotalIssues != 1 {
		t.Errorf("TotalIssues=%d, want 1", result.TotalIssues)
	}
	if result.TotalErrors != 0 {
		t.Errorf("TotalErrors=%d, want 0", result.TotalErrors)
	}

	// Issue should be stored.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'proj-issue-001'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("event count=%d, want 1", count)
	}

	// Cursor must be unchanged (still the sentinel value).
	var cursor *string
	if err := pool.QueryRow(ctx, `SELECT backfill_cursor FROM linear_sync WHERE id = 1`).Scan(&cursor); err != nil {
		t.Fatalf("cursor check: %v", err)
	}
	if cursor == nil || *cursor != "sentinel-cursor" {
		t.Errorf("cursor=%v, want 'sentinel-cursor' (project-scoped run must not modify cursor)", cursor)
	}
}

// TestProjectScopedDryRun verifies that --project --dry-run counts without writing.
func TestProjectScopedDryRun(t *testing.T) {
	url := testdb.URL(t, "linear_project_dryrun")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DELETE FROM events WHERE source = 'linear'`); err != nil {
		t.Fatalf("reset: %v", err)
	}

	const targetProjectID = "proj-0000-0000-0000-000000000002"
	projectIssue := buildTestIssueWithProject("proj-dry-001", "ENG-30", "Dry issue", false, "ENG", targetProjectID)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(map[string]any{
			"data": map[string]any{
				"issues": map[string]any{
					"nodes":    []any{projectIssue},
					"pageInfo": map[string]any{"hasNextPage": false},
				},
			},
		}))
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	provider := NewStaticTokenProvider(syntheticToken)
	client := NewClient(provider)
	client.httpClient = srv.Client()
	client.httpClient.Transport = &rewriteTransport{
		base:   srv.Client().Transport,
		target: srv.URL,
	}
	sink := events.NewDBSink(pool, reg)
	worker := NewSyncWorker(pool, client, sink, nil, reg)
	runner := NewBackfillRunner(client, worker)

	result, err := runner.Run(ctx, BackfillOptions{
		DryRun:    true,
		ProjectID: targetProjectID,
	})
	if err != nil {
		t.Fatalf("Run dry-run: %v", err)
	}

	if result.TotalIssues != 1 {
		t.Errorf("TotalIssues=%d, want 1", result.TotalIssues)
	}

	// Nothing written.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM events WHERE source = 'linear' AND external_id = 'proj-dry-001'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("dry-run project backfill wrote %d events, want 0", count)
	}
}

// buildTestIssueWithProject extends buildTestIssue with a project field.
func buildTestIssueWithProject(id, identifier, title string, private bool, teamKey, projectID string) map[string]any {
	issue := buildTestIssue(id, identifier, title, private, teamKey)
	issue["project"] = map[string]any{
		"id":   projectID,
		"name": "Test Project",
	}
	return issue
}

// decodeBody decodes a JSON request body into dst.
func decodeBody(r *http.Request, dst any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}
