package adminui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAskPromptEnvelope(t *testing.T) {
	input := `preface
<context>
{"asker":{"display_name":"Carol","email":"carol@example.com","slack_user_id":"U123"},"time":{"timezone":"America/Los_Angeles"}}
</context>
<thread_context>
Alice: see the attached screenshot
</thread_context>
<question>
What happened **here**?
</question>
<exact_references>
<reference role="primary"><event_id>slack:C1:123</event_id></reference>
</exact_references>`
	sections, ok := parseAskPromptEnvelope(input)
	require.True(t, ok)
	require.Len(t, sections, 5)
	assert.Equal(t, []string{"additional", "context", "thread_context", "question", "exact_references"},
		[]string{sections[0].Kind, sections[1].Kind, sections[2].Kind, sections[3].Kind, sections[4].Kind})
	assert.Equal(t, "What happened **here**?", sections[3].Text)
	assert.Contains(t, sections[4].Text, "<event_id>slack:C1:123</event_id>")

	exchange := newAskQuestion(input, false)
	assert.Equal(t, "Carol", exchange.Asker)
	assert.Contains(t, string(exchange.Question), "<strong>here</strong>")
	assert.Contains(t, exchange.Context, "\n  \"asker\"", "the context reads as indented JSON")
	assert.Equal(t, "Alice: see the attached screenshot", exchange.Thread)
	assert.Equal(t, "preface", exchange.Other)

	_, ok = parseAskPromptEnvelope("<context>missing close")
	assert.False(t, ok)
	bare := newAskQuestion("<context>missing close", false)
	assert.Contains(t, string(bare.Question), "&lt;context&gt;missing close", "a malformed envelope shows whole")
}

func toolCall(id, name, arguments string) *schema.ContentBlock {
	return schema.NewContentBlock(&schema.FunctionToolCall{CallID: id, Name: name, Arguments: arguments})
}

func toolResult(id, name, text string) *schema.ContentBlock {
	return schema.NewContentBlock(&schema.FunctionToolResult{CallID: id, Name: name, Content: []*schema.FunctionToolResultContentBlock{{
		Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: text},
	}}})
}

func assistant(blocks ...*schema.ContentBlock) *schema.AgenticMessage {
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: blocks}
}

func TestBuildAskExchangesReadsAsAConversation(t *testing.T) {
	at := time.Date(2026, 11, 17, 18, 39, 0, 0, time.UTC)
	question := `<context>{"asker":{"display_name":"Ada"}}</context><question>What changed?</question>`
	messages := []ask.StoredMessage{
		{Turn: 0, CreatedAt: at, Message: ask.NewAgenticUserText(question)},
		{Turn: 1, CreatedAt: at, Message: assistant(
			schema.NewContentBlock(&schema.Reasoning{Text: "Search the **deploy** notes."}),
			schema.NewContentBlock(&schema.Reasoning{Signature: "encrypted"}),
			schema.NewContentBlock(&schema.AssistantGenText{Text: "Looking."}),
			toolCall("c1", "retrieve", `{"question":"rollout"}`),
			toolCall("c2", "sentry_search", `{"query":"timeouts"}`),
		)},
		{Turn: 2, CreatedAt: at, Message: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{
			toolResult("c1", "retrieve", `{"hits":[{"source":"slack"},{"source":"slack"}]}`),
			toolResult("c2", "sentry_search", `{"error":"sentry is down"}`),
		}}},
		{Turn: 3, CreatedAt: at.Add(24 * time.Second), Message: ask.NewAgenticAssistantText("The **listener** moved.")},
		{Turn: 4, CreatedAt: at, Message: ask.NewAgenticUserText("<question>And then?</question>")},
		{Turn: 5, CreatedAt: at, Message: assistant(&schema.ContentBlock{Type: "future_block"})},
		{Turn: 6, CreatedAt: at, Message: ask.NewAgenticAssistantText("Partial list"), IncompleteReason: ask.IncompleteMaxTokens},
	}
	timings := map[string]askToolTiming{"c1": {Took: 1200 * time.Millisecond}, "c2": {Took: time.Second, Failed: true}}
	runs := map[int]askRunView{0: {Took: 24 * time.Second, Outcome: "answered"}, 4: {Outcome: "error", Error: "ask service: model response incomplete: max_tokens"}}
	exchanges := buildAskExchanges(messages, false, timings, runs, false, false)
	require.Len(t, exchanges, 2)

	first := exchanges[0]
	assert.Equal(t, "turn-0", first.Anchor)
	assert.Equal(t, "Ada", first.Asker)
	assert.Equal(t, 24*time.Second, first.Took)
	require.Len(t, first.Steps, 4, "readable reasoning, the remark, and both calls; encrypted reasoning has nothing to show")
	assert.Equal(t, "reasoning", first.Steps[0].Kind)
	assert.Equal(t, "Search the deploy notes.", first.Steps[0].Asked)
	assert.Equal(t, "note", first.Steps[1].Kind)
	retrieve := first.Steps[2]
	assert.Equal(t, "rollout", retrieve.Asked)
	assert.Equal(t, "Found 2 relevant results", retrieve.Found)
	assert.Equal(t, 1200*time.Millisecond, retrieve.Took)
	assert.True(t, retrieve.Returned)
	assert.Contains(t, retrieve.Output, "\n  \"hits\"", "the result reads as indented JSON")
	assert.Equal(t, "turn-1-block-3", retrieve.Anchor)
	sentry := first.Steps[3]
	assert.Equal(t, "timeouts", sentry.Asked, "an unknown tool is described by its common arguments")
	assert.True(t, sentry.Failed)
	assert.Equal(t, "sentry is down", sentry.Error)
	require.Len(t, first.Answers, 1)
	assert.Contains(t, string(first.Answers[0].Body), "<strong>listener</strong>")
	assert.Empty(t, first.Answers[0].Stopped)
	assert.Nil(t, first.stopped())

	second := exchanges[1]
	require.Len(t, second.Steps, 1)
	assert.Equal(t, "unknown", second.Steps[0].Kind)
	assert.Contains(t, second.Steps[0].Raw, "future_block", "a block this page does not know is kept, raw")
	stop := second.stopped()
	require.NotNil(t, stop)
	assert.Equal(t, "Answer stopped at the model's output limit.", stop.Stopped, "the incomplete reason beats the raw run error")
	assert.Equal(t, "turn-6", stop.Anchor)
	assert.Contains(t, string(stop.Body), "Partial list")
}

