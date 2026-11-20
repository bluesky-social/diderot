package ask

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

// searchLikeSchema has the shape of Diderot's search tools: one required
// parameter and filters the model should be free not to set.
const searchLikeSchema = `{
  "type": "object",
  "properties": {
    "question": {"type": "string", "description": "What to look for."},
    "after": {"type": "string", "description": "Only results after this time."},
    "window": {"type": "string", "enum": ["day", "week"]},
    "sources": {"type": "array", "items": {"type": "string", "enum": ["slack", "notion"]}},
    "limit": {"type": "integer", "minimum": 1, "maximum": 50, "default": 8}
  },
  "required": ["question"]
}`

// freeFormSchema cannot be made strict: its "labels" object takes any keys.
const freeFormSchema = `{
  "type": "object",
  "properties": {
    "query": {"type": "string"},
    "labels": {"type": "object", "additionalProperties": {"type": "string"}}
  },
  "required": ["query"]
}`

func decodeSchema(t *testing.T, raw string) map[string]any {
	t.Helper()
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
	return decoded
}

func TestStrictParametersMakesOptionalPropertiesNullable(t *testing.T) {
	t.Parallel()

	strict, ok := strictParameters(decodeSchema(t, searchLikeSchema))
	require.True(t, ok)
	require.Equal(t, false, strict["additionalProperties"])
	require.Equal(t, []any{"after", "limit", "question", "sources", "window"}, strict["required"],
		"strict mode wants every property listed")

	properties := strict["properties"].(map[string]any)
	require.Equal(t, "string", properties["question"].(map[string]any)["type"], "a required property is not made nullable")
	require.Equal(t, []any{"string", "null"}, properties["after"].(map[string]any)["type"])
	window := properties["window"].(map[string]any)
	require.Equal(t, []any{"string", "null"}, window["type"])
	require.Equal(t, []any{"day", "week", nil}, window["enum"], "an optional choice must be able to say none of them")
	sources := properties["sources"].(map[string]any)
	require.Equal(t, []any{"array", "null"}, sources["type"])
	require.Equal(t, []any{"slack", "notion"}, sources["items"].(map[string]any)["enum"], "the items of a list are not optional")
	limit := properties["limit"].(map[string]any)
	require.Equal(t, []any{"integer", "null"}, limit["type"])
	require.EqualValues(t, 50, limit["maximum"])
	require.Equal(t, "Only results after this time.", properties["after"].(map[string]any)["description"])
}

func TestStrictParametersClosesNestedObjects(t *testing.T) {
	t.Parallel()

	strict, ok := strictParameters(decodeSchema(t, `{
	  "type": "object",
	  "properties": {
	    "window": {"type": "object", "properties": {"from": {"type": "string"}, "to": {"type": "string"}}, "required": ["from"]},
	    "rows": {"type": "array", "items": {"type": "object", "properties": {"id": {"type": "integer"}, "note": {"type": "string"}}, "required": ["id"]}}
	  },
	  "required": ["window"]
	}`))
	require.True(t, ok)
	properties := strict["properties"].(map[string]any)
	window := properties["window"].(map[string]any)
	require.Equal(t, "object", window["type"])
	require.Equal(t, false, window["additionalProperties"])
	require.Equal(t, []any{"from", "to"}, window["required"])
	require.Equal(t, []any{"string", "null"}, window["properties"].(map[string]any)["to"].(map[string]any)["type"])
	rows := properties["rows"].(map[string]any)
	require.Equal(t, []any{"array", "null"}, rows["type"])
	row := rows["items"].(map[string]any)
	require.Equal(t, "object", row["type"], "the items of a list are not optional")
	require.Equal(t, []any{"id", "note"}, row["required"])
	require.Equal(t, []any{"string", "null"}, row["properties"].(map[string]any)["note"].(map[string]any)["type"])
}

