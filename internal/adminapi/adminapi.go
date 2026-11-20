// Package adminapi serves the HTTP API under /v1 (retrieve, list, expand,
// Ask, session push and self-service) and authenticates /mcp, and holds the
// in-process Slack, re-embed and image services the browser admin acts
// through. Administration has no HTTP API: it is the /admin UI
// (internal/adminui).
package adminapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/adminjob"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/authorization"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/reembed"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// Expander is the read path behind /v1/expand-chunk. Production
// wires *cmd/diderot's sourceExpander, which hands each event to its
// registered source's Expand. Defined as an interface here so the
// admin server doesn't import the concrete implementation and tests
// can inject a fake.
//
// Errors:
//   - ask.ErrExpandEventNotFound when no row with that id exists.
//   - ask.ErrSourceNotExpandable when the row exists but its source
//     isn't supported (or its backend is unconfigured); ExpandResult.Source
//     is populated so the response envelope still tells the caller
//     which source was attempted.
//   - any other error propagates as a 500 with a logged context.
type Expander interface {
	Expand(ctx context.Context, eventID int64) (ask.ExpandResult, error)
}

// SyncWorker is the queue the Slack history worker drains
// (*slackcx.ChannelSync).
type SyncWorker interface {
	RequestBackfill(ctx context.Context, channelID string) (bool, error)
}

// Retriever is the subset of retrieve.Retriever adminapi needs.
// Abstracted as an interface to keep the admin server testable
// without spinning up qdrant + an embedder.
type Retriever interface {
	Retrieve(ctx context.Context, req retrieve.Request) (*retrieve.Result, error)
	List(ctx context.Context, req retrieve.ListRequest) (*retrieve.Result, error)
}

// AskRunner is the Ask adminapi needs. Abstracted as an interface for
// testability — production wires the replica's Ask from cmd/diderot.
//
// A session UUID identifies the conversation; the runner internally
// loads + persists history so this same interface covers "start a
// new session" (caller created one via CreateSession first) and
// "continue an existing one" (caller threads the UUID through).
type AskRunner interface {
	CreateSession(ctx context.Context) (uuid.UUID, error)
	ValidateModelOverrides(overrides ask.ModelOverrides) error
	ExecuteTurn(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error)
}

type AskArtifactReader interface {
	ReadArtifact(ctx context.Context, sessionID, artifactID uuid.UUID) (ask.ArtifactRecord, []byte, error)
}

// AskInvocationNotifier is shared with the MCP Ask surface so every accepted
// non-Slack invocation follows the same operator-notification path.
type AskInvocationNotifier = ask.AskInvocationNotifier

// ScopeResolver maps a verified operator email to the visibility-scope
// set that identity is entitled to on the content-reading endpoints:
// always {public, internal}, cached public Slack, plus slack-channel:<id> for every private
// channel the caller's Slack identity shares with the bot. Production
// wires *slack.UserScopeResolver; the interface keeps adminapi free of
// a slack-package dependency for this and lets tests inject a fake.
//
// The resolver NEVER returns nil/unrestricted — a lookup failure
// degrades to the {public, internal} baseline.
type ScopeResolver interface {
	ScopesForEmail(ctx context.Context, email string) []string
}

// AskerResolver enriches verified HTTP identity claims with a source profile.
// Production uses the persisted Slack directory so HTTP and Slack turns feed
// the same AskerInfo shape into the shared context renderer.
type AskerResolver interface {
	AskerForEmail(ctx context.Context, email string) (*ask.AskerInfo, error)
}

type SessionPrivacyService interface {
	SetSessions(context.Context, string, []sessioningest.SessionRef, bool) (sessioningest.SessionPrivacyUpdate, error)
	ListSessions(context.Context, string, int, int64) (sessioningest.OwnerSessionPage, error)
}

// ChannelConfigStore is a channel's switches (*slack.ChannelConfigStore).
type ChannelConfigStore interface {
	SetAllowExternalAsk(ctx context.Context, channelID string, allow bool, updatedBy string) error
	SetAskEnabled(ctx context.Context, channelID string, enabled bool, updatedBy string) error
	// SetDocIngestEnabled turns document extraction on or off; turning it
	// on, again too, restarts the channel's document reconciliation.
	SetDocIngestEnabled(ctx context.Context, channelID string, enabled bool, updatedBy string) error
	DocIngestEnabled(ctx context.Context, channelID string) (bool, error)
}

// ChannelInviter is the Slack calls made as the user token's owner
// (*slack.Inviter): resolving a channel's name or ID, inviting and removing
// the bot, and checking its membership before a backfill is queued.
type ChannelInviter interface {
	ResolveChannel(ctx context.Context, nameOrID string) (channelID, name string, err error)
	Invite(ctx context.Context, channelID string) error
	IsBotInChannel(ctx context.Context, channelID string) (bool, error)
	Kick(ctx context.Context, channelID string) error
}

