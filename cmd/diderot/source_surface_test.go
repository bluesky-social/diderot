package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// The source surface goldens record what models, the progress line, and the
// admin pages see of each source. A diff is a behavior change: regenerate
// with DIDEROT_UPDATE_GOLDEN=1 only for an intended one.
const (
	sourceSurfaceGoldenPath  = "testdata/source_surface.golden.json"
	corpusMCPToolsGoldenPath = "testdata/corpus_mcp_tools.golden.json"
)

// surfaceSources are the event sources the goldens exercise: every source
// today, and one no build registers.
var surfaceSources = []string{"slack", "agent_session", "notion", "linear", "zendesk", "imap", "caldav", "legacy_source"}

type actorCase struct {
	Source    string         `json:"source"`
	Display   string         `json:"display"`
	Canonical string         `json:"canonical"`
	SourceID  string         `json:"source_id"`
	Actor     identity.Actor `json:"actor"`
}

type sourceSurface struct {
	AskTools []goldenTool `json:"ask_tools"`
	// Labels are the progress line's names for a hit with no title or
	// channel, and for an expansion, by event source.
	RetrieveLabels map[string]string `json:"retrieve_labels"`
	ListLabels     map[string]string `json:"list_labels"`
	ExpandStatus   map[string]string `json:"expand_status"`
	Actors         []actorCase       `json:"actors"`
	// Connectors are how pages order and name connectors.
	SourceConnectors []string          `json:"source_connectors"`
	ToolConnectors   []string          `json:"tool_connectors"`
	ConnectorTitles  map[string]string `json:"connector_titles"`
	// SourcesPrompt is what {{SOURCES}} renders to.
	SourcesPrompt string `json:"sources_prompt"`
}

