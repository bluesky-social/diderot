package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminjob"
	"github.com/bluesky-social/diderot/internal/auth"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/reembed"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// stringReader wraps strings.NewReader as an io.ReadCloser so it can
// be plugged into httptest.NewRequest.
func stringReader(s string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(s))
}

// recordingMux keeps the pattern of each route mounted on it.
type recordingMux struct{ patterns []string }

func (m *recordingMux) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
}

// TestMountedRoutes pins the HTTP API: corpus use, Ask, and the owner-bound
// session routes. Administration has no route here; it is the /admin UI.
func TestMountedRoutes(t *testing.T) {
	mux := &recordingMux{}
	require.NoError(t, newTestServer("token", nil).Mount(mux))
	assert.ElementsMatch(t, []string{
		"POST /v1/retrieve",
		"POST /v1/list",
		"POST /v1/expand-chunk",
		"POST /v1/ask",
		"GET /v1/ask/{sessionID}/artifacts/{artifactID}",
		"GET /v1/me/agent-sessions",
		"PUT /v1/me/agent-session-privacy",
		"PUT /v1/ingest/agent-sessions/{session_id}",
		"GET /v1/ingest/agent-sessions/{session_id}",
	}, mux.patterns)
}

// TestUseRoutesNeedABearerButNoAdminGroup: a caller outside the
// administrators' groups reaches each use route's own validation, and no
// bearer is refused before it.
func TestUseRoutesNeedABearerButNoAdminGroup(t *testing.T) {
	claims := &auth.Principal{Subject: "user", Email: "user@example.com", Groups: []string{"everyone"}}
	s := NewServer(fakeVerifier{validToken: "token", claims: claims}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetAdminGroups([]string{"admins"})
	mux := http.NewServeMux()
	require.NoError(t, s.Mount(mux))

	for path, want := range map[string]string{
		"/v1/ask":          "question required",
		"/v1/retrieve":     "question required",
		"/v1/list":         "list not configured",
		"/v1/expand-chunk": "expand backend not configured",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		assert.Contains(t, w.Body.String(), want, path)
		assert.NotEqual(t, http.StatusForbidden, w.Code, path)

		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		assert.Equal(t, http.StatusUnauthorized, w.Code, path)
	}
}

func servicesTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.URL(t, "adminapi"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(context.Background(),
		`TRUNCATE admin_jobs, events, slack_channel_info, event_images RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return pool
}

// seedImageEvent stores a Slack message that carries one hosted image.
func seedImageEvent(t *testing.T, pool *pgxpool.Pool, channelID, ts, fileID, scope string) int64 {
	t.Helper()
	raw := `{"type":"message","subtype":"file_share","channel":"` + channelID +
		`","user":"U1","ts":"` + ts + `","message":{"user":"U1","ts":"` + ts +
		`","files":[{"id":"` + fileID + `","name":"diagram.png","mimetype":"image/png","mode":"hosted"}]}}`
	var eventID int64
	require.NoError(t, pool.QueryRow(context.Background(), `
		INSERT INTO events (source, external_id, occurred_at, actor, body, raw, scope)
		VALUES ('slack', $1, now(), 'tester', 'file', $2::jsonb, $3) RETURNING id
	`, channelID+":"+ts, raw, scope).Scan(&eventID))
	_, err := pool.Exec(context.Background(), `
		INSERT INTO slack_message_details (event_id, channel_id, user_id, has_files, occurred_at)
		VALUES ($1, $2, 'U1', true, now())
	`, eventID, channelID)
	require.NoError(t, err)
	return eventID
}

// TestSlackImageReplayJobSurvivesRequestCancellation: the replay rebuilds a
// channel's derived image rows in a job that outlives the request that
// started it, and a second one is refused while it runs.
func TestSlackImageReplayJobSurvivesRequestCancellation(t *testing.T) {
	pool := servicesTestPool(t)
	ctx := context.Background()
	eventID := seedImageEvent(t, pool, "C1", "1700000000.000001", "F1", events.ScopePublic)

	var chunkID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO event_chunks (event_id, kind, chunk_idx, content, occurred_at)
		VALUES ($1, 'image_caption', 0, 'stale caption', now()) RETURNING id`, eventID).Scan(&chunkID))
	_, err := pool.Exec(ctx, `
		INSERT INTO event_images
		       (event_id, source_image_id, media_type, occurred_at, captioned_at, chunk_id)
		VALUES ($1, 'F1', 'image/png', now(), now(), $2)`, eventID, chunkID)
	require.NoError(t, err)

	server := NewServer(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetStatusPool(pool)
	_, err = server.StartSlackImageReplay(ctx, "operator@example.com", "CUNKNOWN")
	require.EqualError(t, err, "slack channel not found")

	requestCtx, cancel := context.WithCancel(ctx)
	jobID, err := server.StartSlackImageReplay(requestCtx, "Operator@Example.com", "C1")
	require.NoError(t, err)
	cancel()

	store := adminjob.NewStore(pool)
	var job adminjob.Job
	require.Eventually(t, func() bool {
		job, err = store.Latest(ctx, slackImageReplayJobKind)
		return err == nil && job.ID == jobID && job.Status == "succeeded"
	}, 3*time.Second, 20*time.Millisecond)
	assert.Equal(t, "operator@example.com", job.RequestedBy)
	var progress SlackImageReplayProgress
	require.NoError(t, json.Unmarshal(job.Progress, &progress))
	assert.Equal(t, "complete", progress.Phase)
	assert.True(t, progress.Done)
	assert.Equal(t, 1, progress.Scanned)
	assert.Equal(t, 1, progress.Reconciled)

	var rows, oldChunks int
	var captioned bool
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), bool_or(captioned_at IS NOT NULL)
		  FROM event_images WHERE event_id=$1`, eventID).Scan(&rows, &captioned))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM event_chunks WHERE id=$1`, chunkID).Scan(&oldChunks))
	assert.Equal(t, 1, rows)
	assert.False(t, captioned)
	assert.Zero(t, oldChunks)

	_, err = pool.Exec(ctx, `
		INSERT INTO admin_jobs (kind, status, requested_by, request, started_at, stale_after_seconds)
		VALUES ('slack_image_replay', 'running', 'operator@example.com', '{"channel_id":"COTHER"}', now(), 120)`)
	require.NoError(t, err)
	_, err = server.StartSlackImageReplay(ctx, "operator@example.com", "")
	assert.ErrorIs(t, err, adminjob.ErrActive)
}

type fixedScopes []string

func (f fixedScopes) ScopesForEmail(context.Context, string) []string { return f }

type fixedBytes struct{}

func (fixedBytes) Get(context.Context, string) ([]byte, string, error) {
	return []byte("PNGDATA"), "", nil
}

// TestAskInputImageIsAnyImageAConversationHolds: an image of a private
// channel is served once an Ask conversation was given it, whatever channels
// the administrator reading that conversation is in, and an image no
// conversation holds is not found, exactly as one that does not exist.
func TestAskInputImageIsAnyImageAConversationHolds(t *testing.T) {
	pool := servicesTestPool(t)
	ctx := context.Background()
	eventID := seedImageEvent(t, pool, "CPRIV", "1700000000.000030", "F_SECRET", events.ScopeChannel("CPRIV"))
	pending := seedImageEvent(t, pool, "CPRIV", "1700000000.000031", "F_PENDING", events.ScopeChannel("CPRIV"))
	_, err := pool.Exec(ctx, `
		INSERT INTO event_images (event_id, source_image_id, storage_key, media_type, occurred_at)
		VALUES ($1, 'F_SECRET', 's3://bucket/secret.png', 'image/png', now()),
		       ($2, 'F_PENDING', NULL, 'image/png', now())`, eventID, pending)
	require.NoError(t, err)

	server := NewServer(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetStatusPool(pool)
	server.SetFileBytesGetter(fixedBytes{})
	// The administrator is in neither channel.
	server.SetScopeResolver(fixedScopes{events.ScopePublic, events.ScopeInternal})

	_, _, err = server.AskInputImage(ctx, "F_SECRET")
	assert.ErrorIs(t, err, ErrImageNotFound, "no conversation holds it yet")
	_, _, err = server.AskInputImage(ctx, "F_NEVER_STORED")
	assert.ErrorIs(t, err, ErrImageNotFound)

	var sessionID string
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id::text`).Scan(&sessionID))
	_, err = pool.Exec(ctx, `
		INSERT INTO ask_input_images (session_id, source, source_event_id, source_message_id, source_turn_id, source_image_id, delivery_status)
		VALUES ($1, 'slack', $2, '1700000000.000030', '1700000000.000030', 'F_SECRET', 'captioned'),
		       ($1, 'slack', $3, '1700000000.000031', '1700000000.000031', 'F_PENDING', 'unavailable')`, sessionID, eventID, pending)
	require.NoError(t, err)

	data, mediaType, err := server.AskInputImage(ctx, "F_SECRET")
	require.NoError(t, err)
	assert.Equal(t, "PNGDATA", string(data))
	assert.Equal(t, "image/png", mediaType)
	_, _, err = server.AskInputImage(ctx, "F_PENDING")
	assert.ErrorIs(t, err, ErrImageNotMirrored)

	_, err = pool.Exec(ctx, `UPDATE events SET deleted = true WHERE id = $1`, eventID)
	require.NoError(t, err)
	_, _, err = server.AskInputImage(ctx, "F_SECRET")
	assert.ErrorIs(t, err, ErrImageNotFound, "a deleted message's image is gone")
}

