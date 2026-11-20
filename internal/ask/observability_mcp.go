package ask

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The alert-rules tool is checked for a read-only operation enum when a
// grafana server lists it. This is mcp-grafana 2.x's name; 1.x served the tool
// as alerting_manage_rules, which is no longer reviewed, so a 1.x server's
// alert-rules tool is refused like any other unreviewed tool.
const alertRulesToolName = "grafana_alerting_rules_read"

// reviewedGrafanaTools were reviewed as read-only, so a grafana server may
// list them whatever annotations it reports for them. The alert-rules tool is
// admitted only with its read-only operation enum. Any other listed tool
// needs readOnlyHint without destructiveHint, or the server's
// opt-in to tools not marked read-only.
var reviewedGrafanaTools = map[string]struct{}{
	alertRulesToolName:                        {},
	"grafana_get_dashboard_panel_queries":     {},
	"grafana_get_dashboard_property":          {},
	"grafana_get_dashboard_summary":           {},
	PanelImageToolName:                        {},
	"grafana_list_loki_label_names":           {},
	"grafana_list_loki_label_values":          {},
	"grafana_list_prometheus_label_names":     {},
	"grafana_list_prometheus_label_values":    {},
	"grafana_list_prometheus_metric_metadata": {},
	"grafana_list_prometheus_metric_names":    {},
	"grafana_list_pyroscope_label_names":      {},
	"grafana_list_pyroscope_label_values":     {},
	"grafana_list_pyroscope_profile_types":    {},
	"grafana_query_loki_logs":                 {},
	"grafana_query_loki_patterns":             {},
	"grafana_query_loki_stats":                {},
	"grafana_query_prometheus":                {},
	"grafana_query_prometheus_histogram":      {},
	"grafana_query_pyroscope":                 {},
	"grafana_search_dashboards":               {},
	"tempo_docs-traceql":                      {},
	"tempo_get-attribute-names":               {},
	"tempo_get-attribute-values":              {},
	"tempo_get-trace":                         {},
	"tempo_traceql-metrics-instant":           {},
	"tempo_traceql-metrics-range":             {},
	"tempo_traceql-search":                    {},
}

func validateReadOnlyAlertSchema(remote *mcp.Tool) error {
	raw, err := json.Marshal(remote.InputSchema)
	if err != nil {
		return fmt.Errorf("inspect alert schema: %w", err)
	}
	var schemaMap map[string]any
	if err := json.Unmarshal(raw, &schemaMap); err != nil {
		return fmt.Errorf("inspect alert schema: %w", err)
	}
	properties, _ := schemaMap["properties"].(map[string]any)
	operation, _ := properties["operation"].(map[string]any)
	enum, _ := operation["enum"].([]any)
	got := make([]string, 0, len(enum))
	for _, value := range enum {
		if text, ok := value.(string); ok {
			got = append(got, text)
		}
	}
	want := []string{"list", "get", "versions"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		return fmt.Errorf("%s is not read-only: operations=%v", remote.Name, got)
	}
	return nil
}

func observabilityPresentation() ToolPresentation {
	return ToolPresentation{
		Status:       func(json.RawMessage) StatusLine { return StatusLine{Emoji: "mag", Verb: "querying observability"} },
		StatusOutput: func(json.RawMessage) string { return "observability result received" },
	}
}

type panelImageMCPTool struct {
	session     *mcp.ClientSession
	store       *ArtifactStore
	description string
}

