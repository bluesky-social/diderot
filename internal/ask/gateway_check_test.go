package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gatewayFixtureServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(server.Close)
	return server
}

// writeGatewayOK writes the minimal valid Responses body the Eino model
// turns into an assistant message with one output block.
func writeGatewayOK(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"id": "resp_ping", "object": "response", "created_at": 1,
		"status": "completed", "model": "gpt-5.6-sol:main",
		"output": []any{map[string]any{
			"type": "message", "id": "msg_ping", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": "p", "annotations": []any{},
			}},
		}},
		"usage": map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3},
	}))
}

func newTestGatewayCheck(t *testing.T, serverURL string, opts GatewayCheckOptions) *GatewayCheck {
	t.Helper()
	opts.BaseURL = serverURL + "/v1"
	opts.APIKey = "test-gateway-token"
	if opts.Model.ID == "" {
		opts.Model = registryModel(t, "gpt-5.6-sol:main")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	check, err := NewGatewayCheck(context.Background(), opts)
	require.NoError(t, err)
	return check
}

func TestGatewayCheckCurrentModelsUseLowestSupportedEffort(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		modelID string
		effort  string
	}{
		{modelID: "gpt-6-sol:main", effort: "none"},
		{modelID: "gpt-6.1-sol:main", effort: "low"},
		{modelID: "gpt-6-astra:main", effort: "low"},
	} {
		t.Run(testCase.modelID, func(t *testing.T) {
			t.Parallel()
			server := gatewayFixtureServer(t, func(writer http.ResponseWriter, request *http.Request) {
				var body map[string]any
				require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
				require.Equal(t, testCase.modelID, body["model"])
				require.Equal(t, testCase.effort, body["reasoning"].(map[string]any)["effort"])
				require.Equal(t, "fast", body["service_tier"])
				require.Equal(t, float64(gatewayCheckMaxTokens), body["max_output_tokens"])
				require.NotContains(t, body, "prompt_cache_retention")
				writeGatewayOK(t, writer)
			})
			check := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{
				Model: registryModel(t, testCase.modelID), ServiceTier: "fast",
			})
			require.NoError(t, check.CheckOnce(t.Context()))
			require.True(t, check.Ready())
		})
	}
}

// gatewayProbeInput is the JSON shape the Eino model serializes for a
// single user AgenticMessage: one easy message item with a plain string.
func gatewayProbeInput(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["input"].([]any)
	require.True(t, ok, "input must serialize as a JSON array")
	items := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		obj, ok := item.(map[string]any)
		require.True(t, ok, "input items must be objects")
		items = append(items, obj)
	}
	return items
}

func TestGatewayCheckNotReadyUntilFirstCheck(t *testing.T) {
	t.Parallel()

	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGatewayOK(t, w)
	})
	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{})
	assert.False(t, g.Ready(), "a fresh checker must never report ready")
	assert.Nil(t, g.LastError())
}

func TestGatewayCheckProbeSuccessContract(t *testing.T) {
	t.Parallel()

	server := gatewayFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/responses", r.URL.Path)
		assert.Equal(t, "Bearer test-gateway-token", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.NotEmpty(t, r.Header.Get("x-agent-parent-session-id"))
		assert.Empty(t, r.Header.Get("x-bedrock-metadata"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		// The probe exercises the exact ask request shape so gateway wire
		// drift (like a rejected prompt_cache_retention) fails readiness
		// with the same error a real turn would hit.
		assert.Equal(t, "gpt-5.6-sol:direct", body["model"])
		assert.Equal(t, "fast", body["service_tier"])
		assert.Equal(t, float64(gatewayCheckMaxTokens), body["max_output_tokens"])
		assert.Equal(t, false, body["store"])
		assert.Equal(t, "24h", body["prompt_cache_retention"])
		assert.NotContains(t, body, "tools")
		assert.NotContains(t, body, "metadata")

		items := gatewayProbeInput(t, body)
		require.Len(t, items, 1)
		// EasyInputMessage serializes without an explicit type; role +
		// content are the contract.
		assert.Equal(t, "user", items[0]["role"])
		assert.Equal(t, "ping", items[0]["content"])
		writeGatewayOK(t, w)
	})

	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{
		Model: registryModel(t, "gpt-5.6-sol:direct"), ServiceTier: "fast",
	})
	require.NoError(t, g.CheckOnce(context.Background()))
	assert.True(t, g.Ready())
	assert.Nil(t, g.LastError())
	assert.False(t, g.LastSuccess().IsZero())
}

func TestGatewayCheckProbeNativeClaudeContract(t *testing.T) {
	t.Parallel()

	server := gatewayFixtureServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/messages", r.URL.Path)
		assert.Equal(t, "Bearer test-gateway-token", r.Header.Get("Authorization"))
		assert.NotEmpty(t, r.Header.Get("x-agent-parent-session-id"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "claude-opus-5:main", body["model"])
		assert.Equal(t, float64(gatewayCheckMaxTokens), body["max_tokens"])
		assert.Equal(t, "disabled", body["thinking"].(map[string]any)["type"])
		assert.NotContains(t, body, "output_config")
		assert.NotContains(t, body, "tools")

		messages := body["messages"].([]any)
		require.Len(t, messages, 1)
		assert.Equal(t, "user", messages[0].(map[string]any)["role"])

		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"id":"msg_ping","type":"message","role":"assistant","model":"claude-opus-5:main","content":[{"type":"text","text":"p"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}`))
		require.NoError(t, err)
	})

	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{Model: registryModel(t, "claude-opus-5:main")})
	require.NoError(t, g.CheckOnce(context.Background()))
	assert.True(t, g.Ready())
}

func TestGatewayCheckProbeHTTPError(t *testing.T) {
	t.Parallel()

	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"prompt_cache_retention must be in_memory"}}`, http.StatusBadRequest)
	})

	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{})
	err := g.CheckOnce(context.Background())
	require.Error(t, err)
	assert.False(t, g.Ready())
	assert.Contains(t, err.Error(), "400", "the diagnostic should surface the gateway status")
	// The diagnostic must never include the API key.
	assert.NotContains(t, err.Error(), "test-gateway-token")
}