func TestBuildAskExchangesKeepsEveryAnswer(t *testing.T) {
	at := time.Date(2026, 11, 17, 18, 39, 0, 0, time.UTC)
	exchanges := buildAskExchanges([]ask.StoredMessage{
		{Turn: 0, CreatedAt: at, Message: ask.NewAgenticUserText("<question>Why?</question>")},
		{Turn: 1, CreatedAt: at, Message: ask.NewAgenticAssistantText("diagnostic"), IncompleteReason: ask.IncompleteStopWithoutRespond},
		{Turn: 2, CreatedAt: at, Message: ask.NewAgenticAssistantText("first")},
		{Turn: 3, CreatedAt: at, Message: ask.NewAgenticAssistantText("second")},
	}, false, nil, nil, false, false)
	require.Len(t, exchanges, 1)
	require.Len(t, exchanges[0].Answers, 3, "a later ending never hides an earlier one")
	assert.NotEmpty(t, exchanges[0].Answers[0].Stopped)
	assert.Contains(t, string(exchanges[0].Answers[2].Body), "second")
	assert.Nil(t, exchanges[0].stopped())
}

func TestBuildAskExchangesEndings(t *testing.T) {
	at := time.Date(2026, 11, 17, 18, 39, 0, 0, time.UTC)
	asked := []ask.StoredMessage{
		{Turn: 0, CreatedAt: at, Message: ask.NewAgenticUserText("<question>Why?</question>")},
		{Turn: 1, CreatedAt: at, Message: assistant(toolCall("c1", "retrieve", `{"question":"why"}`))},
	}

	stopped := buildAskExchanges(asked, false, nil, map[int]askRunView{0: {Outcome: "error", Error: "model call: 429 Too Many Requests"}}, false, false)
	require.NotNil(t, stopped[0].stopped())
	assert.Equal(t, "model call: 429 Too Many Requests", stopped[0].stopped().Stopped)
	assert.Empty(t, stopped[0].stopped().Anchor, "the question's anchor leads there")
	assert.Empty(t, stopped[0].Steps[0].Found, "a call whose result never arrived has none")
	assert.False(t, stopped[0].Steps[0].Returned)

	byAsker := buildAskExchanges(asked, false, nil, map[int]askRunView{0: {Outcome: "stopped"}}, false, false)
	require.NotNil(t, byAsker[0].stopped())
	assert.Equal(t, "Stopped", byAsker[0].stopped().Chosen, "a question its asker stopped did not fail")
	assert.Equal(t, "The person who asked stopped this answer.", byAsker[0].stopped().Stopped)

	byCaller := buildAskExchanges(asked, false, nil, map[int]askRunView{0: {Outcome: "cancelled"}}, false, false)
	require.NotNil(t, byCaller[0].stopped())
	assert.Equal(t, "Cancelled", byCaller[0].stopped().Chosen, "a question its caller cancelled did not fail")
	assert.Equal(t, "The caller cancelled this question before it was answered.", byCaller[0].stopped().Stopped)

	unrecorded := buildAskExchanges(asked, false, nil, nil, false, false)
	require.NotNil(t, unrecorded[0].stopped())
	assert.Equal(t, "No answer was recorded.", unrecorded[0].stopped().Stopped)

	running := buildAskExchanges(asked, false, nil, nil, false, true)
	assert.True(t, running[0].Running)
	assert.Empty(t, running[0].Answers)

	failedBefore := map[int]askRunView{0: {Outcome: "error", Error: "model call: 429 Too Many Requests"}}
	retrying := buildAskExchanges(asked, false, nil, failedBefore, false, true)
	assert.True(t, retrying[0].Running, "a retry under way is not ended by the attempt before it")
	assert.Empty(t, retrying[0].Answers)

	answered := append(append([]ask.StoredMessage(nil), asked...), ask.StoredMessage{Turn: 2, CreatedAt: at, Message: ask.NewAgenticAssistantText("Because.")})
	undelivered := buildAskExchanges(answered, false, nil, map[int]askRunView{0: {Outcome: "error", Error: "artifact delivery is unavailable"}}, false, false)
	require.NotNil(t, undelivered[0].stopped(), "a turn that failed after its answer was stored did not deliver it")
	assert.Equal(t, "artifact delivery is unavailable", undelivered[0].stopped().Stopped)
	assert.Contains(t, string(undelivered[0].stopped().Body), "Because.", "and what it wrote stays readable")

	thinking := append(append([]ask.StoredMessage(nil), asked...), ask.StoredMessage{Turn: 2, CreatedAt: at,
		Message: assistant(schema.NewContentBlock(&schema.Reasoning{Text: "Still thinking."}))})
	stillRunning := buildAskExchanges(thinking, false, nil, nil, false, true)
	assert.True(t, stillRunning[0].Running, "a message of reasoning alone is a step, not an answer")
	assert.Empty(t, stillRunning[0].Answers)
	assert.Equal(t, "reasoning", stillRunning[0].Steps[1].Kind)

	continues := buildAskExchanges(asked, false, nil, nil, true, true)
	assert.True(t, continues[0].Continues, "a window that stops mid-question continues in the next")
	assert.False(t, continues[0].Running)

	// A window that starts after the question: the result still finds no
	// call, and the answer still belongs to the exchange.
	window := []ask.StoredMessage{
		{Turn: 40, CreatedAt: at, Message: &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{
			toolResult("c9", "retrieve", `{"hits":[]}`),
		}}},
		{Turn: 41, CreatedAt: at, Message: ask.NewAgenticAssistantText("Done.")},
	}
	failedEarlier := buildAskExchanges(window, false, nil, map[int]askRunView{2: {Took: time.Minute}, 30: {Outcome: "error", Error: "artifact delivery is unavailable"}, 50: {}}, false, false)
	require.NotNil(t, failedEarlier[0].stopped(), "a window that starts after the question still knows how its turn ended")
	assert.Equal(t, "artifact delivery is unavailable", failedEarlier[0].stopped().Stopped)

	continued := buildAskExchanges(window, false, nil, nil, false, false)
	require.Len(t, continued, 1)
	assert.True(t, continued[0].Continued)
	require.Len(t, continued[0].Steps, 1)
	assert.Equal(t, "Called in earlier messages", continued[0].Steps[0].Asked)
	assert.Equal(t, "Found 0 relevant results", continued[0].Steps[0].Found)
	require.Len(t, continued[0].Answers, 1)
	assert.Empty(t, continued[0].Answers[0].Stopped)

	missing := buildAskExchanges([]ask.StoredMessage{{Turn: 0, CreatedAt: at}}, false, nil, nil, false, false)
	require.Len(t, missing, 1)
	assert.True(t, missing[0].Steps[0].Failed, "a message that cannot be read is shown as one")
}

