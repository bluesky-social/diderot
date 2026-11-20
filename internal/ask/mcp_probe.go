package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// grafanaListDatasourcesTool is the Grafana tool that lists datasources,
// which a probe calls to offer them for pinning.
const grafanaListDatasourcesTool = "grafana_list_datasources"

// MCPServerProbe is what one connection to a server found.
type MCPServerProbe struct {
	// Tools are every tool the server offers, by name.
	Tools []ProbedTool
	// Datasources are a grafana server's datasources of the kinds a pin
	// takes, when it offers its datasource list as a read-only tool.
	// DatasourcesErr says why they could not be read, nil when they were or
	// the server offers no list.
	Datasources    []GrafanaDatasource
	DatasourcesErr error
}

// ProbedTool is one tool a server offers.
type ProbedTool struct {
	Name string
	// Admission says why Ask may call it without an operator's opt-in
	// (AdmittedReviewed, AdmittedAnnotated), or that it may not
	// (AdmissionNone).
	Admission string
}

// Tool admissions.
const (
	AdmittedReviewed  = "reviewed"
	AdmittedAnnotated = "annotated"
	AdmissionNone     = "none"
)

// GrafanaDatasource is one datasource a Grafana server lists, with the pin
// kind (GrafanaPrometheus, GrafanaLoki, GrafanaPyroscope) its type serves.
type GrafanaDatasource struct {
	UID, Name, Kind string
}

// ProbeMCPServer connects to the server opts names, lists every tool it
// offers with whether Ask may call it, and, for a grafana server that offers
// its datasource list read-only, lists its datasources. Tools and the
// allowlist in opts are not checked: ConnectMCPToolServer does that.
func ProbeMCPServer(ctx context.Context, opts MCPToolServerOptions) (MCPServerProbe, error) {
	profile, ok := mcpServerProfiles[opts.Kind]
	if !ok {
		return MCPServerProbe{}, fmt.Errorf("unknown kind %q", opts.Kind)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Minute
	}
	session, err := connectMCP(ctx, opts.Endpoint, opts.Timeout, mcpHeaders(opts))
	if err != nil {
		return MCPServerProbe{}, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = session.Close() }()
	var probe MCPServerProbe
	var listDatasources bool
	for remote, err := range session.Tools(ctx, nil) {
		if err != nil {
			return MCPServerProbe{}, fmt.Errorf("list tools: %w", err)
		}
		tool := ProbedTool{Name: remote.Name, Admission: AdmissionNone}
		switch {
		case checkToolSchema(opts.Kind, remote) != nil:
		case hasKey(profile.reviewed, remote.Name):
			tool.Admission = AdmittedReviewed
		case annotatedReadOnly(remote, profile.explicitNonDestructive):
			tool.Admission = AdmittedAnnotated
		}
		if opts.Kind == MCPServerGrafana && remote.Name == grafanaListDatasourcesTool && tool.Admission != AdmissionNone {
			listDatasources = true
		}
		probe.Tools = append(probe.Tools, tool)
	}
	slices.SortFunc(probe.Tools, func(a, b ProbedTool) int { return strings.Compare(a.Name, b.Name) })
	if listDatasources {
		probe.Datasources, probe.DatasourcesErr = grafanaDatasources(ctx, session)
	}
	return probe, nil
}

// grafanaDatasources calls the Grafana server's datasource list and keeps
// the datasources a pin can name.
func grafanaDatasources(ctx context.Context, session *mcp.ClientSession) ([]GrafanaDatasource, error) {
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: grafanaListDatasourcesTool, Arguments: map[string]any{}})
	if err != nil {
		return nil, fmt.Errorf("list datasources: %w", err)
	}
	if result.IsError {
		return nil, errors.New("list datasources: the server returned an error")
	}
	if result, err = normalizeTextFirstResult(ctx, grafanaListDatasourcesTool, result); err != nil {
		return nil, err
	}
	text := result.Content[0].(*mcp.TextContent).Text
	type summary struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
		Type string `json:"type"`
	}
	var listed []summary
	if err := json.Unmarshal([]byte(text), &listed); err != nil {
		var wrapped struct {
			Datasources []summary `json:"datasources"`
		}
		if json.Unmarshal([]byte(text), &wrapped) != nil {
			return nil, errors.New("list datasources: the list could not be read")
		}
		listed = wrapped.Datasources
	}
	var datasources []GrafanaDatasource
	for _, item := range listed {
		if kind := grafanaDatasourceKind(item.Type); kind != "" && item.UID != "" {
			datasources = append(datasources, GrafanaDatasource{UID: item.UID, Name: item.Name, Kind: kind})
		}
	}
	slices.SortFunc(datasources, func(a, b GrafanaDatasource) int {
		return strings.Compare(a.Kind+"\x00"+strings.ToLower(a.Name), b.Kind+"\x00"+strings.ToLower(b.Name))
	})
	return datasources, nil
}

// grafanaDatasourceKind is the pin kind a Grafana datasource type serves, or
// "" for none.
func grafanaDatasourceKind(datasourceType string) string {
	switch datasourceType := strings.ToLower(datasourceType); {
	case datasourceType == "prometheus":
		return GrafanaPrometheus
	case datasourceType == "loki":
		return GrafanaLoki
	case strings.Contains(datasourceType, "pyroscope"), datasourceType == "phlare":
		return GrafanaPyroscope
	}
	return ""
}

func hasKey(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}