func TestStrictParametersRefusesWhatStrictModeCannotExpress(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]string{
		"an object that takes any keys":    freeFormSchema,
		"an object with no properties":     `{"type":"object","properties":{"q":{"type":"object"}},"required":["q"]}`,
		"oneOf":                            `{"type":"object","properties":{"q":{"oneOf":[{"type":"string"},{"type":"integer"}]}},"required":["q"]}`,
		"allOf":                            `{"type":"object","properties":{"q":{"allOf":[{"type":"string"}]}},"required":["q"]}`,
		"a reference":                      `{"type":"object","properties":{"q":{"$ref":"#/$defs/x"}},"required":["q"],"$defs":{"x":{"type":"string"}}}`,
		"a format outside the list":        `{"type":"object","properties":{"q":{"type":"string","format":"uri"}},"required":["q"]}`,
		"uniqueItems":                      `{"type":"object","properties":{"q":{"type":"array","items":{"type":"string"},"uniqueItems":true}},"required":["q"]}`,
		"a property with no type":          `{"type":"object","properties":{"q":{"description":"anything"}},"required":["q"]}`,
		"items with no type":               `{"type":"object","properties":{"q":{"type":"array","items":{}}},"required":["q"]}`,
		"additional properties allowed":    `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":true}`,
		"a required name with no property": `{"type":"object","properties":{"q":{"type":"string"}},"required":["q","r"]}`,
		"a root that is a union":           `{"type":"object","properties":{"q":{"type":"string"}},"anyOf":[{"required":["q"]}]}`,
		"an optional constant":             `{"type":"object","properties":{"q":{"type":"string"},"mode":{"type":"string","const":"fast"}},"required":["q"]}`,
		"a union":                          `{"type":"object","properties":{"q":{"anyOf":[{"type":"string"},{"type":"integer"}]}},"required":["q"]}`,
		"a type that is a wider union":     `{"type":"object","properties":{"q":{"type":["string","integer"]}},"required":["q"]}`,
		"a list that does not say of what": `{"type":"object","properties":{"q":{"type":"array"}},"required":["q"]}`,
		"a pattern":                        `{"type":"object","properties":{"q":{"type":"string","pattern":"^(?=a)"}},"required":["q"]}`,
		"an object held to listed values":  `{"type":"object","properties":{"q":{"type":"object","properties":{"a":{"type":"string"}},"enum":[{"a":"x"}]}},"required":["q"]}`,
		"a root that is not an object":     `{"type":"string"}`,
	} {
		_, ok := strictParameters(decodeSchema(t, raw))
		require.False(t, ok, name)
	}
}