func TestBuildAskExchangesKeepsReferenceUnavailableAnswers(t *testing.T) {
	at := time.Date(2026, 11, 17, 18, 39, 0, 0, time.UTC)
	exchanges := buildAskExchanges([]ask.StoredMessage{
		{Turn: 0, CreatedAt: at, Message: ask.NewAgenticUserText("<question>Read this</question>")},
		{Turn: 1, CreatedAt: at, Message: ask.NewAgenticAssistantText("I can't open that link."), IncompleteReason: ask.IncompleteReferenceUnavailable},
	}, true, nil, nil, false, false)
	require.Len(t, exchanges[0].Answers, 1)
	assert.Equal(t, "Answer unavailable because a referenced corpus item could not be accessed.", exchanges[0].Answers[0].Notice)
	assert.Empty(t, exchanges[0].Answers[0].Stopped)
}

func TestAskToolDescriptionsFallBack(t *testing.T) {
	assert.Equal(t, "https://example.test", askGenericAsked([]byte(`{"limit":3,"url":"https://example.test"}`)))
	assert.Equal(t, `{"limit":3}`, askGenericAsked([]byte(`{"limit": 3}`)))
	assert.Empty(t, askGenericAsked([]byte(`{}`)))
	assert.Equal(t, "Returned 3 items", askGenericFound(`[1,2,3]`))
	assert.Equal(t, "Returned 1 item", askGenericFound(`{"issues":[{}]}`))
	assert.Equal(t, "Returned 11 B", askGenericFound(`plain words`))
	assert.Equal(t, "Returned nothing", askGenericFound(""))
}

