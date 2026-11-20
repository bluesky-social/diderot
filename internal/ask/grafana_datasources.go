package ask

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Grafana datasource kinds an operator can pin, as GrafanaDatasources keys.
// Tempo tools take no datasource.
const (
	GrafanaPrometheus = "prometheus"
	GrafanaLoki       = "loki"
	GrafanaPyroscope  = "pyroscope"
)

// grafanaDatasourceFields are the argument names Grafana's MCP tools take a
// datasource UID under: Prometheus and Loki tools use one, Pyroscope tools
// the other.
var grafanaDatasourceFields = []string{"datasourceUid", "data_source_uid"}

// grafanaToolKind is the datasource kind a Grafana tool queries, from its
// name, or empty for one that queries no single kind.
func grafanaToolKind(name string) string {
	for _, kind := range []string{GrafanaPrometheus, GrafanaLoki, GrafanaPyroscope} {
		if strings.HasPrefix(name, "grafana_") && strings.Contains(name, "_"+kind) {
			return kind
		}
	}
	return ""
}

// datasourceField is the argument a tool takes its datasource UID under, or
// empty when its input schema has none.
func datasourceField(remote *mcp.Tool) string {
	raw, err := json.Marshal(remote.InputSchema)
	if err != nil {
		return ""
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil {
		return ""
	}
	for _, field := range grafanaDatasourceFields {
		if _, ok := schema.Properties[field]; ok {
			return field
		}
	}
	return ""
}

// pinGrafanaDatasources makes every listed tool of a kind with a configured
// datasource query that datasource, whatever the model passes. A configured
// kind no listed tool takes a datasource for is logged: it pins nothing.
func pinGrafanaDatasources(registrations []RegisteredTool, schemas map[string]*mcp.Tool, datasources map[string]string, server string, logger *slog.Logger) []RegisteredTool {
	used := map[string]bool{}
	out := make([]RegisteredTool, 0, len(registrations))
	for _, registration := range registrations {
		info, err := registration.Tool.Info(context.Background())
		invokable, ok := registration.Tool.(tool.InvokableTool)
		if err == nil && ok {
			kind := grafanaToolKind(info.Name)
			if uid := datasources[kind]; uid != "" {
				if remote := schemas[info.Name]; remote != nil && datasourceField(remote) != "" {
					field := datasourceField(remote)
					registration.Tool = &pinnedDatasourceTool{InvokableTool: invokable, field: field, uid: uid}
					used[kind] = true
				}
			}
		}
		out = append(out, registration)
	}
	for kind, uid := range datasources {
		if uid != "" && !used[kind] {
			logger.Warn("grafana datasource pins no tool: no listed tool of its kind takes a datasource UID",
				"server", server, "kind", kind)
		}
	}
	return out
}

// pinnedDatasourceTool runs a Grafana tool on one datasource: the model's
// schema has no datasource argument, and the call sets it, replacing any the
// model passed anyway.
type pinnedDatasourceTool struct {
	tool.InvokableTool
	field, uid string
}

// Info is the tool's own, without the datasource argument: the model has
// nothing to choose there.
func (t *pinnedDatasourceTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	info, err := t.InvokableTool.Info(ctx)
	if err != nil || info == nil || info.ParamsOneOf == nil {
		return info, err
	}
	parameters, err := info.ToJSONSchema()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(parameters)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if properties, ok := fields["properties"].(map[string]any); ok {
		delete(properties, t.field)
	}
	if required, ok := fields["required"].([]any); ok {
		fields["required"] = slices.DeleteFunc(required, func(name any) bool { return name == t.field })
	}
	if raw, err = json.Marshal(fields); err != nil {
		return nil, err
	}
	var trimmed jsonschema.Schema
	if err := json.Unmarshal(raw, &trimmed); err != nil {
		return nil, err
	}
	out := *info
	out.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(&trimmed)
	return &out, nil
}

func (t *pinnedDatasourceTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	var arguments map[string]json.RawMessage
	// Arguments that are not a JSON object go on as they came, for the
	// server to refuse.
	if err := json.Unmarshal([]byte(argumentsInJSON), &arguments); err == nil && arguments != nil {
		uid, _ := json.Marshal(t.uid)
		arguments[t.field] = uid
		if pinned, err := json.Marshal(arguments); err == nil {
			argumentsInJSON = string(pinned)
		}
	}
	return t.InvokableTool.InvokableRun(ctx, argumentsInJSON, opts...)
}

// GrafanaDatasourcesPrompt is the line the investigator reads about pinned
// datasources, empty when none is pinned.
func GrafanaDatasourcesPrompt(datasources map[string]string) string {
	var named []string
	for _, kind := range []struct{ key, name string }{
		{GrafanaPrometheus, "Prometheus"}, {GrafanaLoki, "Loki"}, {GrafanaPyroscope, "Pyroscope"},
	} {
		if uid := datasources[kind.key]; uid != "" {
			named = append(named, kind.name+" `"+uid+"`")
		}
	}
	if len(named) == 0 {
		return ""
	}
	return "Grafana datasources: " + strings.Join(named, ", ") +
		". Queries of these kinds always use them, and their tools take no datasource, so do not look one up for them."
}