func TestDropUnsetArguments(t *testing.T) {
	t.Parallel()

	parameters := decodeSchema(t, `{
	  "type": "object",
	  "properties": {
	    "question": {"type": "string"},
	    "after": {"type": "string"},
	    "window": {"type": "object", "properties": {"from": {"type": "string"}, "to": {"type": "string"}}, "required": ["from"]},
	    "rows": {"type": "array", "items": {"type": "object", "properties": {"id": {"type": "integer"}, "note": {"type": "string"}}, "required": ["id"]}}
	  },
	  "required": ["question"]
	}`)
	for name, tc := range map[string]struct{ in, want string }{
		"an optional null is the same as leaving it out": {`{"question":"q","after":null}`, `{"question":"q"}`},
		"a required null is left for the tool to reject": {`{"question":null,"after":null}`, `{"question":null}`},
		"inside an object":               {`{"question":"q","window":{"from":null,"to":null}}`, `{"question":"q","window":{"from":null}}`},
		"inside a list of objects":       {`{"question":"q","rows":[{"id":1,"note":null}]}`, `{"question":"q","rows":[{"id":1}]}`},
		"numbers keep their digits":      {`{"question":"q","after":null,"n":12345678901234567890}`, `{"n":12345678901234567890,"question":"q"}`},
		"nothing unset, nothing changed": {`{ "question": "null and void" }`, `{ "question": "null and void" }`},
		"not an object":                  {`null`, `null`},
		"not JSON":                       {`{"question": null`, `{"question": null`},
		"text after the object":          {`{"question":"q","after":null} and more`, `{"question":"q","after":null} and more`},
	} {
		require.Equal(t, tc.want, dropUnsetArguments(tc.in, parameters), name)
	}
	require.Equal(t, `{"anything":null}`, dropUnsetArguments(`{"anything":null}`, nil),
		"with no schema nothing is known to be optional")
	require.Equal(t, `{"question":"q","undeclared":null}`, dropUnsetArguments(`{"question":"q","undeclared":null,"after":null}`, parameters),
		"a property the schema does not declare is the tool's to judge")
	hidden := decodeSchema(t, `{"type":"object","properties":{"x":{"type":["string","null"]}},"allOf":[{"required":["x"]}]}`)
	require.Equal(t, `{"x":null}`, dropUnsetArguments(`{"x":null}`, hidden),
		"a combinator may require what the required list does not name")
	hiddenInList := decodeSchema(t, `{"type":"object","properties":{"rows":{"type":"array",
	  "items":{"type":"object","properties":{"x":{"type":["string","null"]}}},
	  "allOf":[{"items":{"required":["x"]}}]}}}`)
	require.Equal(t, `{"rows":[{"x":null}]}`, dropUnsetArguments(`{"rows":[{"x":null}]}`, hiddenInList),
		"nor is a list looked into when a combinator sits beside its items")
	for name, raw := range map[string]string{
		"dependencies":                           `{"type":"object","properties":{"trigger":{"type":"boolean"},"x":{"type":["string","null"]}},"dependencies":{"trigger":["x"]}}`,
		"minProperties":                          `{"type":"object","properties":{"x":{"type":["string","null"]}},"minProperties":1}`,
		"an enum of whole objects":               `{"type":"object","properties":{"x":{"type":["string","null"]}},"enum":[{"x":null}]}`,
		"a list of unique items":                 `{"type":"object","properties":{"x":{"type":["string","null"]}},"uniqueItems":true}`,
		"a keyword this code has never heard of": `{"type":"object","properties":{"x":{"type":["string","null"]}},"x-requires":["x"]}`,
	} {
		require.Equal(t, `{"x":null}`, dropUnsetArguments(`{"x":null}`, decodeSchema(t, raw)), name)
	}

	grid := `{"type":"object","properties":{"grid":{"type":"array","items":{"type":"array",
	  "items":{"type":"object","properties":{"id":{"type":"integer"},"note":{"type":"string"}},"required":["id"]}}}},"required":["grid"]}`
	require.Equal(t, `{"grid":[[{"id":1},{"id":2,"note":"n"}]]}`,
		dropUnsetArguments(`{"grid":[[{"id":1,"note":null},{"id":2,"note":"n"}]]}`, decodeSchema(t, grid)),
		"a list of lists is walked as far as the schema describes it")
	strictGrid, ok := strictParameters(decodeSchema(t, grid))
	require.True(t, ok)
	require.True(t, requirementsReadable(strictGrid), "whatever is sent strict can have its nulls removed")
}

// argumentRecorder is a tool that keeps the arguments it was run with.
type argumentRecorder struct {
	name       string
	parameters string
	result     string

	mu   sync.Mutex
	seen []string
}

func (r *argumentRecorder) Info(context.Context) (*schema.ToolInfo, error) {
	js, err := parseToolJSONSchema(json.RawMessage(r.parameters))
	if err != nil {
		return nil, err
	}
	return &schema.ToolInfo{Name: r.name, Desc: "Search something.", ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js)}, nil
}

func (r *argumentRecorder) InvokableRun(_ context.Context, arguments string, _ ...tool.Option) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, arguments)
	if r.result != "" {
		return r.result, nil
	}
	return `{"ok":true}`, nil
}

func (r *argumentRecorder) arguments() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// runToolAgent drives a real ReAct loop over mdl with tools under Ask's tool
// contract, and returns once the model stops calling tools.
func runToolAgent(t *testing.T, mdl model.AgenticModel, maxResultBytes int, tools ...tool.BaseTool) {
	t.Helper()
	wrapped, err := WrapRecoverableToolErrors(tools, maxResultBytes)
	require.NoError(t, err)
	agent, err := adk.NewTypedChatModelAgent(context.Background(), &adk.TypedChatModelAgentConfig[*schema.AgenticMessage]{
		Name: "tool-contract", Description: "Exercises the tool contract.", Instruction: "Search, then answer.",
		Model: mdl, MaxIterations: 4,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: wrapped}},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	iter := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: agent}).Query(ctx, "find it")
	for {
		event, ok := iter.Next()
		if !ok {
			return
		}
		require.NoError(t, event.Err)
	}
}