func TestPlainAskText(t *testing.T) {
	for markdown, want := range map[string]string{
		"Found **four directly relevant** threads in [#546400](https://github.com/org/repo/pull/546400).": "Found four directly relevant threads in #546400.",
		"## Summary\n- *bold* and _emphasis_\n- `code` and ~~gone~~":                                      "Summary bold and emphasis code and gone",
		"See <https://example.slack.com/archives/C1/p1|the thread> or <https://example.test>.":            "See the thread or https://example.test.",
		"```go\nfmt.Println(1)\n```\nsnake_case_name stays":                                               "fmt.Println(1) snake_case_name stays",
		"> quoted\n1. first": "quoted first",
	} {
		assert.Equal(t, want, plainAskText(markdown), markdown)
	}
	assert.Equal(t, "one two…", truncateRunes("one two three four", 10))
	assert.Equal(t, "short", truncateRunes("short", 10))
}

func TestAskRunReasonIsOneReadableLine(t *testing.T) {
	const interrupted = "Interrupted: Diderot restarted or the request was cancelled before the answer was finished."
	for _, test := range []struct{ name, runError, want string }{
		{"a tool call cancelled, as stored",
			"ask service: Eino run: [NodeRunError] failed to invoke tool[name:expand id:call_example01]: context canceled\n------------------------\nnode path: [node_1, ToolNode]",
			interrupted},
		{"a tool call cancelled, on one line",
			"ask service: Eino run: [NodeRunError] failed to invoke tool[name:expand id:call_example01]: context canceled ------------------------ node path: [node_1, ToolNode]",
			interrupted},
		{"a model call cancelled",
			"ask service: Eino run: [NodeRunError] failed to create response: context canceled\n------------------------\nnode path: [node_1, ChatModel]",
			interrupted},
		{"a model call cancelled mid-request",
			"ask service: Eino run: [NodeRunError] failed to create streaming response: Post \"https://gateway.example.test/v1/responses\": context canceled\n------------------------\nnode path: [node_1, ChatModel]",
			interrupted},
		{"a cancelled error cut short in the node path",
			"ask service: Eino run: [NodeRunError] failed to create response: context canceled\n------------------------\nnode pa",
			interrupted},
		{"a tool call out of time",
			"ask service: Eino run: [NodeRunError] failed to invoke tool[name:web_fetch id:call_example02]: context deadline exceeded\n------------------------\nnode path: [node_1, ToolNode]",
			"The request timed out or the connection failed."},
		{"a provider failure",
			"ask service: Eino run: [NodeRunError] failed to create response: POST \"https://gateway.example.test/v1/responses\": 503 Service Unavailable\n------------------------\nnode path: [node_1, ChatModel]",
			"The model provider failed to answer."},
		{"a tool that broke its own rules",
			"ask service: Eino run: [NodeRunError] failed to invoke tool[name:expand id:call_example03]: ask tool invariant: result has no handle\n------------------------\nnode path: [node_1, ToolNode]",
			"The expand tool failed: ask tool invariant: result has no handle"},
		{"a tool behind the framework's function wrapper",
			"ask service: Eino run: [NodeRunError] [LocalFunc] failed to invoke tool, toolName=list, err=ask tool invariant: no scope\n------------------------\nnode path: [node_1, ToolNode]",
			"The list tool failed: ask tool invariant: no scope"},
		{"a streamed tool call",
			"ask service: Eino run: [NodeRunError] failed to stream tool call call_example04: ask tool invariant: no scope\n------------------------\nnode path: [node_1, ToolNode]",
			"A tool failed: ask tool invariant: no scope"},
		{"a request the provider rejected",
			"ask service: Eino run: [NodeRunError] failed to create response: POST \"https://gateway.example.test/v1/responses\": 400 Bad Request {\"error\":{\"message\":\"Unknown parameter: 'reasoning.effort'.\",\"type\":\"invalid_request_error\"}}\n------------------------\nnode path: [node_1, ChatModel]",
			"The model call failed: Unknown parameter: 'reasoning.effort'."},
		{"a nested graph", "ask service: Eino run: [GraphRunError] [NodeRunError] recorder closed\n------------------------\nnode path: [node_0, node_1]",
			"recorder closed"},
		{"a failure outside the run", "ask service: run completed without an assistant message", "run completed without an assistant message"},
		{"a follow-up from somewhere the conversation did not begin",
			"ask service: record session provenance: session is not available to this audience surface",
			"Refused: the conversation began somewhere else, and this follow-up may not be allowed to read everything already in it."},
		{"only wrappers", "ask service: Eino run: [NodeRunError] failed to create response: \n------------------------\nnode path: [node_1, ChatModel]",
			"The model call failed."},
	} {
		got := askStateReason(askFailed, test.runError, "")
		assert.Equal(t, test.want, got, test.name)
		for _, internal := range []string{"Eino", "NodeRunError", "node path", "call_", "---"} {
			assert.NotContains(t, got, internal, test.name)
		}
	}
}