// TestStartReembedRecordsTheSelectedSources: the request names the sources to
// re-embed, and a second request waits for the first.
func TestStartReembedRecordsTheSelectedSources(t *testing.T) {
	pool := servicesTestPool(t)
	ctx := context.Background()
	server := NewServer(nil, nil)
	_, err := server.StartReembed(ctx, "operator@example.com", nil)
	require.ErrorIs(t, err, errNoJobStore)

	server.SetStatusPool(pool)
	jobID, err := server.StartReembed(ctx, "operator@example.com", []string{"slack", "notion"})
	require.NoError(t, err)
	job, err := adminjob.NewStore(pool).Latest(ctx, reembed.JobKind)
	require.NoError(t, err)
	assert.Equal(t, jobID, job.ID)
	assert.Equal(t, "queued", job.Status)
	assert.Equal(t, "operator@example.com", job.RequestedBy)
	assert.JSONEq(t, `{"sources":["slack","notion"]}`, string(job.Request))

	_, err = server.StartReembed(ctx, "operator@example.com", nil)
	assert.ErrorIs(t, err, adminjob.ErrActive)
}

// namedInviter resolves one channel by name and records the invites.
type namedInviter struct{ invited []string }

func (*namedInviter) ResolveChannel(_ context.Context, nameOrID string) (string, string, error) {
	if strings.TrimPrefix(nameOrID, "#") == "platform" || nameOrID == "CPLATFORM" {
		return "CPLATFORM", "platform", nil
	}
	return "", "", slackcx.ErrChannelNotFound
}