func responsesFinalMessage(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeResponsesFixture(t, w, map[string]any{
		"id": "resp_final", "object": "response", "created_at": 2, "status": "completed", "model": "gpt-test",
		"output": []any{map[string]any{
			"type": "message", "id": "msg_final", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "done", "annotations": []any{}}},
		}},
		"usage": responsesUsage(5, 1, 0),
	})
}

// TestResponsesOptionalParameterCanBeLeftUnset is the Responses half of the
// contract: a tool strict mode can express is sent strict with its optional
// parameters nullable, a tool it cannot express is sent non-strict as
// written, and a null the model writes for an optional parameter never
// reaches the tool.
func TestResponsesOptionalParameterCanBeLeftUnset(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls int
		sent  []any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeModelRequest(t, r)
		require.Equal(t, "/v1/responses", req.path)
		mu.Lock()
		calls++
		call := calls
		if call == 1 {
			sent, _ = req.body["tools"].([]any)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call > 1 {
			responsesFinalMessage(t, w)
			return
		}
		writeResponsesFixture(t, w, map[string]any{
			"id": "resp_tools", "object": "response", "created_at": 1, "status": "completed", "model": "gpt-test",
			"parallel_tool_calls": true,
			"output": []any{
				map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "search", "status": "completed",
					"arguments": `{"question":"when did it ship","after":null,"window":null,"sources":null,"limit":null}`},
				map[string]any{"type": "function_call", "id": "fc_2", "call_id": "call_2", "name": "query", "status": "completed",
					"arguments": `{"query":"up"}`},
			},
			"usage": responsesUsage(9, 3, 0),
		})
	}))
	defer server.Close()

	mdl, err := NewResponsesModel(context.Background(), ResponsesConfig{
		BaseURL: server.URL + "/v1", APIKey: "token", ModelID: "gpt-test", Timeout: 5 * time.Second, MaxRetries: 1,
	})
	require.NoError(t, err)
	search := &argumentRecorder{name: "search", parameters: searchLikeSchema}
	query := &argumentRecorder{name: "query", parameters: freeFormSchema}
	runToolAgent(t, mdl, RootToolResultMaxBytes, search, query)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, sent, 2)
	byName := map[string]map[string]any{}
	for _, entry := range sent {
		described := entry.(map[string]any)
		byName[described["name"].(string)] = described
	}
	strict := byName["search"]
	require.Equal(t, true, strict["strict"], "a schema strict mode can express is sent strict")
	parameters := strict["parameters"].(map[string]any)
	require.Equal(t, []any{"after", "limit", "question", "sources", "window"}, parameters["required"])
	require.Equal(t, []any{"string", "null"}, parameters["properties"].(map[string]any)["after"].(map[string]any)["type"])

	loose := byName["query"]
	require.Equal(t, false, loose["strict"], "left unstated, the API would make every property of this tool required")
	require.Equal(t, []any{"query"}, loose["parameters"].(map[string]any)["required"], "a non-strict schema is sent as written")

	require.Equal(t, []string{`{"question":"when did it ship"}`}, search.arguments(),
		"null for an optional parameter means it was not set")
	require.Equal(t, []string{`{"query":"up"}`}, query.arguments())
}

