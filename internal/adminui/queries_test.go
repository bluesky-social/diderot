package adminui

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/testdb"
)

func TestAskShowsGeneratedTitleNeverTheQuestion(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	server := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sessionID := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO ask_sessions (id, owner_email) VALUES ($1, 'titles@example.com')`, sessionID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO ask_messages (session_id, turn, role, message_schema_version, message)
		VALUES ($1, 0, 'user', 1,
		        '{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"This question must not become a title"}}]}'),
		       ($1, 1, 'assistant', 1,
		        '{"role":"assistant","content_blocks":[{"type":"assistant_gen_text","assistant_gen_text":{"text":"This answer must not become a title"}}]}')`, sessionID)
	require.NoError(t, err)

	checkTitle := func(expected string) {
		t.Helper()
		listing, _, err := server.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions?owner=titles@example.com", nil))
		require.NoError(t, err)
		require.Len(t, listing.Sessions, 1)
		assert.Equal(t, expected, listing.Sessions[0].Title)
		detail, err := server.loadAskSession(ctx, sessionID)
		require.NoError(t, err)
		assert.Equal(t, expected, detail.Title)
		require.Len(t, detail.Exchanges, 1)
	}
	checkTitle(untitledAsk)
	_, err = pool.Exec(ctx, `UPDATE ask_sessions SET title='Investigating independently owned Ask conversation titles',
		title_model='cheap', title_generated_at=now() WHERE id=$1`, sessionID)
	require.NoError(t, err)
	checkTitle("Investigating independently owned Ask conversation titles")
	_, err = pool.Exec(ctx, `DELETE FROM ask_sessions WHERE id=$1`, sessionID)
	require.NoError(t, err)
}