func TestGatewayCheckProbeEmptyOutputSucceeds(t *testing.T) {
	t.Parallel()

	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_empty", "object": "response", "created_at": 1,
			"status": "completed", "model": "claude-opus-5:main", "output": []any{},
		}))
	})

	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{})
	require.NoError(t, g.CheckOnce(context.Background()))
	assert.True(t, g.Ready(), "one output token may be consumed without a visible content block")
}

func TestGatewayCheckProbeInvalidJSON(t *testing.T) {
	t.Parallel()

	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})

	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{})
	require.Error(t, g.CheckOnce(context.Background()))
	assert.False(t, g.Ready())
}

func TestGatewayCheckProbeTimeout(t *testing.T) {
	t.Parallel()

	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(800 * time.Millisecond)
		writeGatewayOK(t, w)
	})

	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{Timeout: 100 * time.Millisecond})
	start := time.Now()
	err := g.CheckOnce(context.Background())
	assert.Error(t, err, "a hung gateway must fail inside the probe timeout")
	assert.False(t, g.Ready())
	assert.Less(t, time.Since(start), 400*time.Millisecond,
		"the probe timeout must bound retries and backoff, not only each HTTP attempt")
}

func TestGatewayCheckVerdictTracksLatestCheck(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool
	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			http.Error(w, "gateway down", http.StatusServiceUnavailable)
			return
		}
		writeGatewayOK(t, w)
	})

	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{})
	require.NoError(t, g.CheckOnce(context.Background()))
	require.True(t, g.Ready())

	// A later failure must flip the cached verdict, not stick to the
	// earlier success.
	failing.Store(true)
	require.Error(t, g.CheckOnce(context.Background()))
	assert.False(t, g.Ready())
	assert.NotNil(t, g.LastError())

	// And recovery flips it back.
	failing.Store(false)
	require.NoError(t, g.CheckOnce(context.Background()))
	assert.True(t, g.Ready())
	assert.Nil(t, g.LastError())
}

func TestGatewayCheckUpdateHook(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool
	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		writeGatewayOK(t, w)
	})

	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{})
	var (
		hookMu sync.Mutex
		got    []bool
	)
	g.SetUpdateHook(func(ready bool, _ error) {
		hookMu.Lock()
		defer hookMu.Unlock()
		got = append(got, ready)
	})

	failing.Store(true)
	require.Error(t, g.CheckOnce(context.Background()))
	failing.Store(false)
	require.NoError(t, g.CheckOnce(context.Background()))

	hookMu.Lock()
	defer hookMu.Unlock()
	require.Equal(t, []bool{false, true}, got)
}

func TestGatewayCheckRunStartsImmediatelyAndCaches(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeGatewayOK(t, w)
	})

	// Long interval: proves Run performs an immediate check rather than
	// waiting for the first tick, and that Ready() serves the cached
	// verdict without triggering further requests between ticks.
	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(ctx)
	}()

	require.Eventually(t, func() bool { return g.Ready() }, 2*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), calls.Load(), "no extra probe requests between interval ticks")

	cancel()
	<-done
}

func TestGatewayCheckRunInterval(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeGatewayOK(t, w)
	})

	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{Interval: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(ctx)
	}()

	require.Eventually(t, func() bool { return calls.Load() >= 3 }, 2*time.Second, 10*time.Millisecond)
	cancel()
	<-done
}

func TestGatewayCheckConcurrentAccess(t *testing.T) {
	t.Parallel()

	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGatewayOK(t, w)
	})
	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_ = g.CheckOnce(context.Background())
			case 1:
				_ = g.Ready()
			case 2:
				_ = g.LastError()
			}
		}()
	}
	wg.Wait()
	assert.True(t, g.Ready())
}

func TestGatewayCheckSerializesConcurrentProbes(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	server := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		writeGatewayOK(t, w)
	})
	g := newTestGatewayCheck(t, server.URL, GatewayCheckOptions{})
	errs := make(chan error, 2)

	go func() { errs <- g.CheckOnce(context.Background()) }()
	<-entered
	go func() { errs <- g.CheckOnce(context.Background()) }()

	secondEntered := false
	select {
	case <-entered:
		secondEntered = true
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.False(t, secondEntered, "a second probe entered before the first completed")
}

func TestGatewayCheckMetrics(t *testing.T) {
	t.Parallel()

	okServer := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeGatewayOK(t, w)
	})
	g := newTestGatewayCheck(t, okServer.URL, GatewayCheckOptions{})
	require.NoError(t, g.CheckOnce(context.Background()))
	assert.Equal(t, float64(1), testutil.ToFloat64(g.metrics.checksTotal.WithLabelValues("success")))
	assert.Equal(t, float64(0), testutil.ToFloat64(g.metrics.checksTotal.WithLabelValues("error")))
	assert.NotZero(t, testutil.ToFloat64(g.metrics.lastSuccess))

	badServer := gatewayFixtureServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	g2 := newTestGatewayCheck(t, badServer.URL, GatewayCheckOptions{})
	require.Error(t, g2.CheckOnce(context.Background()))
	assert.Equal(t, float64(1), testutil.ToFloat64(g2.metrics.checksTotal.WithLabelValues("error")))
}