func TestAskStateReasonAndPlace(t *testing.T) {
	assert.Empty(t, askStateReason(askAnswered, "boom", "max_tokens"))
	assert.Empty(t, askStateReason(askRunning, "", ""))
	assert.Equal(t, "Answer stopped at the model's output limit.", askStateReason(askFailed, "boom", "max_tokens"))
	assert.Equal(t, "boom", askStateReason(askFailed, "boom", ""))
	assert.Equal(t, "The model provider limited the request rate.",
		askStateReason(askFailed, "ask service: Eino run: model call: status 429: slow down", ""), "a provider failure reads in plain words")
	assert.Equal(t, "extract F1: the password is wrong", askStateReason(askFailed, "extract F1: the password is wrong", ""),
		"a cause only a file can have is not guessed at")
	assert.Equal(t, "No answer was recorded.", askStateReason(askFailed, "", ""))
	assert.Equal(t, toneBad, askStateTone(askFailed))
	assert.Equal(t, toneAttention, askStateTone(askIncomplete))

	assert.Equal(t, askPlace{Kind: "dm", Label: "Slack DM"}, newAskPlace("slack", "D0EXAMPLE101", "", ""))
	assert.Equal(t, askPlace{Kind: "channel", Label: "#platform"}, newAskPlace("slack", "C1", "platform", ""))
	assert.Equal(t, askPlace{Kind: "channel", Label: "Slack", Detail: "C1"}, newAskPlace("slack", "C1", "", ""))
	assert.Equal(t, askPlace{Kind: "report", Label: "Scheduled report", Detail: "Weekly"}, newAskPlace("scheduled", "", "", "Weekly"))
	assert.Equal(t, "API", askSurfaceLabel("http"))
	assert.Equal(t, "Unknown", askSurfaceLabel("unknown"))
}

func TestSlackAskURL(t *testing.T) {
	assert.Equal(t,
		"https://example.slack.com/archives/C123/p1700000000351689",
		slackAskURL("https://example.slack.com", "C123", "1700000000.351689"))
	assert.Empty(t, slackAskURL("https://example.com/path", "C123", "1700000000.351689"))
	assert.Empty(t, slackAskURL("https://example.com", "../admin", "1700000000.351689"))
	assert.Empty(t, slackAskURL("https://example.com", "C123", "not-a-timestamp"))
}

func TestAskSessionsCursorRoundTrip(t *testing.T) {
	created := time.Date(2026, 8, 3, 17, 42, 1, 123, time.FixedZone("test", -7*60*60))
	id := uuid.MustParse("1f1fab90-87e8-4b3e-9f7d-082fd73899f4")

	cursor, ok := parseAskSessionsCursor(formatAskSessionsCursor(created, id))
	require.True(t, ok)
	assert.True(t, created.Equal(cursor.Created))
	assert.Equal(t, id, cursor.ID)

	_, ok = parseAskSessionsCursor("2026-08-03T00:00:00Z")
	assert.False(t, ok)
}

func renderAskSessions(t *testing.T, data askSessionsData) string {
	t.Helper()
	data.pageData = pageData{Title: "Conversations", CurrentNav: navAsk}
	var out bytes.Buffer
	require.NoError(t, askSessionsPage.ExecuteTemplate(&out, "layout", data))
	return out.String()
}

