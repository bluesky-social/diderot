package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/stretchr/testify/require"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestGatewayReadinessCheckDisabledWhenAskOff(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{} // AskEnabled defaults to false
	check, err := gatewayReadinessCheck(context.Background(), cfg, &inferenceModels{}, testLogger())
	require.NoError(t, err)
	require.Nil(t, check, "no gateway probe when ask is disabled")
}

func TestGatewayReadinessCheckEnabledWhenAskConfigured(t *testing.T) {
	var gotModel atomic.Value
	gotModel.Store("")
	var gotThinking atomic.Value
	gotThinking.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/messages", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		gotModel.Store(body["model"])
		gotThinking.Store(body["thinking"].(map[string]any)["type"])
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_ping", "type": "message", "role": "assistant",
			"model": "claude-opus-5:main", "stop_reason": "end_turn",
			"content": []any{map[string]any{"type": "text", "text": "p"}},
			"usage":   map[string]any{"input_tokens": 2, "output_tokens": 1},
		}))
	}))
	defer server.Close()
	cfg := &config.Config{
		AskEnabled:           true,
		AskReadinessTimeout:  2 * time.Second,
		AskReadinessInterval: 10 * time.Second,
	}
	catalog := newModelCatalog(cfg, nil)
	catalog.providers = func() []modelregistry.Provider {
		return []modelregistry.Provider{{Name: "gateway", Kind: modelregistry.KindOpenAI, Endpoint: llmclient.Endpoint{
			BaseURL: server.URL + "/v1", APIKey: "sekret", Auth: llmclient.AuthBearer,
		}}}
	}
	// A model on the Messages protocol takes no service tier.
	saved := gatewayMigration(map[string]string{"ask": "claude-opus-5:main"})
	saved.AskServiceTier = ""
	models := catalog.resolve(saved)
	check, err := gatewayReadinessCheck(context.Background(), cfg, models, testLogger())
	require.NoError(t, err)
	require.NotNil(t, check)
	require.NoError(t, check.CheckOnce(context.Background()))
	require.True(t, check.Ready())
	require.Equal(t, "claude-opus-5:main", gotModel.Load().(string))
	require.Equal(t, "disabled", gotThinking.Load().(string))
}
