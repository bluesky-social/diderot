package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestRecordInactiveObservabilitySteeringPersistsEmptyCardIDs(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	store := NewArtifactStore(pool, &memoryArtifactBlob{})
	sessionID := newAgenticSession(t, mgr)
	steering := BuildObservabilitySteering("I am getting 529 errors from claude. Why is that?")
	require.False(t, steering.Active)
	require.NoError(t, store.RecordSteering(context.Background(), sessionID, 0, steering))

	var cardIDs []string
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT card_ids FROM ask_observability_steering
		 WHERE session_id=$1 AND user_turn=0`, sessionID).Scan(&cardIDs))
	require.NotNil(t, cardIDs)
	require.Empty(t, cardIDs)
}

// The renderer honors relative and epoch-millisecond bounds but silently
// renders an empty window for an RFC 3339 one, so absolute times must be
// converted before the call and everything else must pass through untouched.
func TestNormalizePanelTimeRange(t *testing.T) {
	t.Parallel()

	// 2026-08-13T00:00:00Z as epoch milliseconds.
	const aug13Millis = "1786579200000"

	for _, tc := range []struct {
		name     string
		from, to any
		wantFrom any
		wantTo   any
	}{
		{"rfc3339 becomes epoch millis", "2026-08-13T00:00:00Z", "2026-08-14T08:14:00Z", aug13Millis, "1786695240000"},
		{"fractional seconds", "2026-08-13T00:00:00.500Z", "now", "1786579200500", "now"},
		{"offset is normalized to utc", "2026-08-12T17:00:00-07:00", "now", aug13Millis, "now"},
		{"date only", "2026-08-13", "now", aug13Millis, "now"},
		{"relative bounds untouched", "now-6h", "now", "now-6h", "now"},
		{"epoch millis untouched", aug13Millis, "1786781640000", aug13Millis, "1786781640000"},
		{"unparsable untouched", "last tuesday", "", "last tuesday", ""},
		{"non-string untouched", float64(1), true, float64(1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := map[string]any{
				"dashboardUid": "edge-uploads",
				"timeRange":    map[string]any{"from": tc.from, "to": tc.to},
			}
			normalizePanelTimeRange(args)
			got := args["timeRange"].(map[string]any)
			require.Equal(t, tc.wantFrom, got["from"])
			require.Equal(t, tc.wantTo, got["to"])
		})
	}

	// A missing or wrongly-typed timeRange must not panic; the tool's own
	// required-field check reports it.
	for _, args := range []map[string]any{
		{},
		{"timeRange": "now-6h"},
		{"timeRange": map[string]any{}},
	} {
		require.NotPanics(t, func() { normalizePanelTimeRange(args) })
	}
}

// The child, not the tool layer, decides whether a rendered panel is worth
// passing up, so the report contract has to say what makes one worthless and
// what to do about it. A successful render is not evidence of data.
func TestInvestigateChildReportContractForbidsEmptyPanels(t *testing.T) {
	t.Parallel()

	require.Contains(t, investigateGrafanaReportDiscipline, "grafana_get_panel_image")
	require.Contains(t, investigateGrafanaReportDiscipline, `"No data" image`)
	require.Contains(t, investigateGrafanaReportDiscipline, "leave that artifact out")
	require.Contains(t, investigateGrafanaReportDiscipline, "mention it under Remaining uncertainty")
	require.NotContains(t, investigateChildReportContract, "grafana_get_panel_image",
		"the common report shape must also work for a Sentry-only child")
}

// The alert-rules tool gets a read-only enum check, and the error names it.
// mcp-grafana 1.x's name for it and 2.x's write tool are not reviewed.
func TestReadOnlyAlertSchemaAndOrdinaryResultPolicy(t *testing.T) {
	for _, name := range []string{alertRulesToolName} {
		t.Run(name, func(t *testing.T) {
			require.Contains(t, reviewedGrafanaTools, name)
			for _, tc := range []struct {
				operations string
				wantErr    string
			}{
				{`["list","get","versions"]`, ""},
				{`["list","get","update"]`, name + " is not read-only"},
				{`["list","get","versions","create"]`, name + " is not read-only"},
				{`["list","get"]`, name + " is not read-only"},
			} {
				var remote mcp.Tool
				require.NoError(t, json.Unmarshal([]byte(`{
				  "name":"`+name+`",
				  "inputSchema":{"type":"object","properties":{"operation":{"type":"string","enum":`+tc.operations+`}}}
				}`), &remote))
				err := validateReadOnlyAlertSchema(&remote)
				if tc.wantErr == "" {
					require.NoError(t, err, tc.operations)
					continue
				}
				require.ErrorContains(t, err, tc.wantErr, tc.operations)
			}
		})
	}
	require.NotContains(t, reviewedGrafanaTools, "grafana_alerting_rules_write")
	require.NotContains(t, reviewedGrafanaTools, "grafana_alerting_manage_rules")

	// Since mcp-grafana 2.1, query_prometheus returns its JSON payload as text
	// and again as structured content; the model gets it once.
	normalize := mcpServerProfiles[MCPServerGrafana].normalize
	payload := `{"data":{"resultType":"vector","result":[]},"hints":["no data"]}`
	var structured map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &structured))
	result, err := normalize(context.Background(), "grafana_query_prometheus", &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: payload}},
		StructuredContent: structured,
	})
	require.NoError(t, err)
	require.Nil(t, result.StructuredContent)
	require.Len(t, result.Content, 1)
	require.Equal(t, payload, result.Content[0].(*mcp.TextContent).Text)

	_, err = normalize(context.Background(), "tool", &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{MIMEType: "image/png", Data: []byte{0}},
	}})
	require.ErrorContains(t, err, "unsupported MCP content")
}

type memoryArtifactBlob struct {
	data      map[string][]byte
	typeByKey map[string]string
}

func (m *memoryArtifactBlob) Put(_ context.Context, key, contentType string, data []byte) error {
	if m.data == nil {
		m.data = make(map[string][]byte)
		m.typeByKey = make(map[string]string)
	}
	m.data[key] = append([]byte(nil), data...)
	m.typeByKey[key] = contentType
	return nil
}

func (m *memoryArtifactBlob) Get(_ context.Context, key string) ([]byte, string, error) {
	data, ok := m.data[key]
	if !ok {
		return nil, "", errors.New("not found")
	}
	return append([]byte(nil), data...), m.typeByKey[key], nil
}

func (*memoryArtifactBlob) Bucket() string { return "test" }

func testPanelPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestArtifactSelectionCommitsAtomicallyAndHydrates(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	store := NewArtifactStore(pool, &memoryArtifactBlob{})
	sessionID := newAgenticSession(t, mgr)
	record, err := store.PersistPanelImage(context.Background(), sessionID, "call-panel", testPanelPNG(t))
	require.NoError(t, err)

	selection := ArtifactSelection{ArtifactID: record.ID, Title: "API error rate", AltText: "Panel showing the API error rate."}
	require.NoError(t, mgr.AppendAgenticMessagesWithArtifacts(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("show the panel")},
		{Turn: 1, Message: NewAgenticAssistantText("Here it is")},
	}, 1, []ArtifactSelection{selection}))
	descriptors, err := store.LoadResultArtifacts(context.Background(), sessionID, 1)
	require.NoError(t, err)
	require.Len(t, descriptors, 1)
	require.Equal(t, record.ID, descriptors[0].ID)

	metadata, err := json.Marshal(PanelArtifactMetadata{Schema: panelArtifactSchema, ArtifactID: record.ID})
	require.NoError(t, err)
	history := []StoredMessage{{Turn: 2, Message: newAgenticToolResult("call-panel", PanelImageToolName, string(metadata))}}
	hydrated := store.HydrateHistory(context.Background(), sessionID, history)
	result := AgenticMessageToolResults(hydrated[0].Message)[0]
	require.Len(t, result.Content, 2)
	require.Equal(t, schema.FunctionToolResultContentBlockTypeImage, result.Content[1].Type)
	require.NotEmpty(t, result.Content[1].Image.Base64Data)

	otherSession := newAgenticSession(t, mgr)
	err = mgr.AppendAgenticMessagesWithArtifacts(context.Background(), otherSession, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("wrong session")},
		{Turn: 1, Message: NewAgenticAssistantText("no")},
	}, 1, []ArtifactSelection{selection})
	require.ErrorContains(t, err, "not a ready panel image")
	_, otherHistory, err := mgr.LoadAgenticSession(context.Background(), otherSession)
	require.NoError(t, err)
	require.Empty(t, otherHistory, "artifact validation failure must roll back the answer batch")
}

func TestEnhancedRecordingKeepsImageOutOfPersistence(t *testing.T) {
	recorder, appender, _ := newRecorderTest(t, 0)
	require.NoError(t, recorder.CaptureAssistant(assistantCallMsg(call("panel-call", PanelImageToolName))))
	middleware := newAgenticRecordingMiddleware()
	endpoint, err := middleware.WrapEnhancedInvokableToolCall(context.Background(),
		func(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.ToolResult, error) {
			encoded := "iVBORw0KGgo="
			return &schema.ToolResult{Parts: []schema.ToolOutputPart{
				{Type: schema.ToolPartTypeText, Text: `{"schema":"ask-panel-artifact-v1","artifact_id":"` + uuid.NewString() + `"}`},
				{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &encoded, MIMEType: "image/png"}}},
			}}, nil
		}, &adk.ToolContext{Name: PanelImageToolName, CallID: "panel-call"})
	require.NoError(t, err)
	ctx := withAgenticRunRecorder(context.Background(), recorder)
	result, err := endpoint(ctx, &schema.ToolArgument{Text: `{}`})
	require.NoError(t, err)
	require.Len(t, result.Parts, 2, "model-visible result remains multimodal")
	require.NoError(t, recorder.CommitToolBatch(ctx))
	_, batches := appender.snapshot()
	require.Len(t, batches, 1)
	persisted := AgenticMessageToolResults(batches[0][2].Message)[0]
	require.Len(t, persisted.Content, 1)
	require.Equal(t, schema.FunctionToolResultContentBlockTypeText, persisted.Content[0].Type)
}