func (t *panelImageMCPTool) Info(context.Context) (*schema.ToolInfo, error) {
	js := &jsonschema.Schema{}
	if err := json.Unmarshal([]byte(`{
  "type":"object",
  "additionalProperties":false,
  "properties":{
    "dashboardUid":{"type":"string","description":"Stored Grafana dashboard UID."},
    "panelId":{"type":"integer","description":"Panel ID. Omit only to render the full dashboard."},
    "theme":{"type":"string","enum":["dark","light"]},
    "timeRange":{"type":"object","additionalProperties":false,"properties":{"from":{"type":"string"},"to":{"type":"string"}},"required":["from","to"]},
    "variables":{"type":"object","additionalProperties":{"oneOf":[{"type":"string"},{"type":"array","items":{"type":"string"}}]}}
  },
  "required":["dashboardUid","timeRange"]
}`), js); err != nil {
		return nil, err
	}
	desc := strings.TrimSpace(t.description)
	if desc == "" {
		desc = "Render a stored Grafana panel or dashboard as a durable PNG for the model and Ask result."
	}
	desc += " Width, height, scale, and timeout are server-controlled. The result includes an artifact_id; select it in the final artifacts field only when it belongs with the final answer."
	return &schema.ToolInfo{Name: PanelImageToolName, Desc: desc, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(js)}, nil
}

// normalizePanelTimeRange rewrites absolute timeRange bounds into epoch
// milliseconds in place.
//
// The renderer silently ignores an RFC 3339 bound instead of rejecting it, and
// renders the panel over an empty window — a valid PNG that says "No data".
// The same panel and window return a small "No data" image for
// "2026-08-13T00:00:00Z" and a full chart for the identical instant as epoch
// milliseconds. Nothing in the response distinguishes the two, so a model
// that sends the format its own tool schema advertises ("e.g., 'now-1h',
// '2024-01-01T00:00:00Z'") concludes that observability data is missing and
// reports a broken render pipeline.
//
// Relative expressions ("now", "now-6h") are forwarded untouched: those the
// renderer does honor. Anything already numeric is assumed to be epoch millis.
func normalizePanelTimeRange(arguments map[string]any) {
	timeRange, ok := arguments["timeRange"].(map[string]any)
	if !ok {
		return
	}
	for _, bound := range []string{"from", "to"} {
		text, ok := timeRange[bound].(string)
		if !ok {
			continue
		}
		if millis, ok := epochMillisForAbsoluteTime(text); ok {
			timeRange[bound] = millis
		}
	}
}

// epochMillisForAbsoluteTime converts the absolute timestamp layouts a model
// plausibly emits into an epoch-millisecond string. It reports false for
// relative expressions, already-numeric values, and anything unparsable, all of
// which must pass through unchanged rather than be guessed at.
func epochMillisForAbsoluteTime(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.HasPrefix(trimmed, "now") {
		return "", false
	}
	if _, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return "", false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		parsed, err := time.Parse(layout, trimmed)
		if err != nil {
			continue
		}
		return strconv.FormatInt(parsed.UTC().UnixMilli(), 10), true
	}
	return "", false
}