// TestMessagesOptionalParameterCanBeLeftUnset is the Messages half: tools go
// out as written and non-strict, where a property outside "required" may be
// left out of the call, and a model that writes null instead is read the
// same way.
func TestMessagesOptionalParameterCanBeLeftUnset(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls int
		sent  []any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeModelRequest(t, r)
		require.Equal(t, "/v1/messages", req.path)
		mu.Lock()
		calls++
		call := calls
		if call == 1 {
			sent, _ = req.body["tools"].([]any)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call > 1 {
			writeClaudeMessage(t, w, map[string]any{
				"id": "msg_final", "stop_reason": "end_turn",
				"content": []any{map[string]any{"type": "text", "text": "done"}},
			})
			return
		}
		writeClaudeMessage(t, w, map[string]any{
			"id": "msg_tools", "stop_reason": "tool_use",
			"content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "search", "input": map[string]any{"question": "when did it ship"}},
				map[string]any{"type": "tool_use", "id": "toolu_2", "name": "search", "input": map[string]any{"question": "who owns it", "after": nil}},
			},
		})
	}))
	defer server.Close()

	mdl, err := NewModel(context.Background(), ModelConfig{
		BaseURL: server.URL + "/v1", APIKey: "token", Model: registryModel(t, "claude-opus-5:main"), MaxTokens: 256,
	})
	require.NoError(t, err)
	search := &argumentRecorder{name: "search", parameters: searchLikeSchema}
	runToolAgent(t, mdl, RootToolResultMaxBytes, search)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, sent, 1)
	described := sent[0].(map[string]any)
	_, statesStrict := described["strict"]
	require.False(t, statesStrict, "Messages tools are not strict: a property outside required may be left out")
	inputSchema := described["input_schema"].(map[string]any)
	require.Equal(t, []any{"question"}, inputSchema["required"])
	require.Equal(t, "string", inputSchema["properties"].(map[string]any)["after"].(map[string]any)["type"])

	got := search.arguments()
	require.ElementsMatch(t, []string{`{"question":"when did it ship"}`, `{"question":"who owns it"}`}, got)
}

// TestToolResultIsCutAtTheLimit covers the bound on every tool's result, a
// tool from another server included: the cut is at a character boundary, the
// same every time, and says what was cut and what to do.
func TestToolResultIsCutAtTheLimit(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", 300) // 600 bytes
	bounded := boundToolResult(long, 101)
	require.True(t, strings.HasPrefix(bounded, strings.Repeat("é", 50)+"\n…[cut here: this result is 600 bytes and only the first 100 are shown."),
		"cut before the character the limit would split: %q", bounded)
	require.Contains(t, bounded, "Ask again for less")
	require.Equal(t, bounded, boundToolResult(long, 101), "the same result is always cut the same way")
	require.Equal(t, "short", boundToolResult("short", 101))

	huge := &argumentRecorder{name: "search", parameters: searchLikeSchema, result: strings.Repeat("x", 4096)}
	wrapped, err := WrapRecoverableToolErrors([]tool.BaseTool{huge}, 1024)
	require.NoError(t, err)
	result, err := wrapped[0].(tool.InvokableTool).InvokableRun(context.Background(), `{"question":"q"}`)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(result, strings.Repeat("x", 1024)+"\n…[cut here: this result is 4096 bytes and only the first 1024 are shown."))
	require.Less(t, len(result), 1024+400)

	parts := boundToolResultParts(&schema.ToolResult{Parts: []schema.ToolOutputPart{
		{Type: schema.ToolPartTypeText, Text: strings.Repeat("y", 800)},
		{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{}},
		{Type: schema.ToolPartTypeText, Text: strings.Repeat("z", 800)},
		{Type: schema.ToolPartTypeText, Text: strings.Repeat("w", 800)},
	}}, 1024)
	require.Len(t, parts.Parts, 3, "text after the cut is left out, with one line to say so")
	require.Equal(t, strings.Repeat("y", 800), parts.Parts[0].Text)
	require.NotNil(t, parts.Parts[1].Image, "a picture is not text and is left alone")
	require.True(t, strings.HasPrefix(parts.Parts[2].Text,
		strings.Repeat("z", 224)+"\n…[cut here: this result is 2400 bytes and only the first 1024 are shown."),
		"the parts of one result share the limit: %q", parts.Parts[2].Text[:300])

	_, err = WrapRecoverableToolErrors([]tool.BaseTool{huge}, 0)
	require.ErrorContains(t, err, "result size limit")
}