func (i *namedInviter) Invite(_ context.Context, channelID string) error {
	if len(i.invited) > 0 {
		return errors.New("already_in_channel")
	}
	i.invited = append(i.invited, channelID)
	return nil
}

func (*namedInviter) IsBotInChannel(context.Context, string) (bool, error) { return true, nil }

func (*namedInviter) Kick(context.Context, string) error { return errors.New("not_in_channel") }

// TestSlackChannelInviteByName: a channel's name resolves to its ID, which
// the membership action invites the bot to; both need Slack on and its user
// token.
func TestSlackChannelInviteByName(t *testing.T) {
	ctx := context.Background()
	server := NewServer(nil, nil)
	_, _, err := server.ResolveSlackChannel(ctx, "#platform")
	require.ErrorIs(t, err, ErrSlackOff)

	controls := SlackControls{}
	server.SetSlackControls(func() (SlackControls, bool) { return controls, true })
	_, _, err = server.ResolveSlackChannel(ctx, "#platform")
	require.ErrorIs(t, err, ErrNoSlackUserToken)
	_, err = server.SetSlackMembership(ctx, "CPLATFORM", true)
	require.ErrorIs(t, err, ErrNoSlackUserToken)

	inviter := &namedInviter{}
	controls.Inviter = inviter
	channelID, name, err := server.ResolveSlackChannel(ctx, " #platform ")
	require.NoError(t, err)
	assert.Equal(t, "CPLATFORM", channelID)
	assert.Equal(t, "platform", name)
	_, _, err = server.ResolveSlackChannel(ctx, "nowhere")
	require.ErrorIs(t, err, slackcx.ErrChannelNotFound)

	already, err := server.SetSlackMembership(ctx, channelID, true)
	require.NoError(t, err)
	assert.False(t, already)
	already, err = server.SetSlackMembership(ctx, channelID, true)
	require.NoError(t, err)
	assert.True(t, already, "a second invite finds the bot already there")
	assert.Equal(t, []string{"CPLATFORM"}, inviter.invited)
	notThere, err := server.SetSlackMembership(ctx, channelID, false)
	require.NoError(t, err)
	assert.True(t, notThere)
}
