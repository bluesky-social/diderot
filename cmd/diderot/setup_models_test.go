package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// TestModelTesterReportsWithoutEchoing: each role gets one tiny request on
// its own protocol, and a failure is classified without the provider's body.
func TestModelTesterReportsWithoutEchoing(t *testing.T) {
	// The tester probes roles concurrently.
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/responses":
			assert.Equal(t, float64(modelTestTokens), body["max_output_tokens"])
			assert.Equal(t, map[string]any{"effort": "low"}, body["reasoning"])
			_, _ = io.WriteString(w, `{"id":"r"}`)
		case "/v1/chat/completions":
			assert.Equal(t, float64(modelTestTokens+100), body["max_completion_tokens"])
			assert.Equal(t, "on", body["extra"])
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid key sk-live-SECRET"}`)
		case "/v1/messages":
			_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","content":[]}`)
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	serveEnv(t)
	t.Setenv("DIDEROT_ASK_ENABLED", "true")
	cfg, catalog := loadInference(t, modelregistry.Provider{Name: "test", Kind: modelregistry.KindOpenAI, Endpoint: llmclient.Endpoint{
		BaseURL: server.URL + "/v1", APIKey: "sk-live-SECRET", Auth: llmclient.AuthBearer,
	}})
	// Every role with a saved model is tested, and embeddings.
	saved := settings.Models{
		Roles: map[string]string{
			"ask": "test/asker", "titles": "test/asker", "session_summary_map": "test/claude", "session_summary_reduce": "test/claude",
			"thread_summaries": "test/chatter",
		},
		Available: []modelregistry.Model{
			{Provider: "test", ID: "asker", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses}, ReasoningEfforts: []string{"low", "high"}},
			{Provider: "test", ID: "claude", Protocols: []modelregistry.Protocol{modelregistry.ProtocolMessages}},
			{Provider: "test", ID: "chatter", Protocols: []modelregistry.Protocol{modelregistry.ProtocolChat},
				ChatTokenParam: llmclient.ChatMaxCompletionTokens, ChatReasoningTokenAllowance: 100, ChatExtraBody: map[string]any{"extra": "on"}},
		},
		Embedding: &settings.Embedding{Provider: "test", Model: "embedder", Dimensions: 3, Collection: "c"},
	}
	results := newModelTester(cfg, catalog)(context.Background(), saved)
	byRole := map[string]settings.RoleTest{}
	for _, result := range results {
		byRole[result.Role] = result
		assert.NotContains(t, result.Error, "SECRET")
		assert.NotContains(t, result.Error, server.URL)
	}
	require.Len(t, byRole, 6, "the five roles with a model, and embeddings")
	for _, role := range []string{"Ask", "Titles"} {
		assert.Equal(t, settings.RoleTest{Role: role, Model: "test/asker", Passed: true}, byRole[role])
	}
	for _, role := range []string{"Session summaries, part by part", "Session summaries, whole session"} {
		assert.Equal(t, settings.RoleTest{Role: role, Model: "test/claude", Passed: true}, byRole[role])
	}
	assert.Equal(t, settings.RoleTest{Role: "Thread summaries", Model: "test/chatter", Error: "HTTP 401 Unauthorized"}, byRole["Thread summaries"])
	assert.False(t, byRole["Embeddings"].Passed)
	assert.Contains(t, byRole["Embeddings"].Error, "returned 2 dimensions")
	mu.Lock()
	assert.ElementsMatch(t, []string{
		"/v1/responses", "/v1/responses", "/v1/messages", "/v1/messages",
		"/v1/chat/completions", "/v1/embeddings",
	}, paths)
	paths = nil
	mu.Unlock()

	// One role alone.
	assert.Equal(t, []settings.RoleTest{{Role: "Ask", Model: "test/asker", Passed: true}},
		newModelTester(cfg, catalog)(context.Background(), saved, "ask"))
	mu.Lock()
	assert.Equal(t, []string{"/v1/responses"}, paths)
	mu.Unlock()

	server.Close()
	for _, result := range newModelTester(cfg, catalog)(context.Background(), saved) {
		assert.Equal(t, "could not reach the provider", result.Error, result.Role)
	}
}

func TestWhileLiveSkipsWorkDuringSetup(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "cmd_setup"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
		UPDATE connector_settings
		   SET settings = '{"state":"setup","bootstrap_admin_enabled":true}', version = 1
		 WHERE connector = 'org'`)
	require.NoError(t, err)
	store := settings.NewStore(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, store.Load(ctx))

	ticks := 0
	tick := whileLive(store, func(context.Context) error { ticks++; return nil })
	require.NoError(t, tick(ctx))
	assert.Zero(t, ticks, "scheduled reports wait for go-live")

	_, err = store.UpdateOrg(ctx, 1, settings.Actor{Email: "op@example.com"}, "test", func(org *settings.Org) error {
		org.State = settings.StateLive
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, tick(ctx))
	assert.Equal(t, 1, ticks)
}