func TestCodeSearchResultFitsItsBudget(t *testing.T) {
	t.Parallel()

	minified := strings.Repeat("a", 3*codeSearchMaxChunkBytes)
	out := codeSearchOutput{TotalMatches: 40}
	for i := 0; i < 20; i++ {
		out.Files = append(out.Files, codeSearchFile{
			Repo: "app", Path: "dist/bundle.js",
			Chunks: []codeSearchChunk{{StartLine: 1, Content: minified}},
		})
	}
	bounded := boundCodeSearchOutput(out)
	require.True(t, bounded.Truncated)
	require.Len(t, bounded.Files, 6, "as many whole matches as the budget holds")
	total := 0
	for _, file := range bounded.Files {
		require.Len(t, file.Chunks, 1)
		require.Contains(t, file.Chunks[0].Content, "…(cut: ")
		total += len(file.Chunks[0].Content)
	}
	require.LessOrEqual(t, total, codeSearchMaxContentBytes)
	require.Contains(t, bounded.Note, "6 of 20 matching files are shown")
	require.Contains(t, bounded.Note, "Narrow the search with repo:, file: or lang:")
	encoded, err := json.Marshal(bounded)
	require.NoError(t, err)
	require.Less(t, len(encoded), 64<<10)

	small := codeSearchOutput{TotalMatches: 1, Files: []codeSearchFile{{Repo: "app", Path: "main.go", Chunks: []codeSearchChunk{{StartLine: 3, Content: "func main() {}"}}}}}
	require.Equal(t, small, boundCodeSearchOutput(small), "an ordinary result is returned as it is")
	oneLongLine := codeSearchOutput{TotalMatches: 1, Files: []codeSearchFile{{Repo: "app", Path: "data.json", Chunks: []codeSearchChunk{{StartLine: 1, Content: minified}}}}}
	require.Equal(t, oneLongLine, boundCodeSearchOutput(oneLongLine), "nothing is cut while the whole result fits")
}

func TestUUIDInsideALongerNameIsNotASessionReference(t *testing.T) {
	t.Parallel()

	const id = "3f2b8c1e-9a4d-4e6f-8b2a-1c3d5e7f9a0b"
	for _, input := range []string{
		"FailedAttachVolume for pvc-" + id + " on node-7",
		"pod " + id + "-worker restarted",
		"volume vol_" + id + " is full",
	} {
		require.Empty(t, ParseReferences(input, nil), input)
	}
	for _, input := range []string{
		"what happened in session " + id + "?",
		"session=" + id,
		"(" + id + ")",
	} {
		refs := ParseReferences(input, nil)
		require.Len(t, refs, 1, input)
		require.Equal(t, ReferenceSessionUUID, refs[0].Kind)
	}
}

// TestStrictSchemaRefusalFallsBackToNonStrict: the API's rules for strict
// schemas are its own. If it refuses a tool list this code believed strict,
// the request is sent again non-strict, and that list is not offered as
// strict a second time. Any other refusal is returned as it came.
func TestStrictSchemaRefusalFallsBackToNonStrict(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		flags [][]any
	)
	refuseEverything := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeModelRequest(t, r)
		tools, _ := req.body["tools"].([]any)
		stated := make([]any, len(tools))
		anyStrict := false
		for i, entry := range tools {
			stated[i] = entry.(map[string]any)["strict"]
			anyStrict = anyStrict || stated[i] == true
		}
		mu.Lock()
		flags = append(flags, stated)
		refuse := refuseEverything
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case refuse:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The model does not exist."}}`))
		case anyStrict:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid schema for function 'search': a rule this code does not know."}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer server.Close()

	client := &http.Client{Transport: &strictToolsTransport{}}
	body := `{"model":"m","input":"hi","tools":[{"type":"function","name":"search","description":"d","parameters":` + searchLikeSchema + `}]}`
	post := func() (int, string) {
		t.Helper()
		resp, err := client.Post(server.URL+"/v1/responses", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		text, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp.StatusCode, string(text)
	}

	status, text := post()
	require.Equal(t, http.StatusOK, status, text)
	status, text = post()
	require.Equal(t, http.StatusOK, status, text)
	mu.Lock()
	require.Equal(t, [][]any{{true}, {false}, {false}}, flags, "refused once, then sent non-strict without asking again")
	refuseEverything = true
	mu.Unlock()

	status, text = post()
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, text, "The model does not exist.", "a refusal that is not about a schema is the caller's to read")
}
