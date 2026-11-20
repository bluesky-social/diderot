// Package adminui serves Diderot's browser interfaces: the administrator
// pages and the strictly owner-bound agent-session privacy page.
//
// It intentionally keeps to a small web stack: net/http, html/template,
// embedded assets, ordinary forms, and bounded Postgres queries. JSON API
// handlers and browser handlers share domain stores but have separate
// authentication contracts.
package adminui

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/diderot/internal/adminaction"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Config contains the process-owned dependencies for the browser UI.
type Config struct {
	Pool    *pgxpool.Pool
	Logger  *slog.Logger
	Version string
	// Authenticator identifies browser callers. Required. The default reads
	// trusted-proxy identity headers, which identify nobody when no proxy
	// address is configured. When it is also a BrowserLogin (the browser's
	// own sign-in: OIDC, the bootstrap admin, or a static token), the UI
	// serves the sign-in routes under /auth/ too.
	Authenticator auth.Authenticator
	AdminGroups   []string
	// SelfServiceGroups is the group allowlist for the owner-bound
	// /sessions surface. It is independent of administrator access.
	SelfServiceGroups []string
	// PublicURL is the browser-reachable Diderot origin. Mutation handlers
	// compare Origin/Referer against it instead of the proxy-facing request
	// Host, which may be an internal service name.
	PublicURL string
	// AllowInsecureLoopbackCookies is for session-dev and the browser
	// sign-in's DIDEROT_BROWSER_SESSION_INSECURE_COOKIE. The default remains
	// Secure cookies, and enabling it for any non-loopback HTTP origin is
	// rejected during construction.
	AllowInsecureLoopbackCookies bool
	// SlackWorkspaceHost returns the saved Slack workspace host (for example
	// example.slack.com), empty while Slack is off. It builds links back to
	// a persisted ask thread. Nil builds none.
	SlackWorkspaceHost func() string
	// Connectors backs /admin/connectors. Nil leaves those pages answering
	// 503.
	Connectors ConnectorSettings
	// Connections backs the tool connections' pages. Nil leaves them
	// answering 503 and the Connectors page without them.
	Connections ConnectionSettings
	// Providers backs the model providers' pages (the same
	// *settings.Store). Nil leaves them answering 503 and the Models page
	// without them.
	Providers ConnectionSettings
	// SessionViewerBaseURL returns the session viewer's public origin for
	// agent-session viewer links, empty while the session feed connector is
	// off. Nil builds none.
	SessionViewerBaseURL func() string
	// UsageClient supplies authoritative cost and token accounting for
	// ask-session detail pages. Nil leaves usage hidden.
	UsageClient WorkUsageClient
	// ReembedLauncher records a re-embed request (*adminapi.Server).
	ReembedLauncher adminaction.ReembedLauncher
	// ReembedSources are the event sources a re-embed can rebuild.
	ReembedSources []string
	// CaptionModel returns the model that describes images now, empty while
	// image captions are off. Nil hides the control that describes images
	// again.
	CaptionModel          func() string
	SessionSummaryControl SessionSummaryControl
	// ScheduledReportsStore is the shared durable definition/run store used by
	// the API and background worker. Nil renders an unavailable read-only page.
	ScheduledReportsStore ScheduledReportsStore
	// NotionRootsStore exposes the same managed-root records used by the worker
	// and operator API. Nil leaves the Notion inventory empty.
	NotionRootsStore   NotionRootsStore
	SlackSourceControl SlackSourceControl
	// Images serves the Slack images an Ask session page embeds
	// (*adminapi.Server). Nil leaves them answering 404.
	Images         ImageReader
	SessionPrivacy SessionPrivacyService
	// ToolWording returns the organization wording as a run would see it now,
	// so the prompt view shows tool descriptions as the model receives them.
	// Nil is the neutral wording.
	ToolWording func() ask.Wording
	// Registries are the connectors this build registers: the connectors
	// pages list and name them by kind, and the corpus tools on the prompts
	// page filter by its sources.
	Registries setup.Registries
	// SourceDeps is what a source's page reads its status through.
	SourceDeps source.Deps
	// Setup backs /admin/settings and its pages, and the setup banner on
	// every admin page. Nil leaves those pages answering
	// 503 and shows no banner.
	Setup *setup.Service
	// Registry receives bounded page/action request metrics. Nil uses an
	// isolated registry so tests and embedded callers do not mutate globals.
	Registry prometheus.Registerer
}

