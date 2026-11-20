package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/tool/mcp/officialmcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// diderotMCPActor is the service identity sent in a server's actor header.
const diderotMCPActor = "svc:diderot"

// MCPServerKind selects code reviewed against one upstream MCP server. The
// generic kind has none: its tools are admitted only by annotation or an
// explicit opt-in, and select no investigator prompts.
type MCPServerKind string

const (
	MCPServerGeneric MCPServerKind = "generic"
	// MCPServerGrafana is Grafana/Tempo, typically federated by a gateway. Its
	// listed tools may come from the reviewed read-only set, the alert-rules
	// tool must expose only read operations, the panel-image tool stores its
	// PNG as an Ask artifact, and the investigator gets the Grafana prompts.
	MCPServerGrafana MCPServerKind = "grafana"
	// MCPServerSentry is Sentry's hosted MCP server. Every listed tool must
	// be annotated read-only and explicitly non-destructive (or opted in),
	// and the investigator gets the Sentry prompts.
	MCPServerSentry MCPServerKind = "sentry"
)

type mcpServerProfile struct {
	// reviewed tools were reviewed as read-only, so they are admitted
	// whatever annotations the server reports.
	reviewed map[string]struct{}
	// explicitNonDestructive admits an annotated tool only when
	// destructiveHint is present and false, not merely absent.
	explicitNonDestructive bool
	normalize              func(context.Context, string, *mcp.CallToolResult) (*mcp.CallToolResult, error)
	presentation           func(server string) ToolPresentation
}

var mcpServerProfiles = map[MCPServerKind]mcpServerProfile{
	MCPServerGeneric: {normalize: normalizeTextFirstResult, presentation: genericMCPPresentation},
	MCPServerGrafana: {reviewed: reviewedGrafanaTools, normalize: normalizeTextFirstResult,
		presentation: func(string) ToolPresentation { return observabilityPresentation() }},
	MCPServerSentry: {explicitNonDestructive: true, normalize: normalizeTextFirstResult,
		presentation: func(string) ToolPresentation { return sentryPresentation() }},
}

// MCPToolServerOptions configures one operator-configured MCP server.
type MCPToolServerOptions struct {
	// Name identifies the server in logs, errors, and generic status lines.
	Name     string
	Kind     MCPServerKind
	Endpoint string
	// Token, when set, is sent as "Authorization: <AuthScheme> <Token>";
	// AuthScheme defaults to Bearer.
	Token      string
	AuthScheme string
	// ActorHeader names a header carrying Diderot's service identity. Empty
	// sends none.
	ActorHeader string
	// Tools is the allowlist: every listed tool must be served, and no other
	// tool is exposed. Required.
	Tools []string
	// AllowUnreviewedTools admits listed tools that are neither reviewed for
	// the kind nor annotated read-only, logging a warning for each. Ask calls
	// tools with this server's credential on behalf of any Ask caller, so
	// this is an explicit operator decision, never a default.
	AllowUnreviewedTools bool
	Timeout              time.Duration
	// Artifacts stores grafana_get_panel_image renders; required when a
	// grafana server lists that tool.
	Artifacts *ArtifactStore
	// GrafanaDatasources pins a grafana server's tools to one datasource per
	// kind (GrafanaPrometheus, GrafanaLoki, GrafanaPyroscope): each listed
	// tool of a kind set here queries that UID, whatever the model passes.
	// It is the same for every grafana server.
	GrafanaDatasources map[string]string
	// Logger receives the AllowUnreviewedTools warnings; nil means
	// slog.Default().
	Logger *slog.Logger
}

// MCPToolServer owns one server's streamable-HTTP session and the Ask tools
// adapted from it. Close it when Ask is detached.
type MCPToolServer struct {
	name    string
	kind    MCPServerKind
	session *mcp.ClientSession
	tools   []RegisteredTool
}

func (s *MCPToolServer) Name() string            { return s.name }
func (s *MCPToolServer) Kind() MCPServerKind     { return s.kind }
func (s *MCPToolServer) Tools() []RegisteredTool { return s.tools }

func (s *MCPToolServer) Close() error {
	if s == nil || s.session == nil {
		return nil
	}
	return s.session.Close()
}