// IngestBlacklister is the do-not-ingest list (*slack.IngestBlacklist).
type IngestBlacklister interface {
	Add(ctx context.Context, channelID, channelName, reason, addedBy string) (slackcx.BlacklistEntry, error)
	RemoveByID(ctx context.Context, channelID string) (bool, error)
}

// Server is the HTTP handler bundle.
type Server struct {
	// authenticator identifies /v1 bearer callers (DIDEROT_AUTH_MODE).
	// Non-nil once Mount succeeds.
	authenticator auth.Authenticator
	logger        *slog.Logger

	mu            sync.Mutex
	slack         func() (SlackControls, bool)
	retriever     Retriever
	ask           AskRunner
	askContext    context.Context
	askNotifier   AskInvocationNotifier
	statusPool    *pgxpool.Pool
	expander      Expander
	sessionPush   SessionPusher
	fileBytes     FileBytesGetter
	scopeResolver ScopeResolver
	// scopeGrants adds the scopes sources grant to each caller.
	scopeGrants       ask.ScopeGranter
	askerResolver     AskerResolver
	authorizer        *authorization.Authorizer
	adminGroups       map[string]struct{}
	selfServiceGroups map[string]struct{}
	sessionPrivacy    SessionPrivacyService
	askGate           AskGate
}

// NewServer constructs a Server. authenticator must be non-nil — Mount
// refuses to wire routes otherwise so the API is never exposed
// unauthenticated.
func NewServer(authenticator auth.Authenticator, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{authenticator: authenticator, logger: logger}
}

// SetAdminGroups configures the administrators' groups. No route needs one:
// they decide who may use Ask before the install is live.
func (s *Server) SetAdminGroups(groups []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminGroups = make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if group = strings.TrimSpace(group); group != "" {
			s.adminGroups[group] = struct{}{}
		}
	}
}

// SetSelfServiceGroups configures the groups allowed to use the
// owner-bound /v1/me endpoints. Unlike general use routes, these mutations do
// not rely only on possession of a valid audience token.
func (s *Server) SetSelfServiceGroups(groups []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selfServiceGroups = normalizedGroupSet(groups)
}

func normalizedGroupSet(groups []string) map[string]struct{} {
	out := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if group = strings.TrimSpace(group); group != "" {
			out[group] = struct{}{}
		}
	}
	return out
}

func (s *Server) SetSessionPrivacyService(service SessionPrivacyService) {
	s.mu.Lock()
	s.sessionPrivacy = service
	s.mu.Unlock()
}

func (s *Server) getSessionPrivacyService() SessionPrivacyService {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionPrivacy
}

// SlackControls are what the Slack actions act through, built from the
// saved Slack settings. They read and write the database, which the Slack
// workers follow, and call Slack directly, so they work on every replica.
type SlackControls struct {
	SyncWorker     SyncWorker
	ChannelConfigs ChannelConfigStore
	Blacklist      IngestBlacklister
	// Inviter is nil while Slack has no user token.
	Inviter ChannelInviter
}

// SetSlackControls wires the Slack actions: controls returns the controls
// for the Slack settings saved now, and false while Slack is off.
func (s *Server) SetSlackControls(controls func() (SlackControls, bool)) {
	s.mu.Lock()
	s.slack = controls
	s.mu.Unlock()
}

// slackControls returns the Slack controls, and false while Slack is off.
func (s *Server) slackControls() (SlackControls, bool) {
	s.mu.Lock()
	controls := s.slack
	s.mu.Unlock()
	if controls == nil {
		return SlackControls{}, false
	}
	return controls()
}

// SetRetriever swaps the backing retriever. Not leader-gated —
// qdrant + Postgres are both read-safe from any replica, and once
// we go back to multi-replica the LoadBalancer fans out naturally.
func (s *Server) SetRetriever(r Retriever) {
	s.mu.Lock()
	s.retriever = r
	s.mu.Unlock()
}

// SetAskRunner attaches the replica's Ask, which /v1/ask, its artifact
// route, and the MCP ask tool run on. Every replica has its own; turns of
// one session are serialized across replicas by the runner. MCP ask tasks
// run under ctx, which ends when the replica cancels the turns it still
// runs, and none starts once it ends.
func (s *Server) SetAskRunner(ctx context.Context, r AskRunner) {
	s.mu.Lock()
	s.ask = r
	s.askContext = ctx
	s.mu.Unlock()
}

// SetAskInvocationNotifier wires the out-of-band ask-invocation
// notifier (a Slack DM to the operator). It's a best-effort side
// channel, not a backend the request depends on; a nil notifier just
// makes handleAsk skip the notify.
func (s *Server) SetAskInvocationNotifier(n AskInvocationNotifier) {
	s.mu.Lock()
	s.askNotifier = n
	s.mu.Unlock()
}

func (s *Server) getChannelInviter() ChannelInviter {
	controls, _ := s.slackControls()
	return controls.Inviter
}

// SetExpander wires the source-dispatching expand backend used by
// /v1/expand-chunk. Production passes the full sourceExpander; tests
// inject a fake. The dispatching lives entirely behind this interface,
// so the HTTP handler doesn't know whether a given event_id resolves
// to a slack thread, an agent session transcript, or something else.
func (s *Server) SetExpander(e Expander) {
	s.mu.Lock()
	s.expander = e
	s.mu.Unlock()
}