// WorkUsageClient is the narrow session feed work-usage surface used by the
// browser UI to render authoritative metered accounting for ask-session
// detail pages.
type WorkUsageClient interface {
	GetWorkUsage(context.Context, string, string) (*sessionfeed.Usage, error)
}

type SessionSummaryControl interface {
	State(context.Context) (sessioningest.SummaryControlState, error)
	SetEnabled(context.Context, bool) error
}

type SessionPrivacyService interface {
	SetSessions(context.Context, string, []sessioningest.SessionRef, bool) (sessioningest.SessionPrivacyUpdate, error)
	ListSessions(context.Context, string, int, int64) (sessioningest.OwnerSessionPage, error)
}

// Server owns the HTML routes. Domain-specific stores are added as narrow
// interfaces by each page rather than turning this into a second admin API.
type Server struct {
	// overview holds the corpus totals the Overview shows.
	overview            overviewCache
	pool                *pgxpool.Pool
	logger              *slog.Logger
	version             string
	authenticator       auth.Authenticator
	login               BrowserLogin
	adminGroups         map[string]struct{}
	selfGroups          map[string]struct{}
	publicOrigin        string
	sessionCookieSecure bool
	slackHost           func() string
	connectors          ConnectorSettings
	connections         ConnectionSettings
	providers           ConnectionSettings
	viewerBaseURL       func() string
	usageClient         WorkUsageClient
	reembed             adminaction.ReembedLauncher
	reembedSources      []string
	captionModel        func() string
	summaryControl      SessionSummaryControl
	reports             ScheduledReportsStore
	notionRoots         NotionRootsStore
	sourceControl       SlackSourceControl
	images              ImageReader
	sessionPrivacy      SessionPrivacyService
	toolWording         func() ask.Wording
	registries          setup.Registries
	sourceDeps          source.Deps
	setup               *setup.Service
	metrics             *adminMetrics
}

// New constructs the UI server. An empty group allowlist is rejected: the
// UI indexes private operational data and must never degrade to
// "authenticated user" access.
func New(cfg Config) (*Server, error) {
	if cfg.Pool == nil {
		return nil, fmt.Errorf("adminui: database pool is required")
	}
	if cfg.Authenticator == nil {
		return nil, fmt.Errorf("adminui: browser authenticator is required")
	}
	login, _ := cfg.Authenticator.(BrowserLogin)
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Registry == nil {
		cfg.Registry = prometheus.NewRegistry()
	}
	if cfg.SessionSummaryControl == nil {
		cfg.SessionSummaryControl = sessioningest.NewSummaryControl(sessioningest.NewStore(cfg.Pool), nil)
	}
	groups := make(map[string]struct{}, len(cfg.AdminGroups))
	for _, group := range cfg.AdminGroups {
		if group = strings.TrimSpace(group); group != "" {
			groups[group] = struct{}{}
		}
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("adminui: at least one administrator group is required")
	}
	selfGroups := make(map[string]struct{}, len(cfg.SelfServiceGroups))
	for _, group := range cfg.SelfServiceGroups {
		if group = strings.TrimSpace(group); group != "" {
			selfGroups[group] = struct{}{}
		}
	}
	publicURL := strings.TrimSpace(cfg.PublicURL)
	publicOriginURL, err := url.Parse(publicURL)
	if err != nil || publicOriginURL.Host == "" || publicOriginURL.User != nil ||
		(publicOriginURL.Scheme != "http" && publicOriginURL.Scheme != "https") ||
		(publicOriginURL.Path != "" && publicOriginURL.Path != "/") ||
		publicOriginURL.RawQuery != "" || publicOriginURL.Fragment != "" {
		return nil, fmt.Errorf("adminui: public URL must be an http(s) origin")
	}
	publicOrigin, ok := canonicalOrigin(publicOriginURL)
	if !ok {
		return nil, fmt.Errorf("adminui: public URL must contain a valid host and port")
	}
	if cfg.AllowInsecureLoopbackCookies {
		hostIP := net.ParseIP(publicOriginURL.Hostname())
		if publicOriginURL.Scheme != "http" || hostIP == nil || !hostIP.IsLoopback() {
			return nil, fmt.Errorf("adminui: insecure cookies require a literal loopback HTTP origin")
		}
	}
	return &Server{
		pool:                cfg.Pool,
		logger:              cfg.Logger,
		version:             cfg.Version,
		authenticator:       cfg.Authenticator,
		login:               login,
		adminGroups:         groups,
		selfGroups:          selfGroups,
		publicOrigin:        publicOrigin,
		sessionCookieSecure: !cfg.AllowInsecureLoopbackCookies,
		slackHost:           cfg.SlackWorkspaceHost,
		connectors:          cfg.Connectors,
		connections:         cfg.Connections,
		providers:           cfg.Providers,
		viewerBaseURL:       cfg.SessionViewerBaseURL,
		usageClient:         cfg.UsageClient,
		reembed:             cfg.ReembedLauncher,
		reembedSources:      cfg.ReembedSources,
		captionModel:        cfg.CaptionModel,
		summaryControl:      cfg.SessionSummaryControl,
		reports:             cfg.ScheduledReportsStore,
		notionRoots:         cfg.NotionRootsStore,
		sourceControl:       cfg.SlackSourceControl,
		images:              cfg.Images,
		sessionPrivacy:      cfg.SessionPrivacy,
		toolWording:         cfg.ToolWording,
		registries:          cfg.Registries,
		sourceDeps:          cfg.SourceDeps,
		setup:               cfg.Setup,
		metrics:             newAdminMetrics(cfg.Registry),
	}, nil
}

