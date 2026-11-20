package pagerduty

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/diderot/internal/ask"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/google/uuid"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
)

func TestSlackIncidentMatchingRequiresExactIncidentLinkOnBotRoot(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		matches bool
	}{
		{"link", `{"ts":"123.001","bot_id":"BPD","text":"<https://acme.pagerduty.com/incidents/P123|incident>"}`, true},
		{"attachment", `{"ts":"123.001","bot_id":"BPD","attachments":[{"title_link":"https://acme.pagerduty.com/incidents/P123"}]}`, true},
		{"self thread timestamp", `{"ts":"123.001","thread_ts":"123.001","subtype":"bot_message","text":"https://acme.pagerduty.com/incidents/P123"}`, true},
		{"human paste", `{"ts":"123.001","user":"UHUMAN","text":"https://acme.pagerduty.com/incidents/P123"}`, false},
		{"reply", `{"ts":"123.002","thread_ts":"123.001","bot_id":"BPD","text":"https://acme.pagerduty.com/incidents/P123"}`, false},
		{"broadcast", `{"ts":"123.001","subtype":"thread_broadcast","bot_id":"BPD","text":"https://acme.pagerduty.com/incidents/P123"}`, false},
		{"wrong incident", `{"ts":"123.001","bot_id":"BPD","text":"https://acme.pagerduty.com/incidents/P1234"}`, false},
		{"path suffix", `{"ts":"123.001","bot_id":"BPD","text":"https://acme.pagerduty.com/incidents/P123-evil"}`, false},
		{"host suffix", `{"ts":"123.001","bot_id":"BPD","text":"https://pagerduty.com.evil.example/incidents/P123"}`, false},
		{"title only", `{"ts":"123.001","bot_id":"BPD","text":"P123 api errors"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var message slack.Message
			require.NoError(t, json.Unmarshal([]byte(test.message), &message))
			require.Equal(t, test.matches, matchesIncident(message, testEvent()))
		})
	}
}

type slackFixture struct {
	mu               sync.Mutex
	rootVisible      bool
	posted           string
	posts            int
	histories        int
	failPost         bool
	external         bool
	ambiguous        bool
	badPagination    bool
	paginate         bool
	missingReplyRoot bool
	blocks           string
	postedMessages   []slack.Message
	failAtPost       int
	trusted          []string
}

func (fixture *slackFixture) serve(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	require.NoError(t, request.ParseForm())
	writer.Header().Set("Content-Type", "application/json")
	root := map[string]any{"ts": "1792006201.000001", "bot_id": "BPD", "text": "<https://acme.pagerduty.com/incidents/P123|API errors>"}
	switch request.URL.Path {
	case "/slack/conversations.info":
		require.Equal(t, "CBOTALERTS", request.Form.Get("channel"))
		require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"ok": true, "channel": map[string]any{
			"id": "CBOTALERTS", "name": "ops-alerts", "is_member": true, "is_ext_shared": fixture.external,
		}}))
	case "/slack/auth.test":
		_, _ = writer.Write([]byte(`{"ok":true,"user_id":"UDIDEROT"}`))
	case "/slack/conversations.history":
		fixture.histories++
		require.Equal(t, "CBOTALERTS", request.Form.Get("channel"))
		if fixture.badPagination {
			_, _ = writer.Write([]byte(`{"ok":true,"messages":[],"has_more":true}`))
			return
		}
		if fixture.paginate && request.Form.Get("cursor") == "" {
			_, _ = writer.Write([]byte(`{"ok":true,"messages":[],"has_more":true,"response_metadata":{"next_cursor":"older"}}`))
			return
		}
		messages := []any{}
		if fixture.rootVisible {
			messages = append(messages, root)
		}
		if fixture.ambiguous {
			messages = append(messages, map[string]any{"ts": "1792006202.000001", "bot_id": "BPD", "text": root["text"]})
		}
		require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"ok": true, "messages": messages, "has_more": false}))
	case "/slack/conversations.replies":
		require.Equal(t, "1792006201.000001", request.Form.Get("ts"))
		messages := []any{root}
		if fixture.missingReplyRoot {
			messages = []any{}
		}
		for _, message := range fixture.postedMessages {
			messages = append(messages, message)
		}
		require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"ok": true, "messages": messages, "has_more": false}))
	case "/slack/chat.postMessage":
		require.Equal(t, "CBOTALERTS", request.Form.Get("channel"))
		require.Equal(t, "1792006201.000001", request.Form.Get("thread_ts"))
		require.NotEqual(t, "true", request.Form.Get("reply_broadcast"))
		fixture.posts++
		if strings.Contains(request.Form.Get("text"), "Related-work reference:") {
			require.NotEqual(t, "false", request.Form.Get("mrkdwn"))
			require.Empty(t, request.Form.Get("blocks"))
			fixture.trusted = append(fixture.trusted, request.Form.Get("text"))
		} else {
			require.Equal(t, "false", request.Form.Get("mrkdwn"))
			fixture.posted = request.Form.Get("text")
			fixture.blocks = request.Form.Get("blocks")
		}
		messageTS := fmt.Sprintf("1792006300.%06d", fixture.posts)
		fixture.postedMessages = append(fixture.postedMessages, slack.Message{Msg: slack.Msg{Timestamp: messageTS, User: "UDIDEROT", Text: request.Form.Get("text")}})
		if fixture.failPost || fixture.posts == fixture.failAtPost {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"ok": true, "channel": "CBOTALERTS", "ts": messageTS}))
	default:
		t.Errorf("unexpected Slack request %s", request.URL.Path)
		writer.WriteHeader(404)
	}
}

func newSlackFixture(t *testing.T, store *Store, fixture *slackFixture) *SlackDelivery {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.serve(t, writer, request)
	}))
	t.Cleanup(server.Close)
	api := slack.New("bot-token", slack.OptionAPIURL(server.URL+"/slack/"))
	consumer := slackcx.NewConsumer(store.pool, nil, "", "", "", nil, nil).WithAPI(api)
	return &SlackDelivery{API: api, Answers: consumer, Artifacts: &fakeArtifactLoader{},
		Store: store, ChannelID: "CBOTALERTS", ChannelName: "ops-alerts"}
}

type fakeArtifactLoader struct {
	artifacts []ask.ArtifactDescriptor
	turn      int
	err       error
}

func (loader *fakeArtifactLoader) LoadResultArtifacts(_ context.Context, _ uuid.UUID, turn int) ([]ask.ArtifactDescriptor, error) {
	loader.turn = turn
	return loader.artifacts, loader.err
}

func newSlackJob(t *testing.T, store *Store) *Job {
	t.Helper()
	require.NoError(t, store.Enqueue(t.Context(), testEvent()))
	job, err := store.Claim(t.Context())
	require.NoError(t, err)
	var sessionID uuid.UUID
	require.NoError(t, store.pool.QueryRow(t.Context(), `INSERT INTO ask_sessions DEFAULT VALUES RETURNING id`).Scan(&sessionID))
	require.NoError(t, store.SaveSession(t.Context(), job, sessionID))
	job.SessionID = &sessionID
	return job
}

func TestSlackDeliveryReconcilesAmbiguousSuccessAndConnectsSession(t *testing.T) {
	store, pool := testStore(t)
	job := newSlackJob(t, store)
	fixture := &slackFixture{rootVisible: true, paginate: true, failPost: true}
	delivery := newSlackFixture(t, store, fixture)
	_, err := delivery.Deliver(t.Context(), job, "Hypothesis; not proven. <!here> <@USER>")
	require.Error(t, err)
	fixture.mu.Lock()
	require.Equal(t, 1, fixture.posts)
	require.Equal(t, 2, fixture.histories)
	require.NotContains(t, fixture.posted, "<!here>")
	require.Contains(t, fixture.blocks, `"type":"markdown"`)
	fixture.failPost = false
	fixture.mu.Unlock()
	messageTS, err := delivery.Deliver(t.Context(), job, "saved report")
	require.NoError(t, err)
	require.Equal(t, "1792006300.000001", messageTS)
	fixture.mu.Lock()
	require.Equal(t, 1, fixture.posts)
	require.Equal(t, 2, fixture.histories)
	fixture.mu.Unlock()
	var attached uuid.UUID
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT session_id FROM ask_slack_sessions WHERE channel_id=$1 AND thread_ts=$2`, delivery.ChannelID, job.SlackThreadTS).Scan(&attached))
	require.Equal(t, *job.SessionID, attached)
}

