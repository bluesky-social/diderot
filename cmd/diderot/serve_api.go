package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/adminui"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/config"
	notioncx "github.com/bluesky-social/diderot/internal/connector/notion"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/scheduledreport"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
)

// operatorServices is what the API and the browser UI act through. Each is a database operation, so every replica serves them,
// whether or not it leads and whether or not their connector is enabled.
type operatorServices struct {
	setup          *setup.Service
	sessionSummary sessioningest.SummaryControl
	// sessionPrivacy is agent-session privacy self-service, nil when the
	// session ingest policy is invalid.
	sessionPrivacy *sessioningest.PrivacyService
	// reports are the scheduled report definitions and manual triggers;
	// only the leader drains the runs they queue.
	reports     *scheduledreport.Store
	notionRoots *notioncx.RootsStore
	// sourceDeps is what sources' pages read their status and request
	// backfills through.
	sourceDeps source.Deps
	// scopes resolves a caller's email to the private Slack channels their
	// Slack identity belongs to, for every route that reads content
	// (retrieve, list, Ask, expand, MCP, the admin pages' images);
	// without it a group member could read any private channel diderot
	// indexes. It asks Slack with the saved bot token, through its own
	// client and limiter; while Slack is off only the baseline scopes and
	// the cached workspace-public channels resolve.
	scopes *liveScopeResolver
	// slack is the admin Slack controls and the operator notifier, built
	// from the saved Slack settings.
	slack *liveSlackAdmin
}

func newOperatorServices(cfg *config.Config, pool *pgxpool.Pool, sink *events.DBSink, connectors connectorSet, catalog *modelCatalog, auth *serverAuth, logger *slog.Logger) operatorServices {
	ops := operatorServices{
		setup: setup.New(pool, connectors.store, connectors.registries,
			setupFacts(cfg, catalog, auth, connectors.summaries), catalog, newModelTester(cfg, catalog)),
		sessionSummary: connectors.summaries,
		sessionPrivacy: newSessionPrivacy(cfg, pool, logger),
		reports:        scheduledreport.NewStore(pool),
		notionRoots:    notioncx.NewRootsStore(pool),
		sourceDeps:     source.Deps{Pool: pool, Sink: sink, Logger: logger.With("component", "sources")},
		scopes: &liveScopeResolver{
			store: connectors.store, pool: pool, publicScopes: connectors.publicScopes,
			logger: logger.With("component", "userscopes"),
		},
		slack: &liveSlackAdmin{
			store: connectors.store, pool: pool, publicURL: cfg.PublicURL,
			logger: logger.With("component", "slack-admin"),
		},
	}
	ops.notionRoots.SetHider(notioncx.NewEventSinkAdapter(sink, pool))
	return ops
}

// newSessionPrivacy is agent-session privacy self-service, or nil, with the
// reason logged, when the session ingest policy is invalid.
func newSessionPrivacy(cfg *config.Config, pool *pgxpool.Pool, logger *slog.Logger) *sessioningest.PrivacyService {
	policy, err := configuredSessionIngestPolicy(cfg)
	if err != nil {
		logger.Error("session privacy self-service disabled: invalid base policy", "err", err)
		return nil
	}
	privacy, err := sessioningest.NewPrivacyService(sessioningest.NewStore(pool), policy)
	if err != nil {
		logger.Error("session privacy self-service disabled", "err", err)
		return nil
	}
	return privacy
}

// mountAdminAPI builds the /v1 API and mounts its routes on mux. Every
// replica serves it. The replica's Ask is attached once the listeners run
// (startReplicaAsk); until it is built, Ask answers 503. The browser UI acts
// through the same server's Slack, re-embed and image services.
func mountAdminAPI(mux *http.ServeMux, cfg *config.Config, auth *serverAuth, authorizer *authorization.Authorizer,
	connectors connectorSet, ops operatorServices, readers corpusReaders, pool *pgxpool.Pool, logger *slog.Logger,
) (*adminapi.Server, error) {
	admin := adminapi.NewServer(auth.api, logger.With("component", "adminapi"))
	admin.SetSlackControls(ops.slack.Controls)
	// HTTP and MCP queries have no Slack surface of their own; the
	// operators are DMed about them.
	admin.SetAskInvocationNotifier(ops.slack)
	admin.SetAskGate(connectors.store)
	admin.SetAuthorizer(authorizer)
	admin.SetAdminGroups(cfg.AdminGroups)
	admin.SetSelfServiceGroups(cfg.SelfServiceGroups)
	if connectors.pushHandler != nil {
		admin.SetSessionPush(connectors.pushHandler)
	}
	if ops.sessionPrivacy != nil {
		admin.SetSessionPrivacyService(ops.sessionPrivacy)
	}
	if readers.retriever != nil {
		admin.SetRetriever(readers.retriever)
		logger.Info("retrieve endpoint attached")
	}
	admin.SetStatusPool(pool)
	admin.SetExpander(readers.expander)
	admin.SetScopeResolver(ops.scopes)
	admin.SetScopeGrants(sourceScopeGrants{
		sources: connectors.registries.Sources, deps: ops.sourceDeps, logger: logger.With("component", "scope-grants"),
	})
	admin.SetAskerResolver(ops.scopes)
	if err := admin.Mount(mux); err != nil {
		logger.Error("API mount", "err", err)
		return nil, err
	}
	return admin, nil
}