// SetScopeResolver wires the per-identity visibility-scope resolver
// used by every content-reading endpoint (retrieve, list, Ask, expand, MCP)
// and the admin pages' images. nil is the safe default:
// scopesForRequest degrades to the {public, internal} baseline, so a
// binary running without the slack bot token still serves public +
// internal content and simply can't reach any private channel.
//
// Not leader-gated — resolution is a read (slack_users lookup + one
// Tier-3 users.conversations call) that must work from any replica,
// exactly like SetRetriever.
// SetScopeGrants adds the scopes sources grant to every caller's scopes.
func (s *Server) SetScopeGrants(grants ask.ScopeGranter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopeGrants = grants
}

func (s *Server) SetScopeResolver(r ScopeResolver) {
	s.mu.Lock()
	s.scopeResolver = r
	s.mu.Unlock()
}

func (s *Server) getScopeResolver() ScopeResolver {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scopeResolver
}

// SetAskerResolver wires optional profile enrichment for authenticated HTTP
// asks. It is replica-agnostic and read-only, like SetScopeResolver.
func (s *Server) SetAskerResolver(r AskerResolver) {
	s.mu.Lock()
	s.askerResolver = r
	s.mu.Unlock()
}

func (s *Server) getAskerResolver() AskerResolver {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.askerResolver
}

// scopesForRequest computes the visibility boundary for the caller of
// r. It reads the authenticated principal stashed by authed and asks the
// resolver for that email's scope set.
//
// The return value is ALWAYS a non-empty, non-nil set — at minimum
// {public, internal}. When no resolver is wired, or
// the principal carries no email, we return the baseline rather than nil.
func (s *Server) scopesForRequest(r *http.Request) []string {
	scopes := s.authorizationForRequest(r)
	return scopes
}

// Mux is what the API's routes are registered on: an *http.ServeMux.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Mount wires the API routes onto mux. Returns non-nil error if no
// authenticator is configured.
func (s *Server) Mount(mux Mux) error {
	if s.authenticator == nil {
		return errors.New("adminapi: an authenticator is required to mount the API")
	}
	if err := s.MountSelfService(mux); err != nil {
		return err
	}
	mux.HandleFunc("POST /v1/retrieve", s.authed(s.handleRetrieve))
	mux.HandleFunc("POST /v1/list", s.authed(s.handleListEvents))
	mux.HandleFunc("POST /v1/ask", s.authed(s.handleAsk))
	mux.HandleFunc("GET /v1/ask/{sessionID}/artifacts/{artifactID}", s.authed(s.handleAskArtifact))
	mux.HandleFunc("POST /v1/expand-chunk", s.authed(s.handleExpandChunk))
	return nil
}

// MountSelfService registers only the owner-bound session inventory and
// privacy endpoints and push ingest. session-dev uses this focused surface
// instead of the whole API.
func (s *Server) MountSelfService(mux Mux) error {
	if s.authenticator == nil {
		return errors.New("adminapi: an authenticator is required to mount self-service routes")
	}
	mux.HandleFunc("GET /v1/me/agent-sessions", s.authedSelfService(s.handleMyAgentSessions))
	mux.HandleFunc("PUT /v1/me/agent-session-privacy", s.authedSelfService(s.handleMyAgentSessionPrivacy))
	// Push ingest: the owner is the caller, never the request. GET also
	// serves HEAD. 404 unless push ingest is configured.
	mux.HandleFunc("PUT /v1/ingest/agent-sessions/{session_id}", s.authedSelfService(s.handleSessionPush))
	mux.HandleFunc("GET /v1/ingest/agent-sessions/{session_id}", s.authedSelfService(s.handleSessionPush))
	return nil
}

// SessionPusher serves push ingest for an authenticated owner
// (*sessioningest.PushHandler in production).
type SessionPusher interface {
	ServePut(w http.ResponseWriter, r *http.Request, owner string)
	ServeGet(w http.ResponseWriter, r *http.Request, owner string)
}

// SetSessionPush enables push ingest. nil leaves the routes answering 404.
func (s *Server) SetSessionPush(p SessionPusher) {
	s.mu.Lock()
	s.sessionPush = p
	s.mu.Unlock()
}

func (s *Server) handleSessionPush(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	push := s.sessionPush
	s.mu.Unlock()
	if push == nil {
		http.Error(w, "push ingest is not enabled", http.StatusNotFound)
		return
	}
	owner := operatorFromContext(r.Context()).Email
	if r.Method == http.MethodPut {
		push.ServePut(w, r, owner)
		return
	}
	push.ServeGet(w, r, owner)
}

// MountRetrieve registers only the authenticated corpus retrieval endpoint.
// session-dev uses this focused mount to exercise visibility through the real
// Qdrant and Postgres path without the rest of the API.
func (s *Server) MountRetrieve(mux *http.ServeMux) error {
	if s.authenticator == nil {
		return errors.New("adminapi: an authenticator is required to mount retrieve")
	}
	if s.getRetriever() == nil {
		return errors.New("adminapi: retriever is required to mount retrieve")
	}
	mux.HandleFunc("POST /v1/retrieve", s.authed(s.handleRetrieve))
	return nil
}

