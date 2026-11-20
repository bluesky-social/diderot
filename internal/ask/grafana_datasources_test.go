package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// recordingTool is a Grafana tool that records the arguments it is called
// with.
type recordingTool struct {
	name      string
	arguments string
}

func (t *recordingTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name, Desc: t.name}, nil
}

func (t *recordingTool) InvokableRun(_ context.Context, arguments string, _ ...tool.Option) (string, error) {
	t.arguments = arguments
	return "ok", nil
}

func remoteTakingDatasource(name, field string) *mcp.Tool {
	properties := map[string]any{"expr": map[string]any{"type": "string"}}
	if field != "" {
		properties[field] = map[string]any{"type": "string"}
	}
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object", "properties": properties}}
}

// TestPinnedGrafanaDatasources: a tool of a kind with a pinned datasource
// queries it whatever the model passes, under the argument name its schema
// uses; other tools, and arguments that are not an object, pass unchanged.
func TestPinnedGrafanaDatasources(t *testing.T) {
	prometheus := &recordingTool{name: "grafana_query_prometheus"}
	pyroscope := &recordingTool{name: "grafana_query_pyroscope"}
	loki := &recordingTool{name: "grafana_query_loki_logs"}
	tempo := &recordingTool{name: "tempo_traceql-search"}
	discovered := map[string]*mcp.Tool{
		prometheus.name: remoteTakingDatasource(prometheus.name, "datasourceUid"),
		pyroscope.name:  remoteTakingDatasource(pyroscope.name, "data_source_uid"),
		loki.name:       remoteTakingDatasource(loki.name, "datasourceUid"),
		tempo.name:      remoteTakingDatasource(tempo.name, ""),
	}
	var logs bytes.Buffer
	pinned := pinGrafanaDatasources(
		[]RegisteredTool{{Tool: prometheus}, {Tool: pyroscope}, {Tool: loki}, {Tool: tempo}}, discovered,
		map[string]string{GrafanaPrometheus: "prom-main", GrafanaPyroscope: "profiles", "tempo": "ignored"},
		"grafana", slog.New(slog.NewTextHandler(&logs, nil)))
	run := func(index int, arguments string) {
		t.Helper()
		_, err := pinned[index].Tool.(tool.InvokableTool).InvokableRun(context.Background(), arguments)
		require.NoError(t, err)
	}

	run(0, `{"datasourceUid":"guessed","expr":"up","step":15}`)
	require.JSONEq(t, `{"datasourceUid":"prom-main","expr":"up","step":15}`, prometheus.arguments)
	run(0, `{"expr":"up"}`)
	require.JSONEq(t, `{"datasourceUid":"prom-main","expr":"up"}`, prometheus.arguments, "a missing datasource is filled in")
	run(0, `not json`)
	require.Equal(t, `not json`, prometheus.arguments, "arguments that are not an object go on as they came")
	run(1, `{"data_source_uid":"other","profile_type":"cpu"}`)
	require.JSONEq(t, `{"data_source_uid":"profiles","profile_type":"cpu"}`, pyroscope.arguments)
	run(2, `{"datasourceUid":"chosen"}`)
	require.JSONEq(t, `{"datasourceUid":"chosen"}`, loki.arguments, "a kind without a pinned datasource is the model's choice")
	run(3, `{"query":"{}"}`)
	require.JSONEq(t, `{"query":"{}"}`, tempo.arguments)

	require.Contains(t, logs.String(), "kind=tempo", "a pin no listed tool uses is logged")
	require.NotContains(t, logs.String(), "kind=prometheus")
}

// schemaTool is a Grafana tool whose schema takes a datasource.
type schemaTool struct{ recordingTool }

func (t *schemaTool) Info(context.Context) (*schema.ToolInfo, error) {
	var parameters jsonschema.Schema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{
		"datasourceUid":{"type":"string","description":"The datasource."},
		"expr":{"type":"string"}},"required":["datasourceUid","expr"]}`), &parameters); err != nil {
		return nil, err
	}
	return &schema.ToolInfo{Name: t.name, Desc: t.name, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&parameters)}, nil
}

// TestPinnedGrafanaDatasourceLeavesTheModelsSchema: the model is offered a
// pinned tool without its datasource argument, which the call still sets.
func TestPinnedGrafanaDatasourceLeavesTheModelsSchema(t *testing.T) {
	prometheus := &schemaTool{recordingTool{name: "grafana_query_prometheus"}}
	pinned := pinGrafanaDatasources([]RegisteredTool{{Tool: prometheus}},
		map[string]*mcp.Tool{prometheus.name: remoteTakingDatasource(prometheus.name, "datasourceUid")},
		map[string]string{GrafanaPrometheus: "prom-main"}, "grafana", slog.New(slog.NewTextHandler(io.Discard, nil)))
	info, err := pinned[0].Tool.Info(context.Background())
	require.NoError(t, err)
	parameters, err := info.ToJSONSchema()
	require.NoError(t, err)
	raw, err := json.Marshal(parameters)
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"]}`, string(raw))
	require.Equal(t, prometheus.name, info.Name)

	_, err = pinned[0].Tool.(tool.InvokableTool).InvokableRun(context.Background(), `{"expr":"up"}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"datasourceUid":"prom-main","expr":"up"}`, prometheus.arguments)
}

func TestGrafanaDatasourcesPrompt(t *testing.T) {
	require.Empty(t, GrafanaDatasourcesPrompt(nil))
	require.Equal(t,
		"Grafana datasources: Prometheus `prom-main`, Pyroscope `profiles`. Queries of these kinds always use them, and their tools take no datasource, so do not look one up for them.",
		GrafanaDatasourcesPrompt(map[string]string{GrafanaPyroscope: "profiles", GrafanaPrometheus: "prom-main"}))
}