func TestAskSessionsTemplateEscapesCursorOnce(t *testing.T) {
	html := renderAskSessions(t, askSessionsData{
		Range:    "all",
		Sessions: []askSessionRow{{ID: uuid.NewString(), Title: "One", State: askAnswered, Tone: toneGood}},
		NextURL:  "?before=2026-08-03T12%3A00%3A00Z%7C1f1fab90-87e8-4b3e-9f7d-082fd73899f4",
	})
	assert.Contains(t, strings.ToLower(html), "%7c")
	assert.NotContains(t, strings.ToLower(html), "%257c", "cursor must not be double encoded")
	assert.Contains(t, html, ">Older</a>")
}

func TestAskSessionsTemplateFilters(t *testing.T) {
	html := renderAskSessions(t, askSessionsData{
		Range: "7d", Where: "C123", Status: askFailed, Tool: "retrieve",
		Channels: []askOption{{Value: "C123", Label: "#eng-ops"}},
		Tools:    []string{"retrieve"},
	})
	assert.Contains(t, html, `<option value="C123" selected>#eng-ops</option>`, "channels are offered by name")
	assert.Contains(t, html, `<option value="failed" selected>Failed</option>`)
	assert.Contains(t, html, `<details class="disclosure ask-more" open>`, "a set filter keeps More filters open")
	assert.Contains(t, html, `<a href="/admin/ask/sessions">Clear</a>`)
	assert.Contains(t, html, "No conversations match these filters")

	plain := renderAskSessions(t, askSessionsData{Range: "all", Sessions: []askSessionRow{{ID: uuid.NewString(), Title: "One", State: askAnswered, Tone: toneGood}}})
	assert.Contains(t, plain, `<details class="disclosure ask-more">`)
	assert.NotContains(t, plain, ">Clear</a>")

	fresh := renderAskSessions(t, askSessionsData{Range: "all"})
	assert.NotContains(t, fresh, `class="filter-bar"`, "a fresh install has nothing to filter")
	assert.Contains(t, fresh, "No conversations yet", "a fresh install says how to ask the first question")
	assert.Contains(t, fresh, "<code>POST /v1/ask</code>")
	assert.Contains(t, fresh, "docs/quickstart.md#from-zero-to-a-first-answer", "where connecting an MCP client is shown")
	assert.NotContains(t, fresh, "diderot ask", "there is no client command")

	noMatch := renderAskSessions(t, askSessionsData{Range: "all", Query: "https://x.slack.com/archives/C1/p1", NoMatch: "No conversation started from, or answered in, that Slack message."})
	assert.Contains(t, noMatch, "No conversation found")
}

func TestAskSessionsTemplateRows(t *testing.T) {
	created := time.Now().Add(-2 * time.Hour)
	html := renderAskSessions(t, askSessionsData{Range: "all", Sessions: []askSessionRow{
		{ID: "8200cfca-60da-41d7-a896-67d228cec0cc", Title: "Rollout", Owner: "op@example.test", Created: created,
			Place: askPlace{Kind: "dm", Label: "Slack DM"}, Took: 24 * time.Second, State: askAnswered, Tone: toneGood, Preview: "It moved."},
		{ID: "9200cfca-60da-41d7-a896-67d228cec0cc", Title: "Incident", Created: created,
			Place: askPlace{Kind: "report", Label: "Scheduled report", Detail: "Weekly digest"}, State: askFailed, Tone: toneBad,
			Preview: "never shown", Reason: "Answer stopped at the model's output limit."},
	}})
	assert.Contains(t, html, `href="/admin/ask/sessions/8200cfca-60da-41d7-a896-67d228cec0cc"`)
	assert.Contains(t, html, "Slack DM")
	assert.Contains(t, html, `<span class="sub">Weekly digest</span>`)
	assert.Contains(t, html, ">2 hours ago</time>")
	assert.Contains(t, html, "24s")
	assert.Contains(t, html, `<span class="badge badge-bad">failed</span>`)
	assert.Contains(t, html, `ask-row-reason">Answer stopped at the model&#39;s output limit.`)
	assert.NotContains(t, html, "never shown", "a failed row shows why, not a stale answer")
	assert.Contains(t, html, "2 conversations shown")
}