// mountBrowserUI builds the administrator pages (/admin) and the session
// privacy pages (/sessions), mounts them on mux, and starts warming the
// overview's cache.
func mountBrowserUI(ctx context.Context, mux *http.ServeMux, cfg *config.Config, auth *serverAuth,
	connectors connectorSet, ops operatorServices, admin *adminapi.Server, pool *pgxpool.Pool,
	captionModel func() string, logger *slog.Logger,
) error {
	// A nil *PrivacyService in the interface would read as self-service on.
	var sessionPrivacy adminui.SessionPrivacyService
	if ops.sessionPrivacy != nil {
		sessionPrivacy = ops.sessionPrivacy
	}
	ui, err := adminui.New(adminui.Config{
		Pool:                  pool,
		Logger:                logger.With("component", "adminui"),
		Version:               Version,
		Authenticator:         auth.browser,
		AdminGroups:           cfg.AdminGroups,
		SelfServiceGroups:     cfg.SelfServiceGroups,
		PublicURL:             cfg.PublicURL,
		SlackWorkspaceHost:    slackWorkspaceHost(connectors.store),
		Connectors:            connectors.store,
		Connections:           connectors.store,
		Providers:             connectors.store,
		SessionViewerBaseURL:  connectors.sessionFeed.ViewerBaseURL,
		UsageClient:           connectors.sessionFeed,
		ReembedLauncher:       admin,
		ReembedSources:        reembedSources(),
		CaptionModel:          captionModel,
		SessionSummaryControl: ops.sessionSummary,
		ScheduledReportsStore: ops.reports,
		NotionRootsStore:      ops.notionRoots,
		SlackSourceControl:    admin,
		Images:                admin,
		SessionPrivacy:        sessionPrivacy,
		ToolWording:           toolWording(connectors.store),
		Registries:            connectors.registries,
		SourceDeps:            ops.sourceDeps,
		Setup:                 ops.setup,
		Registry:              prometheus.DefaultRegisterer,

		// The browser sign-in's development opt-in covers the UI's own
		// CSRF cookies too; adminui rejects it off literal loopback.
		AllowInsecureLoopbackCookies: cfg.BrowserSessionInsecureCookie,
	})
	if err != nil {
		return err
	}
	if err := ui.Mount(mux); err != nil {
		return err
	}
	go ui.WarmOverview(ctx)
	logger.Info("administrator and session privacy web UI mounted",
		"admin_groups", cfg.AdminGroups, "self_service_groups", cfg.SelfServiceGroups)
	return nil
}

// mcpResourceMetadata is /mcp's OAuth 2.0 protected resource metadata in
// oidc mode: the endpoint as people reach it, the issuer whose access tokens
// it takes, and the scopes that make the issuer put the identity claims in
// them (offline_access is the client's business, not the resource's). Other
// modes take no OAuth token and publish none.
func mcpResourceMetadata(cfg *config.Config) *oauthex.ProtectedResourceMetadata {
	if cfg.AuthMode != config.AuthModeOIDC || cfg.PublicURL == "" {
		return nil
	}
	return &oauthex.ProtectedResourceMetadata{
		Resource:               cfg.PublicURL + "/mcp",
		AuthorizationServers:   []string{cfg.OIDCIssuer},
		ScopesSupported:        cfg.OIDCScopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Diderot",
	}
}

// mountCorpusMCP mounts the agent-facing corpus surface, streamable HTTP MCP
// at /mcp, whenever retrieval is on. A caller authenticates as on /v1, with
// the bearer the auth mode accepts; with DIDEROT_GATEWAY_API_KEY set, a
// gateway's backend hop may instead present that key, and the caller's
// identity then comes separately in trusted-proxy headers. Its ask tool runs
// on the replica's Ask, through admin, and its attachment tool reads image
// bytes from mirror, which is nil without object storage.
func mountCorpusMCP(ctx context.Context, mux *http.ServeMux, cfg *config.Config, auth *serverAuth,
	sources source.Registry, readers corpusReaders, admin *adminapi.Server, pool *pgxpool.Pool, mirror files.S3Mirror, logger *slog.Logger,
) error {
	if readers.retriever == nil {
		logger.Warn("corpus MCP disabled (needs a configured retriever)")
		return nil
	}
	// sink is the ScopeLookup: expand and attachment check events.scope
	// before any content fetch, as /v1/expand-chunk does, and retrieve
	// resolves explicit handles in its question through the resolver Ask
	// uses.
	corpusMCP, err := ask.NewCorpusMCPServer(readers.retriever, sources.Infos(), readers.expander, readers.sink,
		attachmentReader{pool: pool, mirror: mirror},
		readers.references, readers.linearTeams, admin.MCPAskRunner, pool, ctx, Version)
	if err != nil {
		return fmt.Errorf("build corpus MCP server: %w", err)
	}
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return corpusMCP },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger.With("component", "corpus-mcp")})
	if err := admin.MountMCP(mux, transport, cfg.GatewayAPIKey, auth.mcp, mcpResourceMetadata(cfg)); err != nil {
		return err
	}
	logger.Info("corpus MCP mounted", "path", "/mcp", "tools", "ask,attachment,expand,list,retrieve",
		"gateway_key", cfg.GatewayAPIKey != "")
	return nil
}
