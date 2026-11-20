package ask

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAskRunMetadataExactRequest(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		bodies  []map[string]any
		headers []http.Header
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		bodies = append(bodies, body)
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		writeAskAgentsFixture(t, w, "resp_metadata", "done")
	}))
	defer server.Close()

	mdl, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL: server.URL + "/v1", APIKey: "secret", ModelID: "test",
	})
	require.NoError(t, err)
	agents, err := NewAskAgents(context.Background(), mdl, nil, "fixed prompt", 3)
	require.NoError(t, err)

	sessionID := uuid.MustParse("01900000-0000-7000-8000-000000000001")
	opt, err := withAskRunMetadata(AskRunMetadata{SessionID: sessionID}, model.WithMaxTokens(321))
	require.NoError(t, err)
	runCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	iter := agents.Slack.Query(runCtx, "question", opt)
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, event.Err)
	}

	mu.Lock()
	recorded := append([]map[string]any(nil), bodies...)
	recordedHeaders := append([]http.Header(nil), headers...)
	mu.Unlock()
	require.Len(t, recorded, 1)
	require.Len(t, recordedHeaders, 1)
	body := recorded[0]
	require.Equal(t, float64(321), body["max_output_tokens"], "additional model options must compose with metadata")
	require.NotContains(t, body, "metadata", "a model gateway may reject non-empty Responses metadata")
	require.Equal(t, sessionID.String(), recordedHeaders[0].Get("x-agent-parent-session-id"))
	require.Empty(t, recordedHeaders[0].Get("x-bedrock-metadata"), "provider invocation attribution header must be absent")
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret")
}

func TestAskRunMetadataValidation(t *testing.T) {
	t.Parallel()

	_, err := withAskRunMetadata(AskRunMetadata{})
	require.ErrorContains(t, err, "session ID")
}