func TestParseAskTranscriptWindow(t *testing.T) {
	from, to, err := parseAskTranscriptWindow(map[string][]string{})
	require.NoError(t, err)
	assert.Equal(t, 0, from)
	assert.Equal(t, askTranscriptWindow-1, to)

	from, to, err = parseAskTranscriptWindow(map[string][]string{"from": {"120"}})
	require.NoError(t, err)
	assert.Equal(t, 120, from)
	assert.Equal(t, 219, to)

	from, to, err = parseAskTranscriptWindow(map[string][]string{"to": {"149"}})
	require.NoError(t, err)
	assert.Equal(t, 50, from)
	assert.Equal(t, 149, to)

	_, _, err = parseAskTranscriptWindow(map[string][]string{"from": {"10"}, "to": {"9"}})
	require.ErrorContains(t, err, "greater than")
	_, _, err = parseAskTranscriptWindow(map[string][]string{"from": {"0"}, "to": {"200"}})
	require.ErrorContains(t, err, "cannot exceed")
	_, _, err = parseAskTranscriptWindow(map[string][]string{"from": {"3000000000"}})
	require.ErrorContains(t, err, "between 0")
	_, _, err = parseAskTranscriptWindow(map[string][]string{"to": {"3000000000"}})
	require.ErrorContains(t, err, "between 0")
}

func renderAskSession(t *testing.T, data askSessionData) string {
	t.Helper()
	data.pageData = pageData{Title: data.Title, CurrentNav: navAsk}
	var out bytes.Buffer
	require.NoError(t, askSessionPage.ExecuteTemplate(&out, "layout", data))
	return out.String()
}

func TestAskSessionTemplate(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	id := "8200cfca-60da-41d7-a896-67d228cec0cc"
	html := renderAskSession(t, askSessionData{
		ID: id, Title: "Viewer hierarchy", Owner: "carol@example.com", Created: at, LastTurn: at,
		Place: askPlace{Kind: "channel", Label: "#platform", Href: "https://example.slack.com/archives/C1/p1"},
		Took:  24 * time.Second, State: askAnswered, Tone: toneGood, Model: "claude-opus-5:main",
		Questions: 1, ToolCalls: 1, InputTokens: 73_200, OutputTokens: 1_400,
		Cost: "$1.25", CostExact: "$1.250311", CostNote: "incomplete: 2 calls without a price",
		ChannelID: "C1", ThreadTS: "1700000000.351689",
		Release: &askReleaseView{ID: 42, By: "ci@example.test", Note: "GitHub Actions run 1"},
		Images:  []askInputImageView{{ID: "FIMG1", Filename: "error.png", ProxyURL: "/admin/ask/images/FIMG1", FailureReason: "skipped:too-small", CurrentCaption: "a login error dialog"}},
		Exchanges: []askExchangeView{{
			Anchor: "turn-0", Asked: at, Asker: "Carol", Question: "<p>Original question</p>", Context: `{"time":{}}`,
			Steps:   []askStepView{{Anchor: "turn-1-block-0", Kind: "tool", Tool: "retrieve", CallID: "call-1", Asked: "rollout", Found: "Found 8 relevant results", Took: 1200 * time.Millisecond, Input: "{}", Returned: true, Output: "{}", OutputSize: 2048}},
			Answers: []askAnswerView{{Anchor: "turn-2", At: at, Body: "<p>Delivered answer</p>", ResponseID: "resp_123"}},
		}},
	})
	assert.Contains(t, html, `<a href="/admin/ask/sessions">Conversations</a>`, "the breadcrumb leads back to the list")
	assert.Contains(t, html, `Asked by <strong>carol@example.com</strong> in <a href="https://example.slack.com/archives/C1/p1"`)
	assert.Contains(t, html, "took 24s")
	assert.Contains(t, html, "73.2K in · 1.4K out")
	assert.Contains(t, html, "$1.25")
	assert.Contains(t, html, "incomplete: 2 calls without a price")
	assert.Contains(t, html, `data-copy-text="`+id+`"`)
	assert.Contains(t, html, `<a href="/admin/prompts/versions/42">Version 42</a>`, "the prompts it followed link to their version")
	assert.Contains(t, html, `<div class="page-title"><h1>Viewer hierarchy</h1><span class="badge badge-`, "the state sits beside the title")
	assert.Contains(t, html, "GitHub Actions run 1")
	assert.Contains(t, html, `src="/admin/ask/images/FIMG1"`)
	assert.Contains(t, html, "skipped:too-small")
	assert.Contains(t, html, "Description stored now")
	assert.Contains(t, html, "Original question")
	assert.Contains(t, html, "Context sent with the question")
	assert.Contains(t, html, `<details class="record" id="turn-1-block-0">`, "each tool call is one collapsed line")
	assert.Contains(t, html, "Found 8 relevant results")
	assert.Contains(t, html, `<span>1.2s</span>`, "a quick tool call keeps its tenth of a second")
	assert.Contains(t, html, "2.0 KiB")
	assert.Contains(t, html, "Delivered answer")
	assert.Contains(t, html, "resp_123")
	assert.NotContains(t, html, "role=\"alert\"", "an answered conversation raises nothing")
}

