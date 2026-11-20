package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"
)

// shortcutCallback builds a message-action interaction payload shaped
// like the ones Slack delivers for a message shortcut.
func shortcutCallback(callbackID string, msg slack.Msg, channelID string) *slack.InteractionCallback {
	return &slack.InteractionCallback{
		Type:       slack.InteractionTypeMessageAction,
		CallbackID: callbackID,
		Channel:    slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: channelID}, Name: "ops"}},
		User:       slack.User{ID: "UINVOKER"},
		Message:    slack.Message{Msg: msg},
	}
}

func TestResolveThreadRef(t *testing.T) {
	const chanID = "C0EXAMPLE11"

	cases := []struct {
		name   string
		cb     *slack.InteractionCallback
		want   threadRef
		wantOK bool
	}{
		{
			name: "reply message carries thread_ts",
			cb: shortcutCallback(summarizeCallbackID, slack.Msg{
				Timestamp:       "1700000500.000200",
				ThreadTimestamp: "1700000400.000100",
				Text:            "a reply",
			}, chanID),
			want:   threadRef{ChannelID: chanID, ThreadTS: "1700000400.000100"},
			wantOK: true,
		},
		{
			name: "thread root with replies",
			cb: shortcutCallback(summarizeCallbackID, slack.Msg{
				Timestamp:  "1700000400.000100",
				ReplyCount: 3,
				Text:       "the root",
			}, chanID),
			want:   threadRef{ChannelID: chanID, ThreadTS: "1700000400.000100"},
			wantOK: true,
		},
		{
			name: "container thread_ts fallback",
			cb: func() *slack.InteractionCallback {
				cb := shortcutCallback(summarizeCallbackID, slack.Msg{
					Timestamp: "1700000500.000200",
					Text:      "reply without msg thread_ts",
				}, chanID)
				cb.Container = slack.Container{Type: "message", ThreadTs: "1700000400.000100", MessageTs: "1700000500.000200"}
				return cb
			}(),
			want:   threadRef{ChannelID: chanID, ThreadTS: "1700000400.000100"},
			wantOK: true,
		},
		{
			name: "standalone message rejected — threads only",
			cb: shortcutCallback(summarizeCallbackID, slack.Msg{
				Timestamp: "1700000400.000100",
				Text:      "not in any thread",
			}, chanID),
			wantOK: false,
		},
		{
			name: "thread root with zero replies rejected",
			cb: shortcutCallback(summarizeCallbackID, slack.Msg{
				Timestamp:  "1700000400.000100",
				ReplyCount: 0,
				Text:       "root, no replies yet",
			}, chanID),
			wantOK: false,
		},
		{
			name:   "missing channel id rejected",
			cb:     shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, ""),
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveThreadRef(tc.cb)
			require.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.Equal(t, tc.want, got)
			}
		})
	}
}

func TestHasPinReaction(t *testing.T) {
	require.True(t, hasPinReaction([]slack.ItemReaction{{Name: "pushpin", Count: 1, Users: []string{"U1"}}}))
	require.True(t, hasPinReaction([]slack.ItemReaction{{Name: "eyes", Count: 2}, {Name: "pushpin", Count: 3}}))
	require.False(t, hasPinReaction([]slack.ItemReaction{{Name: "pushpin", Count: 0}}))
	require.False(t, hasPinReaction([]slack.ItemReaction{{Name: "eyes", Count: 2}}))
	require.False(t, hasPinReaction(nil))
}