func checkGolden(t *testing.T, path string, got any) {
	t.Helper()
	encoded, err := json.MarshalIndent(got, "", "  ")
	require.NoError(t, err)
	encoded = append(encoded, '\n')
	if os.Getenv("DIDEROT_UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, encoded, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(encoded))
}

func statusLabel(t *testing.T, presentation ask.ToolPresentation, source string) string {
	t.Helper()
	output, err := json.Marshal(map[string]any{"hits": []map[string]any{{"source": source, "url": "https://example.test/hit"}}})
	require.NoError(t, err)
	sources := presentation.StatusSources(output)
	require.Len(t, sources, 1)
	return sources[0].Label
}

func TestSourceSurfaceMatchesGolden(t *testing.T) {
	ctx := t.Context()
	registries := setup.Registries{Sources: sourceRegistry(nil, nil, nil, nil, nil, nil, false), Tools: toolRegistry(), Automations: automationRegistry(&config.Config{}, nil)}
	infos := registries.Sources.Infos()
	retrieveTool, listTool, expandTool := ask.NewEinoRetrieveTool(nil, infos), ask.NewEinoListTool(nil, infos), ask.NewEinoExpandTool(nil, nil, infos)
	sourceConnectors := registries.Sources.Names()
	got := sourceSurface{
		RetrieveLabels: map[string]string{}, ListLabels: map[string]string{}, ExpandStatus: map[string]string{},
		SourceConnectors: sourceConnectors, ToolConnectors: registries.Names()[len(sourceConnectors):],
		ConnectorTitles: map[string]string{}, SourcesPrompt: infos.PromptList(),
	}
	for _, registration := range []ask.RegisteredTool{retrieveTool, listTool, expandTool} {
		got.AskTools = append(got.AskTools, toolGolden(t, ctx, registration))
	}
	for _, source := range surfaceSources {
		got.RetrieveLabels[source] = statusLabel(t, retrieveTool.Presentation, source)
		got.ListLabels[source] = statusLabel(t, listTool.Presentation, source)
		output, err := json.Marshal(map[string]any{"source": source, "supported": true})
		require.NoError(t, err)
		got.ExpandStatus[source] = expandTool.Presentation.StatusOutput(output)
	}
	for _, c := range []actorCase{
		{Source: "slack", Display: "Alex", Canonical: "alex@example.com", SourceID: "U1"},
		{Source: "slack", Display: "Alex", Canonical: "slack:U1", SourceID: "U1"},
		{Source: "slack", Display: "Deploy bot", Canonical: "slack-bot:B1", SourceID: "B1"},
		{Source: "slack", Display: "legacy", Canonical: "", SourceID: ""},
		{Source: "agent_session", Display: "alex@example.com", Canonical: "alex@example.com", SourceID: "alex@example.com"},
		{Source: "agent_session", Display: "worker", Canonical: "svc:worker@example.com", SourceID: "svc:worker@example.com"},
		{Source: "notion", Display: "Alex", Canonical: "alex@example.com", SourceID: "N1"},
		{Source: "notion", Display: "Alex", Canonical: "notion:N1", SourceID: "N1"},
		{Source: "notion", Display: "Integration", Canonical: "notion-bot:NB1", SourceID: "NB1"},
		{Source: "linear", Display: "Alex", Canonical: "alex@example.com", SourceID: "L1"},
		{Source: "linear", Display: "Alex", Canonical: "linear:L1", SourceID: "L1"},
		{Source: "linear", Display: "", Canonical: "linear:unknown", SourceID: ""},
		{Source: "zendesk", Display: "Agent", Canonical: "Agent@example.com", SourceID: "Z1"},
		{Source: "zendesk", Display: "Requester", Canonical: "zendesk:Z2", SourceID: "Z2"},
		{Source: "imap", Display: "Alex", Canonical: "alex@example.com", SourceID: "alex@example.com"},
		{Source: "legacy_source", Display: "alex@example.com", Canonical: "alex@example.com", SourceID: "alex@example.com"},
		{Source: "legacy_source", Display: "opaque", Canonical: "opaque-value", SourceID: "X1"},
	} {
		c.Actor = identity.FromEvent(infos.Of(c.Source), c.Display, c.Canonical, c.SourceID)
		got.Actors = append(got.Actors, c)
	}
	for _, name := range append(registries.Names(), "example") {
		got.ConnectorTitles[name] = registries.Title(name)
	}
	checkGolden(t, sourceSurfaceGoldenPath, got)
}

// TestCorpusMCPToolListMatchesGolden pins the corpus MCP server's tools/list
// result byte for byte, apart from indentation.
func TestCorpusMCPToolListMatchesGolden(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "cmd_source_surface"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	teams, err := ask.NewLinearTeamCache(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	expander := newSourceExpander(sourceRegistry(pool, nil, nil, nil, nil, nil, false), source.Deps{Pool: pool, Sink: sink})
	server, err := ask.NewCorpusMCPServer(nil, sourceRegistry(pool, nil, nil, nil, nil, nil, false).Infos(), expander, sink, attachmentReader{pool: pool}, newSourceReferenceResolver(sink, expander), teams,
		func() ask.CorpusAskLease { return ask.CorpusAskLease{} }, pool, ctx, "test")
	require.NoError(t, err)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{
		"_meta": map[string]any{
			mcp.MetaKeyProtocolVersion:    "2026-07-28",
			mcp.MetaKeyClientInfo:         map[string]any{"name": "test-client", "version": "1"},
			mcp.MetaKeyClientCapabilities: map[string]any{},
		},
	}})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("MCP-Method", "tools/list")
	resp, err := httpServer.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var decoded struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded), string(raw))
	require.NotEmpty(t, decoded.Result.Tools, string(raw))
	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, decoded.Result.Tools, "", "  "))
	indented.WriteByte('\n')
	if os.Getenv("DIDEROT_UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(corpusMCPToolsGoldenPath, indented.Bytes(), 0o644))
	}
	want, err := os.ReadFile(corpusMCPToolsGoldenPath)
	require.NoError(t, err)
	require.Equal(t, string(want), indented.String())
}