// TestSlackDeliveryWithoutChannelName: the channel name is optional; without
// it, delivery still refuses an external channel.
func TestSlackDeliveryWithoutChannelName(t *testing.T) {
	for _, external := range []bool{false, true} {
		store, _ := testStore(t)
		delivery := newSlackFixture(t, store, &slackFixture{rootVisible: true, external: external})
		delivery.ChannelName = ""
		_, err := delivery.Deliver(t.Context(), newSlackJob(t, store), "report")
		if external {
			require.ErrorContains(t, err, "internal #ops-alerts")
		} else {
			require.NoError(t, err)
		}
	}
}

func TestSlackDeliveryFailsClosedOnMissingAmbiguousOrUnsafeDestination(t *testing.T) {
	for _, test := range []struct {
		name        string
		fixture     *slackFixture
		channelName string
		errorText   string
	}{
		{name: "late root", fixture: &slackFixture{}, errorText: "not yet found"},
		{name: "multiple roots", fixture: &slackFixture{rootVisible: true, ambiguous: true}, errorText: "ambiguous"},
		{name: "external channel", fixture: &slackFixture{rootVisible: true, external: true}, errorText: "internal #ops-alerts"},
		{name: "unexpected channel name", fixture: &slackFixture{rootVisible: true}, channelName: "bot-alerts", errorText: "internal #bot-alerts"},
		{name: "partial history", fixture: &slackFixture{badPagination: true}, errorText: "pagination"},
		{name: "missing reply root", fixture: &slackFixture{rootVisible: true, missingReplyRoot: true}, errorText: "missing alert root"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _ := testStore(t)
			job := newSlackJob(t, store)
			delivery := newSlackFixture(t, store, test.fixture)
			if test.channelName != "" {
				delivery.ChannelName = test.channelName
			}
			_, err := delivery.Deliver(t.Context(), job, "report")
			require.ErrorContains(t, err, test.errorText)
			test.fixture.mu.Lock()
			require.Zero(t, test.fixture.posts)
			test.fixture.mu.Unlock()
		})
	}
}