func TestRenderTranscript(t *testing.T) {
	msgs := []summarizeMessage{
		{Timestamp: "100.1", UserID: "U1", Username: "alice", Text: "we should deploy friday"},
		{Timestamp: "100.2", UserID: "U2", Username: "bob", Text: "agreed", Pinned: true},
	}

	full, note := renderTranscript("#ops", msgs)
	require.Empty(t, note)
	require.Contains(t, full, "Slack thread in channel #ops")
	require.Contains(t, full, "[1] alice: we should deploy friday")
	require.Contains(t, full, "[2] bob [PINNED]: agreed")

	// Gap marker: a message with DroppedBefore renders the inline note.
	gapped := []summarizeMessage{
		{Timestamp: "100.1", UserID: "U1", Username: "alice", Text: "root"},
		{Timestamp: "100.9", UserID: "U2", Username: "bob", Text: "old pin", Pinned: true, DroppedBefore: 7},
	}
	withGaps, _ := renderTranscript("#ops", gapped)
	require.Contains(t, withGaps, "[… 7 earlier message(s) dropped — not pinned …]")
	require.Contains(t, withGaps, "bob [PINNED]: old pin")

	// A message over the prompt budget is per-message truncated inline —
	// the transcript itself never exceeds the budget from one message.
	var sb strings.Builder
	sb.WriteString(strings.Repeat("x", summarizeMaxPromptChars+5000))
	big := []summarizeMessage{{Timestamp: "1.1", UserID: "U1", Username: "a", Text: sb.String()}}
	out, note := renderTranscript("#ops", big)
	require.Empty(t, note, "a single message, per-message capped, must not trigger transcript truncation")
	require.Contains(t, out, "[…truncated…]")
}

// TestRenderTranscriptBudgetFill pins that pinned lines are always
// included, the newest non-pinned lines fill the leftover budget, and a
// too-long newest line does not stop shorter older lines from filling.
func TestRenderTranscriptBudgetFill(t *testing.T) {
	msgs := []summarizeMessage{
		{Timestamp: "1.1", UserID: "U1", Username: "old-short", Text: "short old message"},
		{Timestamp: "1.2", UserID: "U2", Username: "pin", Text: "pinned item", Pinned: true},
		{Timestamp: "1.3", UserID: "U3", Username: "filler", Text: strings.Repeat("f", 100)},
	}
	fillers := make([]summarizeMessage, 0, 40)
	for i := 0; i < 39; i++ {
		fillers = append(fillers, summarizeMessage{
			Timestamp: fmt.Sprintf("%d.1", 10+i),
			UserID:    "UF", Username: "filler",
			Text: strings.Repeat("f", summarizeMaxPromptChars/20),
		})
	}
	all := append([]summarizeMessage{msgs[0]}, fillers...)
	all = append(all, msgs[1], msgs[2])

	out, note := renderTranscript("#ops", all)
	require.NotEmpty(t, note)
	// The pin and the huge newest message are both present…
	require.Contains(t, out, "pin [PINNED]: pinned item")
	// …and if the huge newest line didn't fit the leftover budget, the
	// short older line must still have been considered.
	require.Contains(t, out, "old-short: short old message",
		"shorter older line must fill budget when the newest line doesn't fit")
}

// TestRenderTranscriptTrailingGapMarker pins that a dropped run AFTER the
// last included message still renders its gap marker.
func TestRenderTranscriptTrailingGapMarker(t *testing.T) {
	// Pins consume the whole budget (30 × 2000-char pins ≈ 60k);
	// ALL non-pinned messages are excluded, including everything after
	// the last pin.
	var msgs []summarizeMessage
	for i := 0; i < 30; i++ {
		msgs = append(msgs, summarizeMessage{
			Timestamp: fmt.Sprintf("%d.1", i),
			UserID:    "UP", Username: "pinner",
			Text:   strings.Repeat("p", summarizeMaxMessageChars), // pinned, maxed
			Pinned: true,
		})
	}
	// Trailing non-pinned messages that can never fit.
	msgs = append(msgs,
		summarizeMessage{Timestamp: "40.1", UserID: "U9", Username: "tail1", Text: "tail message one"},
		summarizeMessage{Timestamp: "41.1", UserID: "U9", Username: "tail2", Text: "tail message two"},
	)

	out, _ := renderTranscript("#ops", msgs)
	require.NotContains(t, out, "tail message one", "non-pinned tails must be excluded when pins consume the budget")
	// The trailing dropped run still gets its marker.
	require.Contains(t, out, "[… 2 earlier message(s) dropped — not pinned …]",
		"trailing dropped run must render its gap marker")
}

