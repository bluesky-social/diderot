package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/google/uuid"
	slackgo "github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
)

type answerArtifactRouter struct {
	progressIntegrationRouter
	delivered bool
}

func (router *answerArtifactRouter) ReadArtifact(context.Context, uuid.UUID, uuid.UUID) (ask.ArtifactRecord, []byte, error) {
	return ask.ArtifactRecord{ContentHash: "panel-hash"}, []byte("panel-png"), nil
}

func (router *answerArtifactRouter) ArtifactDeliveryExists(context.Context, uuid.UUID, int, uuid.UUID, string, string) (bool, error) {
	return router.delivered, nil
}

func (router *answerArtifactRouter) RecordArtifactDelivery(_ context.Context, _ uuid.UUID, _ int, _ uuid.UUID, _, _, _, status, _ string) error {
	router.delivered = status == "delivered"
	return nil
}

func TestDeliverAskAnswerRetriesArtifactsWithoutRepostingMarkdown(t *testing.T) {
	pool := getConsumerPool(t)
	sessionID := uuid.MustParse(seedSession(t, pool))
	router := &answerArtifactRouter{}
	posts, uploads := 0, 0
	posted := ""
	failUpload := true
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.NoError(t, request.ParseForm())
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth.test":
			_, _ = writer.Write([]byte(`{"ok":true,"user_id":"UBOT"}`))
		case "/conversations.replies":
			messages := []map[string]string{{"ts": "100.000", "text": "alert"}}
			if posted != "" {
				messages = append(messages, map[string]string{"ts": "101.000", "user": "UBOT", "text": posted})
			}
			require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"ok": true, "messages": messages}))
		case "/chat.postMessage":
			posts++
			posted = request.Form.Get("text")
			require.Contains(t, request.Form.Get("blocks"), `"type":"markdown"`)
			require.Contains(t, posted, "[Grafana](https://grafana.example/explore?orgId=1&from=123)")
			require.NotContains(t, posted, "<!here>")
			_, _ = writer.Write([]byte(`{"ok":true,"channel":"CTEST","ts":"101.000"}`))
		case "/files.getUploadURLExternal":
			if failUpload {
				writer.WriteHeader(http.StatusBadGateway)
				return
			}
			require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"ok": true, "file_id": "FPANEL", "upload_url": server.URL + "/upload"}))
		case "/upload":
			uploads++
			_, _ = writer.Write([]byte("ok"))
		case "/files.completeUploadExternal":
			require.Equal(t, "CTEST", request.Form.Get("channel_id"))
			require.Equal(t, "100.000", request.Form.Get("thread_ts"))
			_, _ = writer.Write([]byte(`{"ok":true,"files":[{"id":"FPANEL","title":"Grafana panel"}]}`))
		default:
			t.Errorf("unexpected Slack method %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	consumer := NewConsumer(pool, nil, "", "", "", nil, nil).
		WithAPI(slackgo.New("token", slackgo.OptionAPIURL(server.URL+"/"))).WithAsk(router)
	result := ask.TurnResult{
		Reply: "## Findings\n[Grafana](https://grafana.example/explore?orgId=1&from=123) <!here>", AssistantTurn: 7,
		Artifacts: []ask.ArtifactDescriptor{{ID: uuid.New(), Title: "Grafana panel"}},
	}
	_, err := consumer.DeliverAskAnswer(t.Context(), "CTEST", "100.000", sessionID, result, "Delivery reference: test")
	require.Error(t, err)
	require.False(t, router.delivered)
	failUpload = false
	for attempt := 0; attempt < 2; attempt++ {
		messageTS, err := consumer.DeliverAskAnswer(t.Context(), "CTEST", "100.000", sessionID, result, "Delivery reference: test")
		require.NoError(t, err)
		require.Equal(t, "101.000", messageTS)
	}
	require.Equal(t, 1, posts)
	require.Equal(t, 1, uploads)
	require.True(t, router.delivered)
}

func TestDeliverAskAnswerRequiresConfiguredSlack(t *testing.T) {
	_, err := (&Consumer{}).DeliverAskAnswer(t.Context(), "CTEST", "100.000", uuid.New(), ask.TurnResult{Reply: "answer"}, "marker")
	require.ErrorContains(t, err, "slack api not configured")
}

// TestDeliverTrustedReplyIdempotentWhenBodyContainsMarkerSubstring is a
// deterministic regression test for a duplicate-post bug in
// deliveredAnswerChunks (answer_delivery.go).
//
// Trigger: DeliverTrustedReply posts text that itself contains the literal
// sequence "\n\n"+marker BEFORE the real trailing marker that
// DeliverTrustedReply always appends (`text+"\n\n"+marker`), then the
// caller retries with the same marker.
//
// Contract (DeliverTrustedReply's doc comment): "posts application-composed
// mrkdwn into a thread at most once per marker". deliveredAnswerChunks used
// to find the marker with strings.Cut(message.Text, "\n\n"+marker), which
// matches the FIRST occurrence anywhere in the text, not the message's own
// trailing one. When the body echoes marker-shaped text, the embedded match
// wins, the entry is stored under a key the retry's `delivered[marker]`
// lookup never finds, and the retry posts a second time.
//
// Expected: posting twice with the identical text and marker produces
// exactly one Slack message (the retry recognizes its own earlier post).
func TestDeliverTrustedReplyIdempotentWhenBodyContainsMarkerSubstring(t *testing.T) {
	const marker = "MK"
	// text itself contains "\n\n"+marker before the real trailing marker
	// DeliverTrustedReply appends when it posts.
	const text = "see reference \n\n" + marker + " for background, and act on it"

	posts := 0
	var lastPostedText string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"UBOT"}`))
		case "/conversations.replies":
			messages := []map[string]string{{"ts": "100.000", "text": "root"}}
			if posts > 0 {
				messages = append(messages, map[string]string{"ts": "101.000", "user": "UBOT", "text": lastPostedText})
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": messages}))
		case "/chat.postMessage":
			require.NoError(t, r.ParseForm())
			posts++
			lastPostedText = r.Form.Get("text")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "CTEST", "ts": fmt.Sprintf("10%d.000", posts)}))
		default:
			t.Fatalf("unexpected Slack method %s", r.URL.Path)
		}
	}))
	defer server.Close()

	consumer := NewConsumer(nil, nil, "", "", "", nil, nil).
		WithAPI(slackgo.New("token", slackgo.OptionAPIURL(server.URL+"/")))

	_, err := consumer.DeliverTrustedReply(t.Context(), "CTEST", "100.000", text, marker)
	require.NoError(t, err)
	require.Equal(t, 1, posts, "first delivery should post exactly once")

	_, err = consumer.DeliverTrustedReply(t.Context(), "CTEST", "100.000", text, marker)
	require.NoError(t, err)
	require.Equal(t, 1, posts, "retry with the identical marker must not post a second message")
}