func TestSlackDeliveryChunksMarkdownAndResumesAfterAmbiguousPartialDelivery(t *testing.T) {
	store, pool := testStore(t)
	job := newSlackJob(t, store)
	job.AssistantTurn = 7
	fixture := &slackFixture{rootVisible: true, failAtPost: 2}
	delivery := newSlackFixture(t, store, fixture)
	loader := delivery.Artifacts.(*fakeArtifactLoader)
	link := "[Grafana evidence](https://grafana.example/explore?orgId=1&from=1792000000000)"
	report := "## Findings\n" + link + "\n" + strings.Repeat("telemetry evidence 💥\n", 700) + "\n[Full transcript](https://diderot.example/admin/ask/sessions/123)"
	_, err := delivery.Deliver(t.Context(), job, report)
	require.Error(t, err)
	require.Equal(t, 2, fixture.posts)
	require.Equal(t, 7, loader.turn)
	messageTS, err := delivery.Deliver(t.Context(), job, report)
	require.NoError(t, err)
	require.Equal(t, "1792006300.000001", messageTS)
	require.Greater(t, fixture.posts, 2)
	var text strings.Builder
	for _, message := range fixture.postedMessages {
		require.LessOrEqual(t, len(message.Text), 3500)
		text.WriteString(message.Text)
	}
	require.Contains(t, text.String(), link)
	require.Contains(t, text.String(), "[Full transcript](https://diderot.example/admin/ask/sessions/123)")
	require.NotContains(t, text.String(), "Report shortened")
	posts := fixture.posts
	_, err = delivery.Deliver(t.Context(), job, report)
	require.NoError(t, err)
	require.Equal(t, posts, fixture.posts)
	var registered int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM ask_answer_messages WHERE session_id=$1 AND turn=7`, *job.SessionID).Scan(&registered))
	require.Equal(t, fixture.posts, registered)
}

func TestSlackDeliveryDoesNotDropSelectedArtifacts(t *testing.T) {
	store, _ := testStore(t)
	job := newSlackJob(t, store)
	fixture := &slackFixture{rootVisible: true}
	delivery := newSlackFixture(t, store, fixture)
	loader := delivery.Artifacts.(*fakeArtifactLoader)
	loader.artifacts = []ask.ArtifactDescriptor{{ID: uuid.New(), Title: "Grafana evidence"}}
	_, err := delivery.Deliver(t.Context(), job, "report")
	require.ErrorContains(t, err, "ask artifact delivery is unavailable")
	require.Equal(t, 1, fixture.posts)
	loader.artifacts = nil
	_, err = delivery.Deliver(t.Context(), job, "report")
	require.NoError(t, err)
	require.Equal(t, 1, fixture.posts)
}