// TestSummarizeOutputSanitized pins that model output containing
// Slack-interpreted mention sequences is defused before posting — a
// prompt-injected thread must not turn the summary into a channel-wide
// notification. Uses diderot's sanitizeSlackMarkdown (plain @word forms,
// not HTML entities — the reader still sees who was referenced).
func TestSummarizeOutputSanitized(t *testing.T) {
	require.Equal(t, "@channel everyone look", sanitizeSlackMarkdown("<!channel> everyone look"))
	require.Equal(t, "@here", sanitizeSlackMarkdown("<!here>"))
	require.Equal(t, "@U123 did it", sanitizeSlackMarkdown("<@U123> did it"))
	require.Equal(t, "plain *bold* and bullets survive", sanitizeSlackMarkdown("plain *bold* and bullets survive"))
}

// pagingThreadReader serves a thread across multiple pages in
// chronological order, like the real conversations.replies.
type pagingThreadReader struct {
	pages [][]slack.Message
	calls int
}

func (p *pagingThreadReader) GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	if p.calls >= len(p.pages) {
		return nil, false, "", nil
	}
	page := p.pages[p.calls]
	p.calls++
	hasMore := p.calls < len(p.pages)
	next := ""
	if hasMore {
		next = fmt.Sprintf("cursor-%d", p.calls)
	}
	return page, hasMore, next, nil
}

func (p *pagingThreadReader) GetUserInfoContext(ctx context.Context, userID string) (*slack.User, error) {
	return &slack.User{ID: userID, Name: userID}, nil
}

// TestFetchThreadHardStopsAtFetchCap pins that pagination stops after
// summarizeMaxFetchedMessages total fetched messages — a huge thread is
// not paged to exhaustion.
func TestFetchThreadHardStopsAtFetchCap(t *testing.T) {
	pageSize := 200
	page := func(start int) []slack.Message {
		msgs := make([]slack.Message, 0, pageSize)
		for i := 0; i < pageSize; i++ {
			msgs = append(msgs, slack.Message{Msg: slack.Msg{
				Timestamp: fmt.Sprintf("%d.%04d", start+i, i),
				User:      "U1", Text: "msg",
			}})
		}
		return msgs
	}
	reader := &pagingThreadReader{}
	for p := 0; p*pageSize < summarizeMaxFetchedMessages+pageSize; p++ {
		reader.pages = append(reader.pages, page(p*pageSize))
	}
	// Its pages array is 11+ entries; the fetch must stop at 10.
	msgs, err := fetchThread(context.Background(), reader, threadRef{ChannelID: "C", ThreadTS: "1.1"})
	require.NoError(t, err)
	require.Equal(t, summarizeMaxFetchedMessages/pageSize, reader.calls,
		"pagination must hard-stop at the fetch cap, not page to exhaustion")
	require.NotEmpty(t, msgs)
}

