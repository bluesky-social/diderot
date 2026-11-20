package ask

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

// einoToolInfo returns Info() from a RegisteredTool produced by the
// production native constructors, asserting it registers cleanly
// through the same validation the ask service will run.
func einoToolInfo(t *testing.T, reg RegisteredTool) *schema.ToolInfo {
	t.Helper()
	tools, presentations, err := NewToolRegistry(context.Background(), []RegisteredTool{reg})
	require.NoError(t, err)
	require.Len(t, tools, 1)
	info, err := tools[0].Info(context.Background())
	require.NoError(t, err)
	if presentations[info.Name].Status == nil {
		t.Fatal("native constructor missing presentation Status")
	}
	return info
}

func TestEinoRetrieveToolSchema(t *testing.T) {
	reg := NewEinoRetrieveTool(nil, testSources)
	info := einoToolInfo(t, reg)

	if info.Name != "retrieve" {
		t.Errorf("name = %q, want retrieve", info.Name)
	}
	// What each source holds is in the sources parameter, from the source
	// registry, so the whole definition is what has to carry these.
	retrieveSchemaJSON, err := info.ToJSONSchema()
	require.NoError(t, err)
	rawSchema, err := json.Marshal(retrieveSchemaJSON)
	require.NoError(t, err)
	normalizedDesc := strings.Join(strings.Fields(info.Desc+" "+string(rawSchema)), " ")
	if !strings.Contains(normalizedDesc, "Semantic search over the organization's corpus") {
		t.Errorf("description lost the corpus contract:\n%q", info.Desc)
	}
	if !strings.Contains(normalizedDesc, "one-to-one and group DMs between other people are never indexed") {
		t.Errorf("definition lost the Slack DM coverage boundary:\n%q", normalizedDesc)
	}
	if !strings.Contains(normalizedDesc, "redacted coding-agent session transcripts") {
		t.Errorf("definition lost the canonical agent-session contract:\n%q", normalizedDesc)
	}
	if strings.Contains(normalizedDesc, "Claude/Bedrock interactive sessions") {
		t.Errorf("definition retains the stale agent-session enumeration:\n%q", normalizedDesc)
	}
	if strings.Contains(normalizedDesc, "across clients, harnesses, origin apps, and inference providers") {
		t.Errorf("description exposes agent-session connector details:\n%q", info.Desc)
	}
	if !strings.Contains(info.Desc, "\n") {
		t.Errorf("description lost its reviewable multiline formatting:\n%q", info.Desc)
	}

	// The legacy model-facing schema is a JSON text constant; the
	// native tool must carry it verbatim (enums, min/max/default,
	// required, and every per-property description). Eino's
	// jsonschema.Schema round-trips the document, so compare the
	// serialized forms.
	if info.ParamsOneOf == nil {
		t.Fatal("native tool has nil ParamsOneOf")
	}
	js, err := info.ToJSONSchema()
	require.NoError(t, err)
	got, err := json.Marshal(js)
	require.NoError(t, err)

	var gotDocument, wantDocument map[string]any
	if err := json.Unmarshal(got, &gotDocument); err != nil {
		t.Fatalf("native schema: %v", err)
	}
	if err := json.Unmarshal(retrieveSchema(testSources), &wantDocument); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if !reflect.DeepEqual(gotDocument, wantDocument) {
		t.Errorf("native schema diverges from legacy retrieveSchema\n got: %v\nwant: %v", gotDocument, wantDocument)
	}
	if !strings.Contains(string(got), "one-to-one and group DMs between other people are never indexed") {
		t.Errorf("retrieve schema lost the Slack DM coverage boundary:\n%s", got)
	}
	if strings.Contains(string(got), "across clients, harnesses, origin apps, and inference providers") {
		t.Errorf("retrieve schema exposes agent-session connector details:\n%s", got)
	}
}

func TestEinoListToolSchema(t *testing.T) {
	reg := NewEinoListTool(nil, testSources)
	info := einoToolInfo(t, reg)

	if info.Name != "list" {
		t.Errorf("name = %q, want list", info.Name)
	}
	if !strings.Contains(info.Desc, "To find the best matches for a topic, use `retrieve`.") {
		t.Errorf("description lost the list decision rule:\n%q", info.Desc)
	}
	if info.ParamsOneOf == nil {
		t.Fatal("native tool has nil ParamsOneOf")
	}
	js, err := info.ToJSONSchema()
	require.NoError(t, err)
	got, err := json.Marshal(js)
	require.NoError(t, err)

	var gotDocument, wantDocument map[string]any
	if err := json.Unmarshal(got, &gotDocument); err != nil {
		t.Fatalf("native schema: %v", err)
	}
	if err := json.Unmarshal(listSchema(testSources), &wantDocument); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if !reflect.DeepEqual(gotDocument, wantDocument) {
		t.Errorf("native schema diverges from legacy listSchema\n got: %v\nwant: %v", gotDocument, wantDocument)
	}
	if !strings.Contains(string(got), "one-to-one and group DMs between other people are never indexed") {
		t.Errorf("list schema lost the Slack DM coverage boundary:\n%s", got)
	}
}