// Mux is what the browser routes are registered on: an *http.ServeMux.
type Mux interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Mount registers the browser surface. The complete /admin tree, including
// assets and not-found responses, is administrator-gated so it never becomes
// a partially discoverable ordinary-user product.
func (s *Server) Mount(mux Mux) error {
	if err := s.MountSessions(mux); err != nil {
		return err
	}
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return fmt.Errorf("adminui: static fs: %w", err)
	}
	mux.Handle("GET /admin/static/", s.authedBrowser(http.StripPrefix("/admin/static/", http.FileServer(http.FS(staticFS)))))
	mux.Handle("GET /admin/{$}", s.authedBrowser(http.HandlerFunc(s.handleOverview)))
	mux.Handle("GET /admin/ask/sessions", s.authedBrowser(http.HandlerFunc(s.handleAskSessions)))
	mux.Handle("GET /admin/ask/sessions/{id}", s.authedBrowser(http.HandlerFunc(s.handleAskSession)))
	mux.Handle("GET /admin/ask/feedback", s.authedBrowser(http.HandlerFunc(s.handleAskFeedback)))
	mux.Handle("GET /admin/ask/images/{id}", s.authedBrowser(http.HandlerFunc(s.handleAskImage)))
	s.mountPromptRoutes(mux)
	// Remaining administrator routes are registered below.

	return s.mountAdminRoutes(mux)
}

// MountSessions registers only the owner-bound session UI, plus the sign-in
// routes when the authenticator provides them. The focused local server uses
// this method so unrelated administrator routes never exist.
func (s *Server) MountSessions(mux Mux) error {
	s.mountLogin(mux)
	mountIcons(mux)
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return fmt.Errorf("adminui: static fs: %w", err)
	}
	mux.Handle("GET /sessions/static/", s.signedInBrowser(http.StripPrefix("/sessions/static/", http.FileServer(http.FS(staticFS)))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		setBrowserSecurityHeaders(w)
		principal, code, message := s.browserPrincipal(w, r)
		if code != http.StatusOK {
			s.denyBrowser(w, r, code, message)
			return
		}
		if s.isAdmin(principal) {
			http.Redirect(w, r, "/admin/", http.StatusSeeOther)
			return
		}
		if principal.HasAnyGroup(s.selfGroups) {
			http.Redirect(w, r, "/sessions", http.StatusSeeOther)
			return
		}
		s.denySelfService(w, r, principal)
	})
	mux.Handle("GET /sessions", s.authedSelfServiceBrowser(http.HandlerFunc(s.handleOwnerSessions)))
	mux.Handle("POST /sessions/privacy", s.authedSelfServiceBrowser(http.HandlerFunc(s.handleOwnerPrivacyUpdate)))
	return nil
}