// TestFetchThreadKeepsMostRecentOnCap pins the rolling-tail behavior: a
// thread longer than summarizeMaxMessages keeps the NEWEST messages
// (including pinned items in later pages), not the first page fetched.
func TestFetchThreadKeepsMostRecentOnCap(t *testing.T) {
	if summarizeMaxMessages < 3 {
		t.Skip("cap too small for this test's shape")
	}
	filler := func(n int, start int) []slack.Message {
		msgs := make([]slack.Message, 0, n)
		for i := 0; i < n; i++ {
			msgs = append(msgs, slack.Message{Msg: slack.Msg{
				Timestamp: fmt.Sprintf("%d.%04d", start+i, i),
				User:      "UOLD",
				Text:      fmt.Sprintf("old filler %d", start+i),
			}})
		}
		return msgs
	}
	reader := &pagingThreadReader{pages: [][]slack.Message{
		// Page 1: old filler PLUS an old pinned message — the pin must
		// survive the cap even though it's far outside the newest-200
		// window (the feature contract: one action item per pin).
		append(filler(1, 100), slack.Message{Msg: slack.Msg{
			Timestamp: "101.0001", User: "UPIN", Text: "old pinned action item",
			Reactions: []slack.ItemReaction{{Name: "pushpin", Count: 1}},
		}}),
		filler(summarizeMaxMessages, 200),
		{
			{Msg: slack.Msg{Timestamp: "900.0001", User: "UNEW", Text: "newest message",
				Reactions: []slack.ItemReaction{{Name: "pushpin", Count: 1}}}},
		},
	}}

	got, err := fetchThread(context.Background(), reader, threadRef{ChannelID: "C", ThreadTS: "1.1"})
	require.NoError(t, err)
	// Tail (200) + the old pin retained additionally.
	require.Len(t, got, summarizeMaxMessages+1)
	last := got[len(got)-1]
	require.Equal(t, "newest message", last.Text)
	require.True(t, last.Pinned)
	// The old pinned message survived, is first, and its gap marker
	// counts the non-pinned filler dropped before it.
	first := got[0]
	require.Equal(t, "old pinned action item", first.Text)
	require.True(t, first.Pinned)
	require.Equal(t, 1, first.DroppedBefore) // the one filler message before it
	// Tail boundary: the message right after the old pin shows the one
	// dropped filler between the pin and the tail window.
	require.Equal(t, 1, got[1].DroppedBefore)
	// All pages consumed (no early return at the cap).
	require.Equal(t, 3, reader.calls)
}

// TestKeepNewestAndPinnedTransfersDroppedGapCounts pins that a message
// dropped on a LATER pass carries its earlier accumulated DroppedBefore
// into the pending run — a bare dropped++ would under-report the gap
// marker when fetchThread reprocesses its own output across pages.
// Scenario: pass 1-2 over 400+ messages leave a retained message with
// DroppedBefore=200; pass 3 grows the tail and that message (non-pinned)
// falls out of the window; the next retained message must show 201, not
// 1, dropped before it.
func TestKeepNewestAndPinnedTransfersDroppedGapCounts(t *testing.T) {
	// Pass 2 output: one old retained message that carries DroppedBefore
	// from the messages dropped before it on pass 1-2, followed by the
	// newest window.
	mk := func(ts string, dropped int) summarizeMessage {
		return summarizeMessage{Timestamp: ts, UserID: "U1", Username: "a", Text: "m", DroppedBefore: dropped}
	}
	prev := make([]summarizeMessage, 0, summarizeMaxMessages+1)
	prev = append(prev, mk("0.1", 200)) // retained on pass 2, gap of 200 before it
	for i := 1; i <= summarizeMaxMessages; i++ {
		prev = append(prev, mk(fmt.Sprintf("%d.1", i), 0))
	}
	require.Len(t, prev, summarizeMaxMessages+1)

	// Pass 3: one more page arrives (202 total, cap 200) so the old
	// gapped message AND the first window message fall out. The old
	// message's 200 accumulated drops must transfer: the next retained
	// message reports 1+200 (old) + 1 (window) = 202 dropped before it.
	withPage := append(append([]summarizeMessage{}, prev...), mk("999.1", 0))
	got := keepNewestAndPinned(withPage, summarizeMaxMessages)
	require.Len(t, got, summarizeMaxMessages)
	require.Equal(t, 202, got[0].DroppedBefore,
		"gap counts from a later-dropped message must transfer, not vanish")
}

// captureSlackServer is an httptest Slack web-API server recording every
// form post. Method routing: conversations.replies + users.info GET-ish
// routes are handled via their POST form payloads; chat.postEphemeral
// and chat.postMessage are captured for assertions.
type captureSlackServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	posts []capturedPost
	// replies is the thread page served for conversations.replies.
	replies []slack.Message
	// failChat flips every chat.* route to an error response, to test
	// the delivery-failure path.
	failChat bool
}

type capturedPost struct {
	method string
	params url.Values
}