func TestEinoCorpusToolsInvokeThroughNativeInterface(t *testing.T) {
	tests := []struct {
		name      string
		reg       RegisteredTool
		arguments string
		wantError string
	}{
		{
			name:      "retrieve typed validation",
			reg:       NewEinoRetrieveTool(nil, testSources),
			arguments: `{"question":"q","top_k":"99999999999999999999"}`,
			wantError: "top_k: ",
		},
		{
			name:      "retrieve reaches business operation",
			reg:       NewEinoRetrieveTool(nil, testSources),
			arguments: `{"question":"q","top_k":"17"}`,
			wantError: "no retriever configured",
		},
		{
			name:      "list typed validation",
			reg:       NewEinoListTool(nil, testSources),
			arguments: `{"sources":["slack"],"occurred_after":"last week"}`,
			wantError: "occurred_after",
		},
		{
			name:      "list reaches business operation",
			reg:       NewEinoListTool(nil, testSources),
			arguments: `{"sources":["slack"],"top_k":"17"}`,
			wantError: "no retriever configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invokable, ok := tt.reg.Tool.(tool.InvokableTool)
			if !ok {
				t.Fatalf("native tool type %T is not tool.InvokableTool", tt.reg.Tool)
			}
			_, err := invokable.InvokableRun(context.Background(), tt.arguments)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestEinoRetrievePresentation(t *testing.T) {
	reg := NewEinoRetrieveTool(nil, testSources)

	if reg.Presentation.Status == nil || reg.Presentation.StatusSources == nil || reg.Presentation.StatusOutput == nil {
		t.Fatal("native retrieve presentation incomplete")
	}
	input := json.RawMessage(`{"question":"what happened in dc2"}`)
	output := json.RawMessage(`{"hits":[{"source":"slack","url":"https://x/1","title":"#ops","actor":{"display":"alice","canonical":{"namespace":"","id":""},"source_identity":{"namespace":"","id":""},"role":"message_author"},"channel":"ops"}]}`)
	if got, want := reg.Presentation.Status(input), retrieveStatus(input); got != want {
		t.Errorf("presentation Status = %+v, want %+v", got, want)
	}
	if got, want := reg.Presentation.StatusSources(output), retrieveStatusSources(output, testSources); !statusSourcesEqual(got, want) {
		t.Errorf("presentation StatusSources = %+v, want %+v", got, want)
	}
	if got, want := reg.Presentation.StatusOutput(output), retrieveStatusOutput(output); got != want {
		t.Errorf("presentation StatusOutput = %q, want %q", got, want)
	}
}

func TestEinoListPresentation(t *testing.T) {
	reg := NewEinoListTool(nil, testSources)

	if reg.Presentation.Status == nil || reg.Presentation.StatusSources == nil || reg.Presentation.StatusOutput == nil {
		t.Fatal("native list presentation incomplete")
	}
	input := json.RawMessage(`{"actors":["alex@example.com"],"sources":["slack"]}`)
	output := json.RawMessage(`{"hits":[{"source":"slack","url":"https://x/1","title":"#ops","actor":{"display":"alice","canonical":{"namespace":"","id":""},"source_identity":{"namespace":"","id":""},"role":"message_author"},"channel":"ops"}],"count":1}`)
	if got, want := reg.Presentation.Status(input), listStatus(input); got != want {
		t.Errorf("presentation Status = %+v, want %+v", got, want)
	}
	if got, want := reg.Presentation.StatusSources(output), listStatusSources(output, testSources); !statusSourcesEqual(got, want) {
		t.Errorf("presentation StatusSources = %+v, want %+v", got, want)
	}
	if got, want := reg.Presentation.StatusOutput(output), listStatusOutput(output); got != want {
		t.Errorf("presentation StatusOutput = %q, want %q", got, want)
	}
}

func statusSourcesEqual(a, b []StatusSource) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