func (t *panelImageMCPTool) InvokableRun(ctx context.Context, argument *schema.ToolArgument, _ ...tool.Option) (*schema.ToolResult, error) {
	if argument == nil || strings.TrimSpace(argument.Text) == "" {
		return nil, errors.New("panel image arguments are required")
	}
	var args struct {
		DashboardUID string                     `json:"dashboardUid"`
		PanelID      *int                       `json:"panelId,omitempty"`
		Theme        string                     `json:"theme,omitempty"`
		TimeRange    map[string]string          `json:"timeRange"`
		Variables    map[string]json.RawMessage `json:"variables,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(argument.Text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return nil, fmt.Errorf("panel image arguments: %w", err)
	}
	if strings.TrimSpace(args.DashboardUID) == "" || strings.TrimSpace(args.TimeRange["from"]) == "" || strings.TrimSpace(args.TimeRange["to"]) == "" {
		return nil, errors.New("dashboardUid and timeRange.from/to are required")
	}
	var rawArguments map[string]any
	if err := json.Unmarshal([]byte(argument.Text), &rawArguments); err != nil {
		return nil, err
	}
	normalizePanelTimeRange(rawArguments)
	data, deepLink, err := t.render(ctx, rawArguments)
	if err != nil {
		return nil, err
	}
	// Blank renders are suppressed by the child's report contract and the root's
	// explicit final artifacts selection, not here. Grafana returns its "No
	// data" state as an ordinary successful PNG with no flag to test, and it
	// draws that state differently per visualization: a timeseries panel paints
	// small centred grey text (about 1% of pixels non-background), while a
	// stat panel paints "No data" in the same enormous green glyphs it would
	// use for a real value (about 13%). Any pixel-density
	// threshold that catches the first misses the second, and a threshold low
	// enough to be safe for a legitimately sparse stat or gauge panel catches
	// neither. Guessing here would discard valid panels irreversibly, before
	// they are ever catalogued. The judgment belongs where the query results
	// are: the child knows whether its window returned points.
	// Every render is catalogued, including ones the root never attaches. That
	// is deliberate and cannot be deferred until after selection: the artifact_id
	// in this result is the only handle the model has for selecting the panel
	// later, and HydrateHistory reads these same bytes back on follow-up turns,
	// so an unattached render is live session history rather than garbage. The
	// cost is that unselected renders accumulate until the session is deleted;
	// the right boundary for reclaiming them is a session retention policy,
	// which diderot does not have yet, not a per-turn sweep.
	sessionID := artifactSessionFromContext(ctx)
	callID := compose.GetToolCallID(ctx)
	record, err := t.store.PersistPanelImage(ctx, sessionID, callID, data)
	if err != nil {
		return nil, err
	}
	metadata := PanelArtifactMetadata{Schema: panelArtifactSchema, ArtifactID: record.ID, MediaType: record.MediaType, ByteSize: record.ByteSize, Width: record.Width, Height: record.Height, DeepLink: deepLink}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{
		{Type: schema.ToolPartTypeText, Text: string(metadataJSON)},
		{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &encoded, MIMEType: "image/png"}}},
	}}, nil
}

// render calls the panel image tool and returns the PNG bytes and the Grafana
// deep link, accepting exactly one PNG, at most one text block, and no
// structured content but the deep link.
func (t *panelImageMCPTool) render(ctx context.Context, arguments map[string]any) ([]byte, string, error) {
	result, err := t.session.CallTool(ctx, &mcp.CallToolParams{Name: PanelImageToolName, Arguments: arguments})
	if err != nil {
		return nil, "", fmt.Errorf("call panel image MCP tool: %w", err)
	}
	if result == nil || result.IsError {
		return nil, "", errors.New("panel image MCP tool returned an error")
	}
	structuredLink, err := structuredPanelDeepLink(result.StructuredContent)
	if err != nil {
		return nil, "", err
	}
	var imageContent *mcp.ImageContent
	deepLink := ""
	for _, content := range result.Content {
		switch typed := content.(type) {
		case *mcp.ImageContent:
			if imageContent != nil {
				return nil, "", errors.New("panel image MCP tool returned multiple images")
			}
			imageContent = typed
		case *mcp.TextContent:
			if deepLink != "" {
				return nil, "", errors.New("panel image MCP tool returned multiple text blocks")
			}
			deepLink = strings.TrimSpace(typed.Text)
		default:
			return nil, "", fmt.Errorf("panel image MCP tool returned unsupported content %T", content)
		}
	}
	if imageContent == nil || imageContent.MIMEType != "image/png" {
		return nil, "", errors.New("panel image MCP tool did not return exactly one PNG")
	}
	// The SDK has already decoded the base64 wire form into bytes.
	if len(imageContent.Data) > maxPanelImageBytes {
		return nil, "", errors.New("panel image exceeds the accepted byte limit")
	}
	if structuredLink != "" {
		if deepLink != "" && deepLink != structuredLink {
			return nil, "", errors.New("panel image MCP tool returned conflicting deep links")
		}
		deepLink = structuredLink
	}
	return imageContent.Data, deepLink, nil
}

// structuredPanelDeepLink reads the structured content of a panel render.
// mcp-grafana 1.6 and later repeat the deep link there, as {"deeplink": url},
// for MCP Apps hosts that drop content metadata; anything else is refused.
func structuredPanelDeepLink(structured any) (string, error) {
	if structured == nil {
		return "", nil
	}
	fields, ok := structured.(map[string]any)
	link, isString := fields["deeplink"].(string)
	if !ok || len(fields) != 1 || !isString {
		return "", errors.New("panel image MCP tool returned unexpected structured content")
	}
	return strings.TrimSpace(link), nil
}