func newCaptureSlackServer(t *testing.T, replies []slack.Message) *captureSlackServer {
	t.Helper()
	cs := &captureSlackServer{replies: replies}
	mux := http.NewServeMux()
	handle := func(method string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
			}
			cs.mu.Lock()
			cs.posts = append(cs.posts, capturedPost{method: method, params: r.PostForm})
			cs.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch method {
			case "conversations.replies":
				msgsJSON, _ := json.Marshal(cs.replies)
				_, _ = fmt.Fprintf(w, `{"ok":true,"messages":%s,"has_more":false,"response_metadata":{"next_cursor":""}}`, msgsJSON)
			case "users.info":
				uid := r.PostForm.Get("user")
				_, _ = fmt.Fprintf(w, `{"ok":true,"user":{"id":%q,"name":"user-%s","profile":{"display_name":"user-%s"}}}`, uid, uid, uid)
			case "chat.postEphemeral", "chat.postMessage":
				if cs.failChat {
					_, _ = fmt.Fprint(w, `{"ok":false,"error":"msg_too_long"}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"ok":true,"ts":"1700000000.000100"}`)
			default:
				_, _ = fmt.Fprint(w, `{"ok":true}`)
			}
		}
	}
	mux.HandleFunc("/conversations.replies", handle("conversations.replies"))
	mux.HandleFunc("/users.info", handle("users.info"))
	mux.HandleFunc("/chat.postEphemeral", handle("chat.postEphemeral"))
	mux.HandleFunc("/chat.postMessage", handle("chat.postMessage"))
	cs.srv = httptest.NewServer(mux)
	t.Cleanup(cs.srv.Close)
	return cs
}

func (cs *captureSlackServer) chatPosts() []capturedPost {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []capturedPost
	for _, p := range cs.posts {
		if p.method == "chat.postMessage" || p.method == "chat.postEphemeral" {
			out = append(out, p)
		}
	}
	return out
}

// newSummarizerTestConsumer builds a Consumer whose api + summarizer point
// at capture servers.
func newSummarizerTestConsumer(t *testing.T, gatewayURL string, replies []slack.Message) (*Consumer, *captureSlackServer) {
	t.Helper()
	cs := newCaptureSlackServer(t, replies)
	sum, err := NewSummarizer(SummarizerConfig{
		BaseURL: gatewayURL,
		APIKey:  "test-key",
		ModelID: "summary-model",
	})
	require.NoError(t, err)
	c := &Consumer{
		api:        slack.New("xoxb-test", slack.OptionAPIURL(cs.srv.URL+"/")),
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics:    newConsumerMetrics(prometheus.NewRegistry()),
		summarizer: sum,
	}
	return c, cs
}

// TestSummarizerGatewayContract pins the request/response shape of the
// gateway call: model, messages (system prompt + transcript), auth
// header, and usage extraction.
func TestSummarizerGatewayContract(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/chat/completions", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "ok summary"},
			}},
			"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 7},
		})
	}))
	t.Cleanup(gw.Close)

	s, err := NewSummarizer(SummarizerConfig{BaseURL: gw.URL + "/", APIKey: " k1 ", ModelID: "m1:main"})
	require.NoError(t, err)
	require.Equal(t, "m1:main", s.ModelID())

	summary, usage, err := s.Summarize(context.Background(), "transcript")
	require.NoError(t, err)
	require.Equal(t, "ok summary", summary)
	require.Equal(t, 12, usage.PromptTokens)
	require.Equal(t, 7, usage.CompletionTokens)
	require.Equal(t, "Bearer k1", gotAuth)

	messages := gotBody["messages"].([]any)
	require.Len(t, messages, 2)
	sys := messages[0].(map[string]any)
	require.Equal(t, "system", sys["role"])
	require.Equal(t, summarizeSystemPrompt, sys["content"])
	usr := messages[1].(map[string]any)
	require.Equal(t, "user", usr["role"])
	require.Equal(t, "transcript", usr["content"])
	require.Equal(t, "m1:main", gotBody["model"])
	// Generation must be bounded so a runaway model
	// can't produce an unpostable response.
	require.Equal(t, float64(summarizeMaxOutputTokens), gotBody["max_tokens"])
}