func TestAskSessionsToolChoicesComeFromRootRunTelemetry(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	server := &Server{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	sessionID, runID := uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO ask_sessions (id, surface) VALUES ($1, 'http')`, sessionID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM ask_sessions WHERE id = $1`, sessionID)
	})
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_runs (id, session_id, user_turn, surface, model_id, started_at, completed_at, duration_ms, outcome)
		VALUES ($1, $2, 0, 'http', 'test-model', now(), now(), 0, 'answered')`, runID, sessionID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_tool_calls (run_id, sequence, workload, call_id, tool_name, started_at, completed_at, duration_ms, outcome)
		VALUES ($1, 0, 'root', 'call-1', 'facet_root_tool', now(), now(), 0, 'ok'),
		       ($1, 1, 'observability_child', 'call-2', 'facet_child_tool', now(), now(), 0, 'ok')`, runID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_messages (session_id, turn, role, message_schema_version, message)
		VALUES ($1, 0, 'assistant', 1,
		        '{"role":"assistant","content_blocks":[{"type":"function_tool_call","function_tool_call":{"call_id":"call-1","name":"facet_transcript_only_tool","arguments":"{}"}}]}')`,
		sessionID)
	require.NoError(t, err)

	listing, _, err := server.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions", nil))
	require.NoError(t, err)
	assert.Contains(t, listing.Tools, "facet_root_tool")
	assert.NotContains(t, listing.Tools, "facet_child_tool", "the filter matches the session's own transcript, which has no child-agent calls")
	assert.NotContains(t, listing.Tools, "facet_transcript_only_tool", "suggestions never read message bodies")
}

func TestAdminPageQueries(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminui"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	s := &Server{
		pool:          pool,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		slackHost:     func() string { return "example.slack.com" },
		viewerBaseURL: func() string { return "https://sessions.example.com" },
	}

	// Empty corpus/queue state is a normal fresh-install state. These
	// calls also execute every overview and prompt-catalog query against
	// the migrated schema so SQL drift fails in CI rather than at runtime.
	_, err = s.loadOverview(ctx, pickOverviewWindow("30d"), time.Now())
	require.NoError(t, err)
	_, err = s.loadHealth(ctx)
	require.NoError(t, err)
	_, _, err = s.loadJobs(ctx, nil)
	require.NoError(t, err)
	_, _, err = s.failureStore().load(ctx, false)
	require.NoError(t, err)
	_, err = promptcatalog.NewStore(pool).EnsureSeed(ctx, promptcatalog.MustEmbedded(), "test:adminui")
	require.NoError(t, err)
	prompts, err := s.loadPrompts(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, prompts.Groups)
	assert.True(t, prompts.Live.Example)
	assert.True(t, prompts.Groups[0].Prompts[0].Example)
	tools, err := s.loadAskTools(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, tools)
	_, err = s.loadPromptVersion(ctx, prompts.Live.ReleaseID)
	require.NoError(t, err)

	created := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	sessionID := uuid.MustParse("1f1fab90-87e8-4b3e-9f7d-082fd73899f4")
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_sessions
		       (id, created_at, last_turn_at, owner_email, title, title_model, title_generated_at,
		        surface)
		VALUES ($1, $2::timestamptz, $2::timestamptz + interval '2 minutes', 'operator@example.com',
		        'Deployment changes', 'gpt-5.4-mini:main', $2::timestamptz + interval '2 minutes',
		        'slack')`, sessionID, created)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO legacy_ask_prompts (id, body, created_by, note, created_at, recovery_source)
		VALUES (18, 'legacy Ask body', 'operator@example.com', 'pre-catalog prompt', $1, 'test restore')`,
		created.Add(-time.Hour))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO legacy_ask_session_prompts (session_id, legacy_prompt_id, recovery_source)
		VALUES ($1, 18, 'test restore')`, sessionID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO slack_channel_info (channel_id, name, is_private, is_archived)
		VALUES ('C123', 'eng-ops', false, false)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_slack_sessions (channel_id, thread_ts, session_id)
		VALUES ('C123', '1700000000.351689', $1)`, sessionID)
	require.NoError(t, err)
	var eventID, chunkID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw)
		VALUES ('slack', 'C123:1700000000.351689', $1, 'What changed?',
		        '{"files":[{"id":"FIMG1","name":"error.png","mimetype":"image/png"}]}'::jsonb)
		RETURNING id`, created).Scan(&eventID)
	require.NoError(t, err)
	err = pool.QueryRow(ctx, `
		INSERT INTO event_chunks (event_id, chunk_idx, kind, content, occurred_at)
		VALUES ($1, 0, 'image_caption', 'a login error dialog', $2)
		RETURNING id`, eventID, created).Scan(&chunkID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO event_images
		       (event_id, source_image_id, storage_key, media_type, chunk_id, captioned_at, occurred_at)
		VALUES ($1, 'FIMG1', 'slack/ab/error.png', 'image/png', $2, $3, $3)`,
		eventID, chunkID, created)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_input_images
		       (session_id, source, source_event_id, source_message_id, source_turn_id,
		        source_image_id, filename, media_type, permalink, delivery_status, failure_reason)
		VALUES ($1, 'slack', $2, '1700000000.351689', '1700000000.351689',
		        'FIMG1', 'error.png', 'image/png', 'https://slack.example/files/FIMG1',
		        'unavailable', 'skipped:too-small')`, sessionID, eventID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_messages
		       (session_id, turn, role, message_schema_version, message, created_at,
		        input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		        incomplete_reason)
		VALUES ($1, 0, 'user', 1,
		        '{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"What changed?"}}]}'::jsonb,
		        $2::timestamptz,
		        NULL, NULL, NULL, NULL, NULL),
		       ($1, 1, 'assistant', 1,
		        '{"role":"assistant","content_blocks":[{"type":"function_tool_call","function_tool_call":{"call_id":"tool-1","name":"retrieve","arguments":"{\"query\":\"changes\"}"}},{"type":"assistant_gen_text","assistant_gen_text":{"text":"The deploy changed."}}],"response_meta":{"token_usage":{"prompt_tokens":180,"prompt_token_details":{"cached_tokens":80},"completion_tokens":25,"total_tokens":205,"completion_token_details":{}}}}'::jsonb,
		        $2::timestamptz + interval '1 minute', 180, 25, 80, NULL, 'max_tokens'),
		       ($1, 2, 'assistant', 1,
		        '{"role":"assistant","content_blocks":[{"type":"assistant_gen_text","assistant_gen_text":{"text":"The deploy changed."}}]}'::jsonb,
		        $2::timestamptz + interval '2 minutes', NULL, NULL, NULL, NULL, NULL)`, sessionID, created)
	require.NoError(t, err)

	// Fifty more rows share the exact timestamp. The second page must
	// still contain the final row: timestamp-only cursors skip it.
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_sessions (id, created_at, last_turn_at)
		SELECT gen_random_uuid(), $1, $1 FROM generate_series(1, 50)`, created)
	require.NoError(t, err)

	first, _, err := s.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions", nil))
	require.NoError(t, err)
	require.Len(t, first.Sessions, askPageSize)
	require.NotEmpty(t, first.NextURL)

	second, _, err := s.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions"+first.NextURL, nil))
	require.NoError(t, err)
	require.Len(t, second.Sessions, 1)
	assert.Empty(t, second.NextURL)
	seen := make(map[string]struct{}, len(first.Sessions))
	for _, row := range first.Sessions {
		seen[row.ID] = struct{}{}
	}
	_, duplicated := seen[second.Sessions[0].ID]
	assert.False(t, duplicated)

	listOne := func(t *testing.T, target string) askSessionRow {
		t.Helper()
		result, found, err := s.loadAskSessions(ctx, httptest.NewRequest("GET", target, nil))
		require.NoError(t, err)
		require.Equal(t, uuid.Nil, found)
		require.Len(t, result.Sessions, 1, target)
		require.Equal(t, sessionID.String(), result.Sessions[0].ID)
		return result.Sessions[0]
	}
	row := listOne(t, "/admin/ask/sessions?where=C123")
	assert.Equal(t, askPlace{Kind: "channel", Label: "#eng-ops"}, row.Place)
	assert.Equal(t, "Deployment changes", row.Title)
	assert.Equal(t, askAnswered, row.State)
	assert.Equal(t, "The deploy changed.", row.Preview)
	assert.Equal(t, 2*time.Minute, row.Took, "without recorded turns, from the first message to the last")
	filtered, _, err := s.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions?where=C123", nil))
	require.NoError(t, err)
	assert.Contains(t, filtered.Channels, askOption{Value: "C123", Label: "#eng-ops"})
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_messages (session_id, turn, role, message_schema_version, message, created_at, incomplete_reason)
		VALUES ($1, 3, 'user', 1,
		        '{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"And after that?"}}]}'::jsonb,
		        $2::timestamptz + interval '3 minutes', NULL),
		       ($1, 4, 'assistant', 1,
		        '{"role":"assistant","content_blocks":[{"type":"assistant_gen_text","assistant_gen_text":{"text":"internal terminal diagnostic"}}]}'::jsonb,
		        $2::timestamptz + interval '4 minutes', 'stop_without_respond')`, sessionID, created)
	require.NoError(t, err)
	diagnosticOnly := listOne(t, "/admin/ask/sessions?where=C123&status=failed")
	assert.Empty(t, diagnosticOnly.Preview, "an earlier answer is not reused after a non-delivered terminal diagnostic")
	assert.Equal(t, "Answer stopped before the assistant submitted a response.", diagnosticOnly.Reason)
	_, err = pool.Exec(ctx, `DELETE FROM ask_messages WHERE session_id = $1 AND turn IN (3, 4)`, sessionID)
	require.NoError(t, err)

	// A recorded turn decides over the last message; a held lease reads as
	// running.
	runID := uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_runs (id, session_id, user_turn, surface, model_id, started_at, completed_at, duration_ms, outcome, error)
		VALUES ($1, $2, 0, 'slack', 'test-model', $3::timestamptz, $3::timestamptz + interval '3 minutes', 9000, 'error', 'slack post failed')`,
		runID, sessionID, created)
	require.NoError(t, err)
	failed := listOne(t, "/admin/ask/sessions?status=failed&where=slack")
	assert.Equal(t, "slack post failed", failed.Reason)
	assert.Equal(t, 9*time.Second, failed.Took)
	_, err = pool.Exec(ctx, `INSERT INTO ask_session_leases (session_id, holder, expires_at) VALUES ($1, $2, now() + interval '1 minute')`, sessionID, uuid.New())
	require.NoError(t, err)
	running := listOne(t, "/admin/ask/sessions?status=running")
	assert.Equal(t, askRunning, running.State)
	assert.Empty(t, running.Reason)
	_, err = pool.Exec(ctx, `DELETE FROM ask_session_leases WHERE session_id = $1`, sessionID)
	require.NoError(t, err)
	// A recorded turn never vouches for a question asked after it ended,
	// however soon after.
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_messages (session_id, turn, role, message_schema_version, message, created_at)
		VALUES ($1, 3, 'user', 1,
		        '{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"Again?"}}]}'::jsonb,
		        $2::timestamptz + interval '4 minutes')`, sessionID, created)
	require.NoError(t, err)
	stale := listOne(t, "/admin/ask/sessions?status=failed&where=C123")
	assert.Equal(t, "No answer was recorded.", stale.Reason, "an earlier turn's error is not this question's")
	_, err = pool.Exec(ctx, `DELETE FROM ask_messages WHERE session_id = $1 AND turn = 3`, sessionID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE ask_runs SET outcome = 'answered', error = NULL WHERE id = $1`, runID)
	require.NoError(t, err)
	listOne(t, "/admin/ask/sessions?status=answered&where=C123")
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_messages (session_id, turn, role, message_schema_version, message, created_at)
		VALUES ($1, 3, 'user', 1,
		        '{"role":"user","content_blocks":[{"type":"user_input_text","user_input_text":{"text":"And then?"}}]}'::jsonb,
		        $2::timestamptz + interval '3 minutes 1 second')`, sessionID, created)
	require.NoError(t, err)
	unanswered := listOne(t, "/admin/ask/sessions?status=failed&where=C123")
	assert.Equal(t, "No answer was recorded.", unanswered.Reason)
	_, err = pool.Exec(ctx, `
		UPDATE ask_messages SET role = 'assistant',
		       message = '{"role":"assistant","content_blocks":[{"type":"reasoning","reasoning":{"text":"Still thinking."}}]}'::jsonb
		 WHERE session_id = $1 AND turn = 3`, sessionID)
	require.NoError(t, err)
	listOne(t, "/admin/ask/sessions?status=failed&where=C123") // reasoning alone is no answer
	_, err = pool.Exec(ctx, `DELETE FROM ask_messages WHERE session_id = $1 AND turn = 3`, sessionID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM ask_runs WHERE id = $1`, runID)
	require.NoError(t, err)

	for name, target := range map[string]string{
		"title":   "/admin/ask/sessions?q=deployment",
		"owner":   "/admin/ask/sessions?owner=OPERATOR@example",
		"surface": "/admin/ask/sessions?where=slack",
		"state":   "/admin/ask/sessions?status=answered&where=C123",
		"tool":    "/admin/ask/sessions?tool=retrieve",
		"dates":   "/admin/ask/sessions?from=2026-08-03&to=2026-08-03&where=C123",
	} {
		t.Run("ask filter "+name, func(t *testing.T) { listOne(t, target) })
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_answer_messages (channel_id, message_ts, session_id, turn)
		VALUES ('C123', '1778880000.000200', $1, 2)`, sessionID)
	require.NoError(t, err)
	for name, query := range map[string]string{
		"id":            sessionID.String(),
		"thread link":   "https://example.slack.com/archives/C123/p1700000000351689",
		"reply link":    "https://example.slack.com/archives/C123/p1700000091000100?thread_ts=1700000000.351689&cid=C123",
		"answered link": "https://example.slack.com/archives/C123/p1778880000000200",
	} {
		t.Run("ask search "+name, func(t *testing.T) {
			_, found, err := s.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions?q="+url.QueryEscape(query), nil))
			require.NoError(t, err)
			assert.Equal(t, sessionID, found, "a pasted ID or link opens its conversation")
		})
	}
	for _, query := range []string{uuid.NewString(), "https://example.slack.com/archives/C999/p1700000000351689"} {
		missing, found, err := s.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions?q="+url.QueryEscape(query), nil))
		require.NoError(t, err)
		assert.Equal(t, uuid.Nil, found)
		assert.Empty(t, missing.Sessions)
		assert.NotEmpty(t, missing.NoMatch)
	}
	invalid, _, err := s.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions?status=idle&where=../x&from=yesterday", nil))
	require.NoError(t, err)
	assert.Contains(t, invalid.FilterError, "State must be")
	assert.Contains(t, invalid.FilterError, "Where must be")
	assert.Contains(t, invalid.FilterError, "From must be a date")
	timeFiltered, _, err := s.loadAskSessions(ctx, httptest.NewRequest("GET", "/admin/ask/sessions?from=2026-08-04", nil))
	require.NoError(t, err)
	assert.Empty(t, timeFiltered.Sessions)

	detail, err := s.loadAskSession(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, "Deployment changes", detail.Title)
	assert.Equal(t, askAnswered, detail.State)
	assert.Equal(t, "#eng-ops", detail.Place.Label)
	assert.Equal(t, "https://example.slack.com/archives/C123/p1700000000351689", detail.Place.Href)
	require.NotNil(t, detail.Release)
	assert.Equal(t, int64(18), detail.Release.ID)
	assert.True(t, detail.Release.Legacy)
	assert.Equal(t, "operator@example.com", detail.Release.By)
	assert.Equal(t, "pre-catalog prompt", detail.Release.Note)
	assert.Equal(t, int64(1), detail.ToolCalls)
	assert.Equal(t, int64(1), detail.Questions)
	assert.Equal(t, int64(180), detail.InputTokens)
	require.Len(t, detail.Images, 1)
	assert.Equal(t, "/admin/ask/images/FIMG1", detail.Images[0].ProxyURL)
	assert.False(t, detail.Images[0].Described)
	assert.Equal(t, "a login error dialog", detail.Images[0].CurrentCaption)
	require.Len(t, detail.Exchanges, 1)
	exchange := detail.Exchanges[0]
	require.Len(t, exchange.Steps, 2, "the call, and the remark beside it")
	assert.Equal(t, "retrieve", exchange.Steps[0].Tool)
	assert.False(t, exchange.Steps[0].Returned)
	assert.Equal(t, "note", exchange.Steps[1].Kind)
	require.Len(t, exchange.Answers, 1)
	assert.Contains(t, string(exchange.Answers[0].Body), "The deploy changed.")

	_, err = pool.Exec(ctx, `
		INSERT INTO ask_messages
		       (session_id, turn, role, message_schema_version, message, created_at)
		SELECT $1, n, 'assistant', 1,
		       jsonb_build_object(
		         'role', 'assistant',
		         'content_blocks', jsonb_build_array(jsonb_build_object(
		           'type', 'assistant_gen_text',
		           'assistant_gen_text', jsonb_build_object('text', 'turn ' || n::text)))),
		       $2::timestamptz + n * interval '1 second'
		  FROM generate_series(3, 206) n`, sessionID, created)
	require.NoError(t, err)
	windowed, err := s.loadAskSession(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, windowed.Window)
	assert.Equal(t, askWindow{From: 0, To: 99, Total: 207, LaterURL: "?from=100&to=199"}, *windowed.Window)
	assert.Equal(t, 207, windowed.Messages)
	require.Len(t, windowed.Exchanges, 1)
	assert.Len(t, windowed.Exchanges[0].Answers, 98, "every answer in the window is kept")
	assert.Equal(t, "turn-99", windowed.Exchanges[0].Answers[97].Anchor)

	secondWindow, err := s.loadAskSessionWindow(ctx, sessionID, 100, 206)
	require.NoError(t, err)
	require.NotNil(t, secondWindow.Window)
	assert.Equal(t, askWindow{From: 100, To: 206, Total: 207, EarlierURL: "?from=0&to=99"}, *secondWindow.Window)
	require.Len(t, secondWindow.Exchanges, 1)
	assert.True(t, secondWindow.Exchanges[0].Continued)
	assert.Len(t, secondWindow.Exchanges[0].Answers, 107)
}