// ConnectMCPToolServer connects to one server, checks its listed tools, and
// adapts them. Discovery is fail-closed: every listed tool must be served and
// admitted read-only, or no tool attaches.
func ConnectMCPToolServer(ctx context.Context, opts MCPToolServerOptions) (*MCPToolServer, error) {
	profile, ok := mcpServerProfiles[opts.Kind]
	if !ok {
		return nil, fmt.Errorf("ask MCP server %q: unknown kind %q", opts.Name, opts.Kind)
	}
	fail := func(err error) error { return fmt.Errorf("ask MCP server %q: %w", opts.Name, err) }
	if strings.TrimSpace(opts.Endpoint) == "" {
		return nil, fail(errors.New("endpoint is required"))
	}
	if len(opts.Tools) == 0 {
		return nil, fail(errors.New("tool allowlist is required"))
	}
	panelListed := opts.Kind == MCPServerGrafana && containsToolName(opts.Tools, PanelImageToolName)
	if panelListed && opts.Artifacts == nil {
		return nil, fail(fmt.Errorf("%s needs the artifact store (DIDEROT_IMAGES_BUCKET)", PanelImageToolName))
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Minute
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	session, err := connectMCP(ctx, opts.Endpoint, opts.Timeout, mcpHeaders(opts))
	if err != nil {
		return nil, fail(fmt.Errorf("connect: %w", err))
	}
	closeOnError := func(err error) (*MCPToolServer, error) {
		_ = session.Close()
		return nil, fail(err)
	}

	discovered := make(map[string]*mcp.Tool)
	for remote, err := range session.Tools(ctx, nil) {
		if err != nil {
			return closeOnError(fmt.Errorf("list tools: %w", err))
		}
		discovered[remote.Name] = remote
	}
	var missing []string
	for _, name := range opts.Tools {
		if _, ok := discovered[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return closeOnError(fmt.Errorf("tool allowlist is incomplete; missing %s", strings.Join(missing, ", ")))
	}
	for _, name := range opts.Tools {
		if err := checkToolSchema(opts.Kind, discovered[name]); err != nil {
			return closeOnError(err)
		}
	}
	if err := admitMCPTools(opts, profile, discovered); err != nil {
		return closeOnError(err)
	}

	ordinaryNames := make([]string, 0, len(opts.Tools))
	for _, name := range opts.Tools {
		if !panelListed || name != PanelImageToolName {
			ordinaryNames = append(ordinaryNames, name)
		}
	}
	presentation := profile.presentation(opts.Name)
	registrations := make([]RegisteredTool, 0, len(opts.Tools))
	// adapted is the metadata of the tools as adapted, which datasource
	// pinning reads, rather than the first listing.
	adapted := make(map[string]*mcp.Tool, len(opts.Tools))
	// An empty ToolNameList makes GetTools adapt every tool the server
	// lists, so a panel-only allowlist must skip it.
	if len(ordinaryNames) > 0 {
		var unreviewed []string
		ordinary, err := officialmcp.GetTools(ctx, &officialmcp.Config{
			Cli:                   session,
			ToolNameList:          ordinaryNames,
			ListToolsMode:         officialmcp.ListToolsAllPages,
			ToolCallResultHandler: profile.normalize,
			// GetTools lists the tools again and adapts that second response,
			// so the checks above are repeated on exactly the metadata it
			// adapts: a server cannot pass admission and then serve other
			// annotations or schemas. The opt-in warning follows the same
			// metadata.
			ToolNameMapper: func(_ context.Context, input officialmcp.ToolNameMapperInput) (officialmcp.ToolNameMapperOutput, error) {
				remote := input.Tool
				adapted[remote.Name] = &remote
				if err := checkToolSchema(opts.Kind, &remote); err != nil {
					return officialmcp.ToolNameMapperOutput{}, err
				}
				if !profile.admits(&remote) {
					if !opts.AllowUnreviewedTools {
						return officialmcp.ToolNameMapperOutput{}, fmt.Errorf("tool %s changed to unadmitted annotations during discovery", remote.Name)
					}
					unreviewed = append(unreviewed, remote.Name)
				}
				return officialmcp.ToolNameMapperOutput{ExposedName: remote.Name}, nil
			},
		})
		if err != nil {
			return closeOnError(fmt.Errorf("adapt tools: %w", err))
		}
		if len(ordinary) != len(ordinaryNames) {
			return closeOnError(errors.New("tool allowlist changed during discovery"))
		}
		for _, name := range unreviewed {
			logger.Warn("ask MCP tool exposed without review or a read-only annotation; it runs with the server's credential for any Ask caller",
				"server", opts.Name, "tool", name)
		}
		for _, adapted := range ordinary {
			registrations = append(registrations, RegisteredTool{Tool: adapted, Presentation: presentation})
		}
	}
	if panelListed {
		registrations = append(registrations, RegisteredTool{
			Tool:         &panelImageMCPTool{session: session, store: opts.Artifacts, description: discovered[PanelImageToolName].Description},
			Presentation: presentation,
		})
	}
	if opts.Kind == MCPServerGrafana && len(opts.GrafanaDatasources) > 0 {
		registrations = pinGrafanaDatasources(registrations, adapted, opts.GrafanaDatasources, opts.Name, logger)
	}
	return &MCPToolServer{name: opts.Name, kind: opts.Kind, session: session, tools: registrations}, nil
}

// mcpHeaders are the headers every request to the server carries: its
// credential, and Diderot's identity in the actor header.
func mcpHeaders(opts MCPToolServerOptions) map[string]string {
	headers := map[string]string{}
	if opts.Token != "" {
		scheme := opts.AuthScheme
		if scheme == "" {
			scheme = "Bearer"
		}
		headers["Authorization"] = scheme + " " + opts.Token
	}
	if opts.ActorHeader != "" {
		// A gateway may federate Zendesk alongside the observability
		// targets. Zendesk requires an authenticated actor even during the
		// shared tools/list fanout; this service identity keeps discovery
		// healthy without claiming to be an end user.
		headers[opts.ActorHeader] = diderotMCPActor
	}
	return headers
}

// admitMCPTools refuses any listed tool that is neither reviewed for the
// server's kind nor annotated read-only, unless the operator opted in to
// unreviewed tools. A reviewed tool is admitted whatever its annotations say:
// the review, not the server's self-description, established it as
// read-only. The opt-in warning is logged where tools are adapted, from the
// metadata actually exposed.
func admitMCPTools(opts MCPToolServerOptions, profile mcpServerProfile, discovered map[string]*mcp.Tool) error {
	if opts.AllowUnreviewedTools {
		return nil
	}
	var refused []string
	for _, name := range opts.Tools {
		if !profile.admits(discovered[name]) {
			refused = append(refused, name)
		}
	}
	if len(refused) == 0 {
		return nil
	}
	sort.Strings(refused)
	annotation := "readOnlyHint without destructiveHint"
	if profile.explicitNonDestructive {
		annotation = "readOnlyHint with destructiveHint false"
	}
	return fmt.Errorf("tools %s are not reviewed read-only tools and are not annotated %s; refusing them unless the connection allows tools not marked read-only",
		strings.Join(refused, ", "), annotation)
}

// admits reports whether remote is reviewed for the kind or annotated
// read-only, before any opt-in.
func (p mcpServerProfile) admits(remote *mcp.Tool) bool {
	if _, reviewed := p.reviewed[remote.Name]; reviewed {
		return true
	}
	return annotatedReadOnly(remote, p.explicitNonDestructive)
}

// checkToolSchema applies a kind's per-tool schema review: a grafana
// server's alert-rules tool must expose only read operations.
func checkToolSchema(kind MCPServerKind, remote *mcp.Tool) error {
	if kind == MCPServerGrafana && remote.Name == alertRulesToolName {
		return validateReadOnlyAlertSchema(remote)
	}
	return nil
}

// annotatedReadOnly reports whether the server marks the tool read-only and
// not destructive. ReadOnlyHint is a plain bool in the SDK, so an absent hint
// reads as false and fails closed. explicit also requires destructiveHint to
// be present.
func annotatedReadOnly(remote *mcp.Tool, explicit bool) bool {
	if remote == nil || remote.Annotations == nil || !remote.Annotations.ReadOnlyHint {
		return false
	}
	destructive := remote.Annotations.DestructiveHint
	if destructive == nil {
		return !explicit
	}
	return !*destructive
}

// normalizeTextFirstResult keeps the server's human-readable text when it is
// present and uses structured content only as a fallback. MCP servers are
// expected to return equivalent text alongside structured content, so feeding
// both to the model would duplicate potentially large results.
func normalizeTextFirstResult(_ context.Context, name string, result *mcp.CallToolResult) (*mcp.CallToolResult, error) {
	if result == nil {
		return nil, fmt.Errorf("%s returned no MCP result", name)
	}
	texts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		switch typed := content.(type) {
		case *mcp.TextContent:
			texts = append(texts, typed.Text)
		default:
			return nil, fmt.Errorf("%s returned unsupported MCP content %T", name, content)
		}
	}
	if len(texts) == 0 && result.StructuredContent != nil {
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("%s returned unserializable structured content: %w", name, err)
		}
		texts = append(texts, string(raw))
	}
	if len(texts) == 0 {
		return nil, fmt.Errorf("%s returned no text or structured content", name)
	}
	result.Content = []mcp.Content{&mcp.TextContent{Text: strings.Join(texts, "\n")}}
	result.StructuredContent = nil
	return result, nil
}

func sentryPresentation() ToolPresentation {
	return ToolPresentation{
		Status: func(json.RawMessage) StatusLine {
			return StatusLine{Emoji: "mag", Verb: "searching Sentry"}
		},
		StatusOutput: func(json.RawMessage) string { return "Sentry result received" },
	}
}

func genericMCPPresentation(server string) ToolPresentation {
	return ToolPresentation{
		Status: func(json.RawMessage) StatusLine {
			return StatusLine{Emoji: "electric_plug", Verb: "querying " + server}
		},
		StatusOutput: func(json.RawMessage) string { return server + " result received" },
	}
}