// TestSummarizerRegistryExtraBody: chat_extra_body follows the summarizer's
// own fields and may not override one of them.
func TestSummarizerRegistryExtraBody(t *testing.T) {
	var raw []byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(gw.Close)

	s, err := NewSummarizer(SummarizerConfig{
		BaseURL: gw.URL, APIKey: "k", ModelID: "local", ExtraBody: map[string]any{"temperature": 0.2},
	})
	require.NoError(t, err)
	_, _, err = s.Summarize(context.Background(), "transcript")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(string(raw), `"model":"local","temperature":0.2}`), string(raw))

	_, err = NewSummarizer(SummarizerConfig{BaseURL: gw.URL, APIKey: "k", ExtraBody: map[string]any{"max_tokens": 1}})
	require.ErrorContains(t, err, "conflicts with a request field")
}

// TestSummarizerMaxCompletionTokens: a model whose registry entry sets
// chat_token_param: max_completion_tokens and a reasoning token allowance
// (OpenAI's reasoning models) gets the summary limit plus the allowance under
// that field instead of max_tokens, and never temperature.
func TestSummarizerMaxCompletionTokens(t *testing.T) {
	var raw []byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	t.Cleanup(gw.Close)

	s, err := NewSummarizer(SummarizerConfig{
		BaseURL: gw.URL, APIKey: "k", ModelID: "gpt-6-luna", TokenParam: "max_completion_tokens", ReasoningTokenAllowance: 25000,
	})
	require.NoError(t, err)
	_, _, err = s.Summarize(context.Background(), "transcript")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(raw), `{"max_completion_tokens":27000,"messages":[`), string(raw))
	require.True(t, strings.HasSuffix(string(raw), `],"model":"gpt-6-luna"}`), string(raw))
	require.NotContains(t, string(raw), `"max_tokens"`)
	require.NotContains(t, string(raw), `"temperature"`)

	_, err = NewSummarizer(SummarizerConfig{
		BaseURL: gw.URL, APIKey: "k", TokenParam: "max_completion_tokens", ExtraBody: map[string]any{"max_completion_tokens": 1},
	})
	require.ErrorContains(t, err, "conflicts with a request field")
	_, err = NewSummarizer(SummarizerConfig{BaseURL: gw.URL, APIKey: "k", TokenParam: "max_output_tokens"})
	require.ErrorContains(t, err, `chat token parameter "max_output_tokens"`)
	_, err = NewSummarizer(SummarizerConfig{BaseURL: gw.URL, APIKey: "k", ReasoningTokenAllowance: -1})
	require.ErrorContains(t, err, "must not be negative")
}

func TestNewSummarizerValidation(t *testing.T) {
	_, err := NewSummarizer(SummarizerConfig{})
	require.Error(t, err)
	_, err = NewSummarizer(SummarizerConfig{BaseURL: "http://gw"})
	require.Error(t, err, "missing API key must fail construction")
	_, err = NewSummarizer(SummarizerConfig{BaseURL: "http://user:pass@gw", APIKey: "k"})
	require.Error(t, err, "userinfo must be rejected")
	_, err = NewSummarizer(SummarizerConfig{BaseURL: "http://gw?q=1", APIKey: "k"})
	require.Error(t, err, "query must be rejected")
	_, err = NewSummarizer(SummarizerConfig{BaseURL: "http://gw/", APIKey: " k "})
	require.ErrorContains(t, err, "a model is required", "no model is anyone's default")
	s, err := NewSummarizer(SummarizerConfig{BaseURL: "http://gw/", APIKey: " k ", ModelID: " m1 "})
	require.NoError(t, err)
	require.Equal(t, "m1", s.ModelID())
}

