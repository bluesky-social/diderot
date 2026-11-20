package main

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/components/model"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askconn"
	"github.com/bluesky-social/diderot/internal/settings"
)

// askMCPTools is the Ask surface built from the tool connections that are
// on.
type askMCPTools struct {
	// root are tools of connections Ask itself uses.
	root []ask.RegisteredTool
	// investigate is the investigate_observability delegation tool, or nil
	// when no connection for the investigator attached.
	investigate *ask.RegisteredTool
	// grafana reports that investigate has a Grafana connection, which
	// PagerDuty investigations need.
	grafana bool
	// servers are the attached connections, each listed once however many
	// agents use it.
	servers []*ask.MCPToolServer
	// failed reports that a connection that is on did not attach, so the
	// replica tries again later.
	failed bool
}

func (t *askMCPTools) close() {
	for _, server := range t.servers {
		_ = server.Close()
	}
}

// familyRows is the settings cache's tool connections (*settings.Store).
type familyRows interface {
	FamilyRows(prefix string) []settings.FamilyRow
}

// liveConnections returns the tool connections that are on, as the
// settings cache holds them now, in name order, and the names of those
// whose credential can't be read, which it leaves out.
func liveConnections(store familyRows) (connections []askconn.Connection, unreadable []string) {
	for _, row := range store.FamilyRows(askconn.Prefix) {
		switch {
		case !row.Row.Enabled:
		case row.Err != nil:
			unreadable = append(unreadable, askconn.Name(row.Row.Connector))
		default:
			connections = append(connections, askconn.FromResolved(row.Resolved))
		}
	}
	return connections, unreadable
}

// connectionsVersion identifies a set of connections: their names and
// versions, so a change to any of them, or one turned on or off, changes it.
func connectionsVersion(connections []askconn.Connection) string {
	var parts []string
	for _, c := range connections {
		parts = append(parts, c.Name+"@"+strconv.FormatInt(c.Version, 10))
	}
	return strings.Join(parts, ",")
}

// grafanaDatasources are the Grafana datasources the investigator is told
// every query of their kind uses: a kind's pin when every Grafana connection
// that is on pins it to the same datasource. Each connection enforces its
// own pins, so where two disagree, or one leaves a kind to the investigator,
// no single datasource holds for that kind and none is named.
func grafanaDatasources(connections []askconn.Connection) map[string]string {
	pinned := map[string]string{}
	for _, c := range connections {
		if c.Kind != askconn.KindGrafana {
			continue
		}
		for _, kind := range slices.Sorted(maps.Keys(c.Datasources)) {
			uid := c.Datasources[kind]
			if !slices.ContainsFunc(connections, func(other askconn.Connection) bool {
				return other.Kind == askconn.KindGrafana && other.Datasources[kind] != uid
			}) {
				pinned[kind] = uid
			}
		}
	}
	return pinned
}

// grafanaDatasourcesPrompt is {{GRAFANA_DATASOURCES}} for the connections
// that are on now, empty when no datasource is pinned.
func grafanaDatasourcesPrompt(store familyRows) func() string {
	return func() string {
		connections, _ := liveConnections(store)
		return ask.GrafanaDatasourcesPrompt(grafanaDatasources(connections))
	}
}

// grafanaConnected reports that a Grafana connection for the investigator
// is on, which PagerDuty investigations need.
func grafanaConnected(store familyRows) func() bool {
	return func() bool {
		connections, _ := liveConnections(store)
		return slices.ContainsFunc(connections, func(c askconn.Connection) bool {
			return c.Kind == askconn.KindGrafana && c.ForInvestigator()
		})
	}
}

type askMCPToolsOptions struct {
	Model     model.AgenticModel
	Artifacts *ask.ArtifactStore
	Metrics   *ask.Metrics
}

// buildAskMCPTools connects every connection once and assembles the tools
// Ask registers. A connection both agents use gives the same tools, over
// the same connection, to each. A connection that cannot connect, whose
// tools fail admission, or that lists a tool an earlier connection (by
// name) already gives, is logged and left out of every agent; ordinary Ask
// remains available.
func buildAskMCPTools(ctx context.Context, limits settings.AskLimits, connections []askconn.Connection, logger *slog.Logger, opts askMCPToolsOptions) askMCPTools {
	var tools askMCPTools
	// observabilityOnly are the investigator's connections Ask does not
	// also use, which the investigator alone keeps open.
	var observability, observabilityOnly []*ask.MCPToolServer
	toolOwners := map[string]string{}
	for _, c := range connections {
		if owner := firstOwned(toolOwners, c.Tools); owner != "" {
			logger.Error("tool connection not attached: it lists a tool another connection gives; ordinary Ask remains available",
				"connection", c.Name, "other", owner)
			tools.failed = true
			continue
		}
		root := c.ForAsk()
		options := c.Options()
		options.Artifacts, options.Logger = opts.Artifacts, logger
		connected, err := ask.ConnectMCPToolServer(ctx, options)
		if err == nil && root {
			// Root tools join Ask's registry, which rejects any tool it cannot
			// present to the model (an empty description, say) and with it
			// all of Ask. Checking here leaves out only this connection, from
			// every agent it names. The investigator alone never applies that
			// check to its research tools.
			if _, _, err = ask.NewToolRegistry(ctx, connected.Tools()); err != nil {
				_ = connected.Close()
			}
		}
		if err != nil {
			logger.Error("tool connection not attached; ordinary Ask remains available",
				"connection", c.Name, "agents", c.Agents, "err", err)
			tools.failed = true
			continue
		}
		for _, tool := range c.Tools {
			toolOwners[tool] = c.Name
		}
		logger.Info("tool connection attached", "connection", c.Name, "kind", c.Kind,
			"agents", c.Agents, "tool_count", len(connected.Tools()))
		if root {
			tools.servers = append(tools.servers, connected)
			tools.root = append(tools.root, connected.Tools()...)
		}
		if c.ForInvestigator() {
			observability = append(observability, connected)
			if !root {
				observabilityOnly = append(observabilityOnly, connected)
			}
		}
	}
	if len(observability) == 0 {
		return tools
	}
	// An investigator-only connection's tools are never registered on the
	// root: the root sees only investigate_observability, and the child
	// emits no status of its own. This also keeps repeated high-cardinality
	// searches inside the child's time and iteration bounds rather than
	// spending the root's tool budget. A connection both use is on the root
	// as well.
	investigate, err := ask.NewEinoInvestigateTool(ask.InvestigateToolOptions{
		Model:        opts.Model,
		Servers:      observability,
		Artifacts:    opts.Artifacts,
		Metrics:      opts.Metrics,
		Logger:       logger.With("component", "ask-investigate"),
		SoftDeadline: limits.InvestigatorDeadline,
		Grace:        limits.InvestigatorGrace,
	})
	if err != nil {
		logger.Error("ask observability investigate tool not attached; ordinary Ask remains available", "err", err)
		for _, server := range observabilityOnly {
			_ = server.Close()
		}
		tools.failed = true
		return tools
	}
	tools.servers = append(tools.servers, observabilityOnly...)
	tools.investigate = &investigate
	childToolCount := 0
	for _, server := range observability {
		childToolCount += len(server.Tools())
		if server.Kind() == ask.MCPServerGrafana {
			tools.grafana = true
		}
	}
	logger.Info("ask observability investigate tool attached", "child_tool_count", childToolCount)
	return tools
}

// firstOwned returns the connection that already gives one of tools, or ""
// when none does.
func firstOwned(owners map[string]string, tools []string) string {
	for _, tool := range tools {
		if owner := owners[tool]; owner != "" {
			return owner
		}
	}
	return ""
}