// operatorFromContext returns the principal stashed by authed, or nil if no
// operator identity has been attached to the context.
func operatorFromContext(ctx context.Context) *auth.Principal {
	return auth.FromContext(ctx)
}

// authed wraps an operator-facing handler with bearer authentication (an
// OIDC access token, or a static token in static mode). Who may hold a valid
// token is governed by its issuer, not by this handler.
//
// Status codes: 401 for a missing/malformed/invalid token, 403 for an
// identity outside the allowed email domains.
func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, code, msg := s.verifyOperator(r)
		if principal == nil {
			http.Error(w, msg, code)
			return
		}
		// Audit trail: who performed which operator action. Keyed on the
		// subject; email is best-effort.
		s.logger.Info("operator request",
			"sub", principal.Subject, "email", principal.Email,
			"method", r.Method, "path", r.URL.Path)
		h(w, r.WithContext(auth.NewContext(r.Context(), principal)))
	}
}

func (s *Server) principalIsAdmin(principal *auth.Principal) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return principal.HasAnyGroup(s.adminGroups)
}

func (s *Server) authedSelfService(h http.HandlerFunc) http.HandlerFunc {
	return s.authed(func(w http.ResponseWriter, r *http.Request) {
		if !s.principalCanSelfServe(operatorFromContext(r.Context())) {
			http.Error(w, "team member access required", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

// principalCanSelfServe is the authedSelfService gate: an owner-capable
// email plus self-service group membership.
func (s *Server) principalCanSelfServe(principal *auth.Principal) bool {
	return principal != nil && sessioningest.CanonicalOwner(principal.Email) != "" && s.principalIsSelfServiceMember(principal)
}

func (s *Server) principalIsSelfServiceMember(principal *auth.Principal) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return principal.HasAnyGroup(s.selfServiceGroups)
}

// verifyOperator authenticates the request's bearer. On success returns the
// principal and (200, ""). On failure returns nil plus the HTTP status +
// message the caller should send: 401 for a missing/invalid token, 403 for a
// valid one whose identity the deployment does not admit (signing in again
// cannot help).
func (s *Server) verifyOperator(r *http.Request) (*auth.Principal, int, string) {
	principal, err := s.authenticator.Authenticate(r)
	switch {
	case errors.Is(err, auth.ErrNoCredentials):
		return nil, http.StatusUnauthorized, "missing bearer token"
	case errors.Is(err, auth.ErrIdentityNotAllowed):
		return nil, http.StatusForbidden, "identity not allowed: email missing, unverified, or outside the allowed domains"
	case err != nil || principal == nil:
		return nil, http.StatusUnauthorized, "invalid token"
	}
	return principal, http.StatusOK, ""
}

func (s *Server) getRetriever() Retriever {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retriever
}

func (s *Server) getAsk() AskRunner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ask
}

// MCPAskRunner returns the replica's Ask, when it supports the atomic
// owned-session boundary MCP tasks require, with the context its tasks run
// under and the operator notifier.
func (s *Server) MCPAskRunner() ask.CorpusAskLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	runner, _ := s.ask.(ask.CorpusAskRunner)
	return ask.CorpusAskLease{Runner: runner, Context: s.askContext, Notifier: s.askNotifier}
}

func (s *Server) getAskInvocationNotifier() AskInvocationNotifier {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.askNotifier
}

// StartReembed records a re-embed of sources, or of every source when there
// are none, for the leader to run, one job at a time.
func (s *Server) StartReembed(ctx context.Context, requestedBy string, sources []string) (int64, error) {
	pool := s.getStatusPool()
	if pool == nil {
		return 0, errNoJobStore
	}
	return reembed.Request(ctx, adminjob.NewStore(pool), requestedBy, sources)
}

// retrieveRequest is the JSON shape for POST /v1/retrieve. Maps
// directly onto retrieve.Request plus minor ergonomic fields
// (ISO-8601 timestamps that Go unmarshals as time.Time).
type retrieveRequest struct {
	Question       string    `json:"question"`
	Sources        []string  `json:"sources,omitempty"`
	Actors         []string  `json:"actors,omitempty"`
	OccurredAfter  time.Time `json:"occurred_after,omitempty"`
	OccurredBefore time.Time `json:"occurred_before,omitempty"`
	TopK           int       `json:"top_k,omitempty"`
}

func (s *Server) handleRetrieve(w http.ResponseWriter, r *http.Request) {
	var req retrieveRequest
	if err := readJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		http.Error(w, "question required", http.StatusBadRequest)
		return
	}
	retr := s.getRetriever()
	if !requireBackend(w, retr, "retrieve not configured (no embedder URL or qdrant)") {
		return
	}
	start := time.Now()
	res, err := retr.Retrieve(r.Context(), retrieve.Request{
		Question: req.Question,
		Filters: retrieve.Filters{
			Sources:        req.Sources,
			Actors:         req.Actors,
			OccurredAfter:  req.OccurredAfter,
			OccurredBefore: req.OccurredBefore,
			// Visibility boundary for this caller's identity. Always
			// non-nil {public, internal}+ so the HTTP path can no
			// longer read private-channel content the caller isn't a
			// member of. See scopesForRequest.
			AllowedScopes: s.scopesForRequest(r),
		},
		TopK: req.TopK,
	})
	if errors.Is(err, retrieve.ErrNotSetUp) {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.logger.Warn("retrieve failed", "err", err)
		http.Error(w, fmt.Sprintf("retrieve: %v", err), http.StatusInternalServerError)
		return
	}
	s.logRetrieveCall(r.Context(), "retrieve", req.Question, req, res, time.Since(start))
	s.writeJSON(w, http.StatusOK, res)
}

// listRequest is the JSON shape for POST /v1/list. Maps onto
// retrieve.ListRequest plus minor ergonomic fields (RFC3339
// timestamps Go unmarshals into time.Time). No question — this is
// the database-shaped enumeration path the agent's `list` tool also uses.
type listRequest struct {
	Sources          []string  `json:"sources,omitempty"`
	Actors           []string  `json:"actors,omitempty"`
	OccurredAfter    time.Time `json:"occurred_after,omitempty"`
	OccurredBefore   time.Time `json:"occurred_before,omitempty"`
	TopK             int       `json:"top_k,omitempty"`
	ThreadRootsOnly  bool      `json:"thread_roots_only,omitempty"`
	SessionLevelOnly bool      `json:"session_level_only,omitempty"`
	Cursor           string    `json:"cursor,omitempty"`
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	var req listRequest
	if err := readJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	retr := s.getRetriever()
	if !requireBackend(w, retr, "list not configured (no retriever)") {
		return
	}
	// Clamp top_k server-side. The retrieve.List method also clamps
	// (defense in depth).
	if req.TopK < 0 {
		req.TopK = 0
	}
	if req.TopK > retrieve.ListMaxTopK {
		req.TopK = retrieve.ListMaxTopK
	}
	start := time.Now()
	res, err := retr.List(r.Context(), retrieve.ListRequest{
		Sources:          req.Sources,
		Actors:           req.Actors,
		OccurredAfter:    req.OccurredAfter,
		OccurredBefore:   req.OccurredBefore,
		TopK:             req.TopK,
		ThreadRootsOnly:  req.ThreadRootsOnly,
		SessionLevelOnly: req.SessionLevelOnly,
		Cursor:           req.Cursor,
		// Visibility boundary — same contract as handleRetrieve.
		AllowedScopes: s.scopesForRequest(r),
	})
	if err != nil {
		// Validation failures (no filter, malformed cursor) → 400,
		// not 500, so a caller can tell them from a server bug, fix the
		// request and retry.
		if errors.Is(err, retrieve.ErrListNoFilter) || errors.Is(err, retrieve.ErrListBadCursor) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.logger.Warn("list failed", "err", err)
		http.Error(w, fmt.Sprintf("list: %v", err), http.StatusInternalServerError)
		return
	}
	s.logRetrieveCall(r.Context(), "list", "", req, res, time.Since(start))
	s.writeJSON(w, http.StatusOK, res)
}

// askRequest is the JSON shape for POST /v1/ask.
//
// SessionID is optional: an empty string creates a new session and
// the response echoes back its UUID. A non-empty UUID continues an
// existing session — the runner loads + appends to the persisted
// history.
//
// The endpoint is synchronous (the caller's HTTP request stays
// open for the duration of the multi-turn model loop, ~10-30s
// typical). Streaming + background-job options live behind a feature
// flag in a later iteration.
type askRequest struct {
	SessionID               string                       `json:"session_id,omitempty"`
	Question                string                       `json:"question"`
	ReasoningEffort         *string                      `json:"reasoning_effort,omitempty"`
	ServiceTier             *string                      `json:"service_tier,omitempty"`
	ReasoningEffortSchedule *ask.ReasoningEffortSchedule `json:"reasoning_effort_schedule,omitempty"`
}

type askResponse struct {
	SessionID        string                   `json:"session_id"`
	Reply            string                   `json:"reply"`
	IncompleteReason ask.IncompleteReason     `json:"incomplete_reason,omitempty"`
	Artifacts        []ask.ArtifactDescriptor `json:"artifacts,omitempty"`
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	// The setup gate covers starting and continuing a session alike.
	if !s.askAllowed(s.principalIsAdmin(operatorFromContext(r.Context()))) {
		http.Error(w, settings.SetupNotice, http.StatusServiceUnavailable)
		return
	}
	var req askRequest
	if err := readJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		http.Error(w, "question required", http.StatusBadRequest)
		return
	}

	runner := s.getAsk()
	if !requireBackend(w, runner, ask.ErrUnavailable.Error()) {
		return
	}
	overrides := ask.ModelOverrides{
		ReasoningEffort:         req.ReasoningEffort,
		ServiceTier:             req.ServiceTier,
		ReasoningEffortSchedule: req.ReasoningEffortSchedule,
	}
	if err := runner.ValidateModelOverrides(overrides); err != nil {
		if errors.Is(err, ask.ErrUnavailable) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "model overrides: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Verified caller email, lowercased for case-insensitive matching.
	// authed guarantees a principal on this route; email may be empty
	// (no claim, or unverified while verification is required), which
	// degrades to no ownership + baseline scopes below.
	caller := s.askerForRequest(r.Context(), operatorFromContext(r.Context()))
	var callerEmail string
	if caller != nil {
		callerEmail = caller.Email
	}

	var sessionID uuid.UUID
	if req.SessionID == "" {
		id, err := runner.CreateSession(r.Context())
		if err != nil {
			s.logger.Warn("ask: create session failed", "err", err)
			http.Error(w, fmt.Sprintf("create session: %v", err), http.StatusInternalServerError)
			return
		}
		sessionID = id
		// Stamp ownership so only this identity can continue the
		// session via HTTP. Slack-surface sessions don't pass through
		// this create path — they're stamped with the original asker's
		// email in LoadOrCreateForSlackThread — and their continuation
		// boundary stays the channel (the slack path keys on
		// channel+thread, never on owner_email).
		if err := s.stampSessionOwner(r.Context(), sessionID, callerEmail); err != nil {
			s.logger.Warn("ask: stamp session owner failed", "session_id", sessionID, "err", err)
			http.Error(w, fmt.Sprintf("create session: %v", err), http.StatusInternalServerError)
			return
		}
	} else {
		id, err := uuid.Parse(req.SessionID)
		if err != nil {
			http.Error(w, fmt.Sprintf("session_id: %v", err), http.StatusBadRequest)
			return
		}
		sessionID = id
		// Ownership gate on continuation. A session the caller doesn't
		// own — including one owned by nobody (owner_email NULL, e.g. a
		// pre-migration session or one whose asker email couldn't be
		// resolved) — is
		// answered with the SAME shape as a genuinely-unknown session
		// id, so continuation is not an ownership oracle. Because the
		// runner replays prior tool results into the model's context,
		// skipping this check would let user B read content retrieved
		// under user A's scopes.
		if ok, err := s.callerOwnsSession(r.Context(), sessionID, callerEmail); err != nil {
			s.logger.Warn("ask: ownership check failed", "session_id", sessionID, "err", err)
			http.Error(w, fmt.Sprintf("ask: %v", err), http.StatusInternalServerError)
			return
		} else if !ok {
			s.writeSessionNotFound(w, sessionID)
			return
		}
	}

	// Resolve transport-specific audience state at ingress, then submit the
	// same request shape used by Slack. The shared service installs scopes for
	// corpus tools and exact-reference prerequisites.
	// The turn stops, recorded as cancelled, when the caller goes away.
	ctx, release := ask.WithCallerCancel(r.Context())
	defer release()
	allowedScopes := s.authorizationForRequest(r)

	// Notify when the turn is accepted so failed and canceled turns are visible too.
	// Slack delivery is detached and must not affect the ask response.
	if n := s.getAskInvocationNotifier(); n != nil {
		go func() {
			// Reached from every POST /v1/ask on every replica. Recovered
			// like the MCP ("mcp-notify-invocation") and Slack
			// ("slack-notify-mention") call sites that reach the same
			// Consumer.sendAskNotification, so a latent panic in the
			// shared formatter or sender fails only this detached
			// notification instead of the replica.
			defer worker.Recover(s.logger, "http-notify-invocation")
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := n.NotifyAskInvocation(dctx, callerEmail, sessionID.String(), req.Question); err != nil {
				s.logger.Warn("ask: invocation DM failed", "session_id", sessionID, "err", err)
			}
		}()
	}

	turn, err := runner.ExecuteTurn(ctx, ask.TurnRequest{
		SessionID:      sessionID,
		UserInput:      req.Question,
		Surface:        ask.AskRunSurfaceHTTP,
		Asker:          caller,
		AllowedScopes:  allowedScopes,
		ModelOverrides: overrides,
	})
	if err != nil && ask.TurnCancelled(ctx) {
		// Nobody is left to answer.
		s.logger.Info("ask: turn cancelled by its caller", "session_id", sessionID)
		return
	}
	if err != nil {
		s.logger.Warn("ask: turn failed", "session_id", sessionID, "err", err)
		if errors.Is(err, ask.ErrUnavailable) {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, fmt.Sprintf("ask: %v", err), http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, http.StatusOK, askResponse{
		SessionID:        sessionID.String(),
		Reply:            turn.Reply,
		IncompleteReason: turn.IncompleteReason,
		Artifacts:        turn.Artifacts,
	})
}

func (s *Server) handleAskArtifact(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(r.PathValue("sessionID"))
	if err != nil {
		http.Error(w, "session_id: invalid UUID", http.StatusBadRequest)
		return
	}
	artifactID, err := uuid.Parse(r.PathValue("artifactID"))
	if err != nil {
		http.Error(w, "artifact_id: invalid UUID", http.StatusBadRequest)
		return
	}
	runner := s.getAsk()
	reader, ok := runner.(AskArtifactReader)
	if !ok || !requireBackend(w, runner, ask.ErrUnavailable.Error()) {
		return
	}
	caller := s.askerForRequest(r.Context(), operatorFromContext(r.Context()))
	callerEmail := ""
	if caller != nil {
		callerEmail = caller.Email
	}
	owned, err := s.callerOwnsSession(r.Context(), sessionID, callerEmail)
	if err != nil {
		s.logger.Warn("ask artifact: ownership check failed", "session_id", sessionID, "err", err)
		http.Error(w, "ask artifact lookup failed", http.StatusInternalServerError)
		return
	}
	if !owned {
		s.writeSessionNotFound(w, sessionID)
		return
	}
	record, data, err := reader.ReadArtifact(r.Context(), sessionID, artifactID)
	if err != nil {
		if ask.IsArtifactNotFound(err) {
			s.writeSessionNotFound(w, sessionID)
			return
		}
		s.logger.Warn("ask artifact: read failed", "session_id", sessionID, "artifact_id", artifactID, "err", err)
		http.Error(w, "ask artifact read failed", http.StatusInternalServerError)
		return
	}
	etag := `"` + record.ContentHash + `"`
	w.Header().Set("Content-Type", record.MediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Disposition", `inline; filename="grafana-panel-`+record.ID.String()+`.png"`)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// askerFromOperator projects the authenticated principal into the same
// transport-neutral identity shape used by Slack turns. Email is canonical
// for corpus actor matching; Display is best-effort display text.
func askerFromOperator(principal *auth.Principal) *ask.AskerInfo {
	if principal == nil {
		return nil
	}
	email := strings.ToLower(strings.TrimSpace(principal.Email))
	display := strings.TrimSpace(principal.Display)
	if display == "" {
		display = email
	}
	if display == "" {
		return nil
	}
	return &ask.AskerInfo{DisplayName: display, Email: email}
}

// askerForRequest treats the authenticated principal as the ownership
// authority, then fills presentation and timezone fields from the Slack
// directory when that email resolves uniquely. Resolver failures are
// best-effort: the turn still receives the authenticated email instead of
// losing asker context entirely.
func (s *Server) askerForRequest(ctx context.Context, principal *auth.Principal) *ask.AskerInfo {
	base := askerFromOperator(principal)
	if base == nil || base.Email == "" {
		return base
	}
	resolver := s.getAskerResolver()
	if resolver == nil {
		return base
	}
	enriched, err := resolver.AskerForEmail(ctx, base.Email)
	if err != nil {
		s.logger.Warn("ask: asker profile enrichment failed", "email", base.Email, "err", err)
		return base
	}
	if enriched == nil {
		return base
	}
	result := *enriched
	// The authenticator supplied the caller email. Slack enriches the
	// profile but cannot replace the ownership/canonical identity boundary.
	result.Email = base.Email
	if result.DisplayName == "" {
		result.DisplayName = base.DisplayName
	}
	return &result
}

// stampSessionOwner sets ownership on
// a freshly-created HTTP session. email may be empty (token without an email
// claim); we store NULL in that case, which makes the session un-continuable.
func (s *Server) stampSessionOwner(ctx context.Context, sessionID uuid.UUID, email string) error {
	pool := s.getStatusPool()
	if pool == nil {
		// No pool wired (should not happen on a route that reached the
		// runner) — surface loudly rather than silently skipping the
		// ownership stamp, which would leave the session continuable by
		// anyone.
		return errors.New("status pool not configured")
	}
	var ownerArg any
	if email != "" {
		ownerArg = email
	}
	_, err := pool.Exec(ctx,
		`UPDATE ask_sessions
		    SET owner_email = $2
		  WHERE id = $1`,
		sessionID, ownerArg)
	return err
}

// callerOwnsSession reports whether email owns a personal HTTP session.
// Returns (false, nil) when the session doesn't exist, has a NULL owner
// or a shared audience, or is owned by a different email —
// all collapse to the same "not yours / not found" outcome so
// the continuation path is not an ownership oracle. A caller with an
// empty email never owns anything.
func (s *Server) callerOwnsSession(ctx context.Context, sessionID uuid.UUID, email string) (bool, error) {
	if email == "" {
		return false, nil
	}
	pool := s.getStatusPool()
	if pool == nil {
		return false, errors.New("status pool not configured")
	}
	return ask.PersonalSessionOwnedBy(ctx, pool, sessionID, email)
}

// writeSessionNotFound is the single response shape for "this session
// id can't be continued": genuinely unknown, or owned by someone else.
// Keeping them byte-identical is the point — a distinct 403 would leak
// that the session exists.
func (s *Server) writeSessionNotFound(w http.ResponseWriter, sessionID uuid.UUID) {
	http.Error(w, fmt.Sprintf("ask: session %s not found", sessionID), http.StatusNotFound)
}

// /v1/expand-chunk returns the fuller source content behind one retrieve
// hit, by event ID. The configured Expander dispatches on the event's
// source. JSON in, JSON out; the response mirrors the model-facing expand
// tool's result (source, supported, content, truncated, note).

type expandChunkRequest struct {
	EventID int64 `json:"event_id"`
}

// expandChunkResponse mirrors the ask tool's expandOutput shape.
type expandChunkResponse struct {
	Source    string         `json:"source"`
	Actor     identity.Actor `json:"actor"`
	Supported bool           `json:"supported"`
	Note      string         `json:"note,omitempty"`
	Content   string         `json:"content,omitempty"`
	Truncated bool           `json:"truncated,omitempty"`
}

func (s *Server) getExpander() Expander {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expander
}

// eventInScope reports whether the event is visible under allowedScopes.
// A missing (or deleted) row returns (false, nil) — the caller collapses
// that into the same not-found response as an out-of-scope row, so the
// expand endpoint can't be used to probe event existence outside the
// caller's audience. Mirrors the events.scope predicate used everywhere
// else; kept here (rather than reusing events.DBSink.EventScope) so the
// deleted-row filter is applied uniformly.
func (s *Server) eventInScope(ctx context.Context, eventID int64, allowedScopes []string) (bool, error) {
	pool := s.getStatusPool()
	if pool == nil {
		return false, errors.New("status pool not configured")
	}
	var one int
	err := pool.QueryRow(ctx, `
		SELECT 1 FROM events
		 WHERE id = $1 AND deleted = false AND scope = ANY($2) AND scope NOT IN ('', 'unstamped')
	`, eventID, allowedScopes).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("event scope lookup: %w", err)
	}
	return true, nil
}

// handleExpandChunk delegates to the configured Expander and writes
// the JSON envelope. Returns 503 when no expander is wired (early
// deploy phase). Returns 404 when the event_id is unknown. Returns
// 200 with supported=false when the row exists but its source isn't
// supported (or its backend isn't configured) — that's a normal
// recoverable state, not an error.
func (s *Server) handleExpandChunk(w http.ResponseWriter, r *http.Request) {
	expander := s.getExpander()
	if !requireBackend(w, expander, "expand backend not configured") {
		return
	}
	var req expandChunkRequest
	if err := readJSON(r, &req); err != nil {
		badRequest(w, err)
		return
	}
	if req.EventID == 0 {
		http.Error(w, "event_id required", http.StatusBadRequest)
		return
	}

	// Visibility gate BEFORE the content fetch, as the native Ask expand
	// tool has: the production sourceExpander checks the event against the
	// scopes in ctx again before any source reads it, and Slack, Zendesk,
	// and pushed sessions also filter what they return around it.
	// Out-of-scope and genuinely-missing both return the existing 404
	// "event not found" — expand is not an existence oracle for ids
	// outside the caller's audience.
	allowedScopes := s.scopesForRequest(r)
	if ok, err := s.eventInScope(r.Context(), req.EventID, allowedScopes); err != nil {
		s.logger.Error("expand-chunk: scope lookup failed",
			"event_id", req.EventID, "err", err)
		http.Error(w, fmt.Sprintf("expand: %v", err), http.StatusInternalServerError)
		return
	} else if !ok {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	}

	res, err := expander.Expand(ask.WithAllowedScopes(r.Context(), allowedScopes), req.EventID)
	switch {
	case errors.Is(err, ask.ErrExpandEventNotFound):
		http.Error(w, "event not found", http.StatusNotFound)
		return
	case errors.Is(err, ask.ErrSourceNotExpandable):
		s.writeJSON(w, http.StatusOK, expandChunkResponse{
			Source:    res.Source,
			Actor:     res.Actor,
			Supported: false,
			Note: fmt.Sprintf(
				"source %q is not expandable in this configuration",
				res.Source),
		})
		return
	case err != nil:
		s.logger.Error("expand-chunk: expander failed",
			"event_id", req.EventID, "source", res.Source, "err", err)
		http.Error(w, fmt.Sprintf("expand: %v", err), http.StatusInternalServerError)
		return
	}

	s.writeJSON(w, http.StatusOK, expandChunkResponse{
		Source:    res.Source,
		Actor:     res.Actor,
		Supported: true,
		Content:   res.Content,
		Truncated: res.Truncated,
	})
}

var _ AskRunner = (*ask.AskService)(nil)
var _ ChannelConfigStore = (*slackcx.ChannelConfigStore)(nil)
var _ ChannelInviter = (*slackcx.Inviter)(nil)
var _ ScopeResolver = (*slackcx.UserScopeResolver)(nil)

// SetStatusPool wires the database the Ask ownership checks, the expand
// scope check, the Slack actions, the jobs and the images read and write.
func (s *Server) SetStatusPool(pool *pgxpool.Pool) {
	s.mu.Lock()
	s.statusPool = pool
	s.mu.Unlock()
}

func (s *Server) getStatusPool() *pgxpool.Pool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusPool
}