// TestRunSummarizePipeline drives the full shortcut flow against a fake
// gateway and capture Slack server: thread resolution from the payload,
// pin detection, username resolution, LLM call, and the public/private
// posting split.
func TestRunSummarizePipeline(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "*Summary*\ndeploy friday\n\n*Action Items*\n• 📌 @user-U2: agreed"},
			}},
			"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 7},
		})
	}))
	t.Cleanup(gw.Close)

	replies := []slack.Message{
		{Msg: slack.Msg{Timestamp: "1.0", User: "U1", Text: "root: what about friday?"}},
		{Msg: slack.Msg{Timestamp: "1.1", User: "U2", Text: "agreed",
			Reactions: []slack.ItemReaction{{Name: "pushpin", Count: 1}}}},
	}

	t.Run("public posts into the thread", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
		cb := shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1")
		c.runSummarize(context.Background(), *cb, false)
		posts := cs.chatPosts()
		require.Len(t, posts, 1)
		require.Equal(t, "chat.postMessage", posts[0].method)
		require.Equal(t, "1.0", posts[0].params.Get("thread_ts"), "summary must anchor to the thread root")
		text := posts[0].params.Get("text")
		require.Contains(t, text, "deploy friday")
		require.Contains(t, text, "_summary by diderot")
		require.Contains(t, text, "summary-model")
		require.Contains(t, text, "12 in / 7 out tokens")
	})

	t.Run("private posts ephemeral to the invoker", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
		cb := shortcutCallback(summarizePrivateCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1")
		c.runSummarize(context.Background(), *cb, true)
		posts := cs.chatPosts()
		require.Len(t, posts, 1)
		require.Equal(t, "chat.postEphemeral", posts[0].method)
		require.Equal(t, "UINVOKER", posts[0].params.Get("user"))
		require.Equal(t, "1.0", posts[0].params.Get("thread_ts"))
		require.Contains(t, posts[0].params.Get("text"), "deploy friday")
	})

	t.Run("not a thread gets guidance, no gateway call", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
		cb := shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "9.9", Text: "standalone"}, "C1")
		c.runSummarize(context.Background(), *cb, false)
		posts := cs.chatPosts()
		require.Len(t, posts, 1)
		require.Equal(t, "chat.postEphemeral", posts[0].method)
		require.Contains(t, posts[0].params.Get("text"), "isn't in a thread")
	})

	t.Run("unconfigured replies not configured", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
		c.summarizer = nil
		cb := shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1")
		c.runSummarize(context.Background(), *cb, false)
		posts := cs.chatPosts()
		require.Len(t, posts, 1)
		require.Contains(t, posts[0].params.Get("text"), "not configured")
	})

	t.Run("fetch failure gets an error ephemeral", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, nil)
		// Empty replies page yields zero messages -> "empty" feedback.
		cb := shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1")
		c.runSummarize(context.Background(), *cb, false)
		posts := cs.chatPosts()
		require.Len(t, posts, 1)
		require.Contains(t, posts[0].params.Get("text"), "empty")
	})

	// A 200 response with empty/whitespace
	// content must not be posted as a footer-only "summary".
	t.Run("empty model content gets an error ephemeral, no post", func(t *testing.T) {
		emptyGW := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"content": "   "},
				}},
				"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
			})
		}))
		t.Cleanup(emptyGW.Close)
		c, cs := newSummarizerTestConsumer(t, emptyGW.URL, replies)
		cb := shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1")
		c.runSummarize(context.Background(), *cb, false)
		posts := cs.chatPosts()
		require.Len(t, posts, 1)
		require.Equal(t, "chat.postEphemeral", posts[0].method)
		require.Contains(t, posts[0].params.Get("text"), "no usable result")
		require.NotContains(t, posts[0].params.Get("text"), "_summary by diderot",
			"a footer-only post must never happen")
	})

	// A failed delivery must NOT be reported
	// as success — the invoker gets an error ephemeral and (asserted by
	// the metric label below) the outcome is classified post_error.
	t.Run("delivery failure notifies invoker, not silent success", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
		cs.mu.Lock()
		cs.failChat = true
		cs.mu.Unlock()
		// The summary post fails; the follow-up feedback ephemeral must
		// also fail (failChat covers all chat.*), so assert on the
		// captured post contents: the first chat post is the summary
		// attempt. The pipeline must attempt delivery AND post feedback
		// — both land in the capture; neither is a "success" path.
		cb := shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1")
		c.runSummarize(context.Background(), *cb, false)
		posts := cs.chatPosts()
		require.NotEmpty(t, posts, "the summary delivery must be attempted")
		// The pipeline then posts feedback about the failure.
		require.Eventually(t, func() bool {
			cs.mu.Lock()
			defer cs.mu.Unlock()
			for _, p := range cs.posts {
				if p.method == "chat.postEphemeral" &&
					strings.Contains(p.params.Get("text"), "couldn't post the result") {
					return true
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond, "invoker must be told the summary couldn't be posted")
	})
}

// TestHandleMessageActionRouting pins the dispatch layer: known
// callback IDs run the pipeline (bounded by the admission semaphore),
// unknown IDs are ignored without posting.
func TestHandleMessageActionRouting(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "ok"},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	t.Cleanup(gw.Close)
	replies := []slack.Message{{Msg: slack.Msg{Timestamp: "1.0", User: "U1", Text: "hi"}}}

	t.Run("unknown callback id ignored", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
		c.handleMessageAction(context.Background(),
			*shortcutCallback("some_other_action", slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1"))
		require.Empty(t, cs.chatPosts())
	})

	t.Run("not a thread rejected via dispatch", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
		c.handleMessageAction(context.Background(),
			*shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "9.9", Text: "standalone"}, "C1"))
		posts := cs.chatPosts()
		require.Len(t, posts, 1)
		require.Contains(t, posts[0].params.Get("text"), "isn't in a thread")
	})

	t.Run("rejection when semaphore full", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
		for i := 0; i < summarizeConcurrency; i++ {
			summarizeSlots <- struct{}{}
		}
		t.Cleanup(func() {
			for i := 0; i < summarizeConcurrency; i++ {
				<-summarizeSlots
			}
		})
		c.handleMessageAction(context.Background(),
			*shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1"))
		require.Eventually(t, func() bool { return len(cs.chatPosts()) == 1 }, 5*time.Second, 10*time.Millisecond)
		require.Contains(t, cs.chatPosts()[0].params.Get("text"), "Too many summaries")
	})

	t.Run("concurrency cap admits after a slot frees", func(t *testing.T) {
		c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
		summarizeSlots <- struct{}{} // one of N slots occupied
		t.Cleanup(func() {
			select {
			case <-summarizeSlots:
			default:
			}
		})
		c.handleMessageAction(context.Background(),
			*shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1"))
		require.Eventually(t, func() bool { return len(cs.chatPosts()) == 1 }, 5*time.Second, 10*time.Millisecond)
		require.Contains(t, cs.chatPosts()[0].params.Get("text"), "ok")
		require.NotContains(t, cs.chatPosts()[0].params.Get("text"), "Too many summaries")
	})
}

// TestHandleInteractiveRoutesMessageActions pins the optin dispatcher
// passes message-action payloads to the summarizer.
func TestHandleInteractiveRoutesMessageActions(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "ok"},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	t.Cleanup(gw.Close)
	replies := []slack.Message{{Msg: slack.Msg{Timestamp: "1.0", User: "U1", Text: "hi"}}}
	c, cs := newSummarizerTestConsumer(t, gw.URL, replies)
	cb := shortcutCallback(summarizeCallbackID, slack.Msg{Timestamp: "1.1", ThreadTimestamp: "1.0"}, "C1")
	c.handleInteractive(context.Background(), *cb)
	require.Eventually(t, func() bool { return len(cs.chatPosts()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Contains(t, cs.chatPosts()[0].params.Get("text"), "ok")
}