func TestAskSessionTemplateFailure(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	html := renderAskSession(t, askSessionData{
		ID: uuid.NewString(), Title: "Incident", Created: at, LastTurn: at, Place: askPlace{Kind: "api", Label: "API"},
		State: askFailed, Tone: toneBad, Reason: "Answer stopped at the model's output limit.", ReasonAnchor: "turn-6",
		CostNote: "incomplete: 2 calls with unknown billing",
		Window:   &askWindow{From: 100, To: 199, Total: 240, EarlierURL: "?from=0&to=99", LaterURL: "?from=200&to=239"},
		Exchanges: []askExchangeView{{
			Anchor: "turn-4", Asked: at, Question: "<p>And then?</p>",
			Answers: []askAnswerView{{Anchor: "turn-6", Stopped: "Answer stopped at the model's output limit.", Body: "<p>Partial list</p>"}},
		}},
	})
	assert.Contains(t, html, "through the API")
	assert.Contains(t, html, "<dt>Cost</dt><dd><span class=\"muted\">Unknown (incomplete: 2 calls with unknown billing)</span>",
		"a gap in the meter shows even when it priced nothing")
	assert.Contains(t, html, `<div class="notice notice-bad" role="alert">`)
	assert.Contains(t, html, "The last question got no answer.")
	assert.Contains(t, html, `<a href="#turn-6">Show where it stopped</a>`)
	assert.Contains(t, html, `id="turn-6"><p><strong>No answer.</strong>`)
	assert.Contains(t, html, "<summary>What the model wrote</summary>")
	assert.Contains(t, html, "Messages 101–200 of 240")
	assert.Contains(t, html, `href="?from=0&amp;to=99">Earlier</a>`)
	assert.Contains(t, html, `href="?from=200&amp;to=239">Later</a>`)
}

func TestFormatAskCost(t *testing.T) {
	tests := []struct {
		name                 string
		usage                *sessionfeed.Usage
		rounded, exact, note string
	}{
		{name: "unavailable"},
		{name: "complete", usage: &sessionfeed.Usage{MeteredCostUSD: "0.184211", CoverageComplete: true}, rounded: "$0.18", exact: "$0.184211"},
		{name: "estimated", usage: &sessionfeed.Usage{MeteredCostUSD: "0.1842", CoverageComplete: true, HasEstimates: true}, rounded: "$0.18", exact: "$0.1842", note: "partly estimated"},
		{name: "tiny", usage: &sessionfeed.Usage{MeteredCostUSD: "0.0004", CoverageComplete: true}, rounded: "<$0.01", exact: "$0.0004"},
		{name: "large", usage: &sessionfeed.Usage{MeteredCostUSD: "123.456", CoverageComplete: true}, rounded: "$123", exact: "$123.456"},
		{name: "incomplete estimated", usage: &sessionfeed.Usage{MeteredCostUSD: "0.1842", HasEstimates: true}, rounded: "$0.18", exact: "$0.1842", note: "incomplete: partly estimated"},
		{name: "unknown billing", usage: &sessionfeed.Usage{MeteredCostUSD: "1.25", UnknownBillingAttemptCount: 2}, rounded: "$1.25", exact: "$1.25", note: "incomplete: 2 calls with unknown billing"},
		{name: "unpriced", usage: &sessionfeed.Usage{MeteredCostUSD: "1.25", UnpricedMeteredAttemptCount: 1}, rounded: "$1.25", exact: "$1.25", note: "incomplete: 1 call without a price"},
		{name: "other currency", usage: &sessionfeed.Usage{Currency: "EUR", MeteredCostUSD: "0.50", CoverageComplete: true}, rounded: "EUR 0.50", exact: "EUR 0.50"},
		{name: "coverage only", usage: &sessionfeed.Usage{}, note: "incomplete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rounded, exact, note := formatAskCost(tt.usage)
			assert.Equal(t, tt.rounded, rounded)
			assert.Equal(t, tt.exact, exact)
			assert.Equal(t, tt.note, note)
		})
	}
}

func TestAskResponseID(t *testing.T) {
	message := ask.NewAgenticAssistantText("done")
	assert.Empty(t, askResponseID(message))
	message.ResponseMeta = &schema.AgenticResponseMeta{OpenAIExtension: &openai.ResponseMetaExtension{
		ID: "resp_2", PreviousResponseID: "resp_1", Status: openai.ResponseStatusCompleted,
	}}
	assert.Equal(t, "resp_2", askResponseID(message))
}