func (s *Server) mountAdminRoutes(mux Mux) error {
	s.mountConnectorRoutes(mux)
	s.mountConnectorSectionRoutes(mux)
	s.mountConnectionRoutes(mux)
	s.mountProviderRoutes(mux)
	mux.Handle("GET /admin/reports", s.authedBrowser(http.HandlerFunc(s.handleReports)))
	mux.Handle("POST /admin/reports", s.authedBrowser(http.HandlerFunc(s.handleReportCreate)))
	mux.Handle("GET /admin/reports/new", s.authedBrowser(http.HandlerFunc(s.handleReportNew)))
	mux.Handle("GET /admin/reports/{id}", s.authedBrowser(http.HandlerFunc(s.handleReport)))
	mux.Handle("POST /admin/reports/{id}", s.authedBrowser(http.HandlerFunc(s.handleReportUpdate)))
	mux.Handle("GET /admin/reports/{id}/edit", s.authedBrowser(http.HandlerFunc(s.handleReportEdit)))
	mux.Handle("POST /admin/reports/{id}/enabled", s.authedBrowser(http.HandlerFunc(s.handleReportEnabled)))
	mux.Handle("POST /admin/reports/{id}/run", s.authedBrowser(http.HandlerFunc(s.handleReportRun)))
	mux.Handle("POST /admin/reports/{id}/delete", s.authedBrowser(http.HandlerFunc(s.handleReportDelete)))
	mux.Handle("GET /admin/operations", s.authedBrowser(http.HandlerFunc(s.handleOperations)))
	mux.Handle("GET /admin/operations/jobs", s.authedBrowser(http.HandlerFunc(s.handleOperationsJobs)))
	mux.Handle("POST /admin/operations/reembed", s.authedBrowser(http.HandlerFunc(s.handleOperationsReembed)))
	mux.Handle("POST /admin/operations/slack-documents", s.authedBrowser(http.HandlerFunc(s.handleOperationsSlackDocuments)))
	mux.Handle("POST /admin/operations/images", s.authedBrowser(http.HandlerFunc(s.handleOperationsImages)))
	mux.Handle("POST /admin/operations/session-summaries/repair", s.authedBrowser(http.HandlerFunc(s.handleOperationsSummaryRepair)))
	mux.Handle("POST /admin/operations/session-summarization", s.authedBrowser(http.HandlerFunc(s.handleOperationsSessionSummarization)))
	mux.Handle("POST /admin/operations/session-summaries/schedule", s.authedBrowser(http.HandlerFunc(s.handleOperationsSummarySchedule)))
	mux.Handle("GET /admin/operations/failures", s.authedBrowser(http.HandlerFunc(s.handleFailures)))
	mux.Handle("POST /admin/operations/failures/retry", s.authedBrowser(http.HandlerFunc(s.handleFailuresRetry)))
	mux.Handle("GET /admin/operations/audit", s.authedBrowser(http.HandlerFunc(s.handleAudit)))
	s.mountSetupRoutes(mux)
	mux.Handle("/admin/", s.authedBrowser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.renderNotFound(w, r, missingPage)
	})))
	return nil
}

// mustPage parses a page: the layout, the shared partials, any partials of
// its own, and last the page, so the page's definitions of the page-header
// slots ("header-meta", "header-actions") replace a partial's.
func mustPage(name string, partials ...string) *template.Template {
	files := []string{"templates/layout.html", "templates/partials.html"}
	for _, partial := range partials {
		files = append(files, "templates/"+partial)
	}
	files = append(files, "templates/"+name)
	return template.Must(template.New("layout.html").Funcs(templateFuncs).ParseFS(assets, files...))
}

func (s *Server) render(w http.ResponseWriter, tmpl *template.Template, data any) {
	s.renderStatus(w, tmpl, data, http.StatusOK)
}

func (s *Server) renderStatus(w http.ResponseWriter, tmpl *template.Template, data any, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		s.logger.Error("admin ui render failed", "err", err)
	}
}
