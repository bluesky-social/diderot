package adminui

import (
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/sessioningest"
)

const (
	ownerSessionPageSize = 50
	selfCSRFCookieName   = "diderot_session_privacy_csrf"
	sessionsPath         = "/sessions"
)

var (
	ownerSessionsPage = mustSelfPage("sessions.html")
	selfDeniedPage    = mustSelfPage("denied.html")
)

// selfPageData is what the self-service layout needs from every page.
type selfPageData struct {
	Title     string
	Principal auth.Principal
	Version   string
	// SignOutURL and SignOutToken are the header's sign-out form, when the
	// built-in sign-in is active.
	SignOutURL   string
	SignOutToken string
	// SessionsLink is how the header shows My sessions: "current" on that
	// page, "link" on another for an account that can open it, and empty
	// to leave it out.
	SessionsLink string
}

// selfDeniedData is the page a signed-in account gets instead of a page its
// groups do not open: My sessions, or the admin pages.
type selfDeniedData struct {
	selfPageData
	// Place is what the account can't open, as a sentence names it.
	Place string
	// Setting is the deployment variable that lists the groups that can
	// open it, and Groups the groups it lists.
	Setting string
	Groups  []string
	// Elsewhere is a page the account can open instead, if any.
	Elsewhere *deniedLink
}

type deniedLink struct {
	Label string
	Href  string
}

type ownerSessionsData struct {
	selfPageData
	Sessions  []ownerSessionView
	CSRFToken string
	// Before is the page shown, for the form to come back to; NextURL the
	// next older page.
	Before  int64
	NextURL string
	Notice  string
	Error   string
	// NotIndexed says why the sessions listed as not indexed aren't, empty
	// while none is.
	NotIndexed string
}

// Pages is the pagination row under the list.
func (d ownerSessionsData) Pages() pagination {
	return pagination{
		Summary:    fmt.Sprintf("%s shown, newest first", plural(len(d.Sessions), "session")),
		ForwardURL: d.NextURL,
	}
}

type ownerSessionView struct {
	Client    string
	SessionID string
	Title     string
	OriginApp string
	Updated   *time.Time
	Turns     int
	// Status and Tone are the session's search badge.
	Status          string
	Tone            string
	ViewerURL       string
	SelectionKey    string
	OwnerOnly       bool
	PrivacyExplicit bool
}

func mustSelfPage(name string) *template.Template {
	return template.Must(template.New("self_layout.html").Funcs(templateFuncs).ParseFS(assets,
		"templates/self_layout.html", "templates/partials.html", "templates/"+name))
}

func (s *Server) handleOwnerSessions(w http.ResponseWriter, r *http.Request) {
	if s.sessionPrivacy == nil {
		http.Error(w, "agent session privacy unavailable", http.StatusServiceUnavailable)
		return
	}
	principal, _ := PrincipalFromContext(r.Context())
	before, err := parseBefore(r.URL.Query().Get("before"))
	if err != nil {
		http.Redirect(w, r, sessionsPath, http.StatusSeeOther)
		return
	}
	page, err := s.sessionPrivacy.ListSessions(r.Context(), principal.Email, ownerSessionPageSize, before)
	if err != nil {
		s.logger.Error("session privacy page inventory query failed", "owner", principal.Email, "err", err)
		http.Error(w, "agent session query failed", http.StatusInternalServerError)
		return
	}
	summaries, err := s.sessionSummaries(r.Context())
	if err != nil {
		s.logger.Error("session privacy page summary state query failed", "err", err)
		http.Error(w, "agent session query failed", http.StatusInternalServerError)
		return
	}
	csrfToken, err := s.issueSelfCSRFToken(w, r)
	if err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	query := r.URL.Query()
	data := newOwnerSessionsData(principal, s.version, page, csrfToken,
		s.sessionViewerURL(), cleanNotice(query.Get("notice")), summaries.OwnerReason())
	data.Error, data.Before = cleanNotice(query.Get("error")), before
	data.SignOutURL, data.SignOutToken = s.signOutURL(), signOutTokenFromContext(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := ownerSessionsPage.ExecuteTemplate(w, "self_layout", data); err != nil {
		s.logger.Error("session privacy page render failed", "err", err)
	}
}

func newOwnerSessionsData(
	principal auth.Principal,
	version string,
	page sessioningest.OwnerSessionPage,
	csrfToken, viewerBaseURL, notice, notIndexed string,
) ownerSessionsData {
	data := ownerSessionsData{
		selfPageData: selfPageData{Title: "My sessions", Principal: principal, Version: version, SessionsLink: "current"},
		CSRFToken:    csrfToken, Notice: notice,
	}
	for _, item := range page.Sessions {
		view := ownerSessionView{
			Client: displaySessionClient(item.Client), SessionID: item.SessionID, Title: item.Title, OriginApp: item.OriginApp,
			Updated: item.SourceUpdatedAt, Turns: item.UserTurnCount,
			ViewerURL:    ownerSessionViewerURL(viewerBaseURL, item.SessionID),
			SelectionKey: encodeSessionSelection(sessioningest.SessionRef{Client: item.Client, SessionID: item.SessionID}),
			OwnerOnly:    item.OwnerOnly, PrivacyExplicit: item.PrivacyExplicit,
		}
		switch {
		case item.Deleted:
			view.Status, view.Tone = "deleted", toneNeutral
		case item.Excluded:
			view.Status, view.Tone = "not indexed", toneNeutral
		case item.Indexed:
			view.Status, view.Tone = "indexed", toneGood
		case notIndexed != "":
			view.Status, view.Tone = "not indexed", toneNeutral
			data.NotIndexed = notIndexed
		case item.SummaryFailed:
			// Nothing is trying it any more, so it is not waiting.
			view.Status, view.Tone = "could not be summarized", toneBad
		default:
			view.Status, view.Tone = "queued", toneAttention
		}
		data.Sessions = append(data.Sessions, view)
	}
	if page.NextBefore > 0 {
		data.NextURL = sessionsPath + "?before=" + strconv.FormatInt(page.NextBefore, 10)
	}
	return data
}

func (s *Server) handleOwnerPrivacyUpdate(w http.ResponseWriter, r *http.Request) {
	if s.sessionPrivacy == nil {
		http.Error(w, "agent session privacy unavailable", http.StatusServiceUnavailable)
		return
	}
	if !s.requireSelfMutation(w, r) {
		return
	}
	raw := r.Form.Get("owner_only")
	if raw != "true" && raw != "false" {
		http.Error(w, "owner_only must be true or false", http.StatusBadRequest)
		return
	}
	back := ownerSessionsReturn(r.Form.Get("before"))
	selected := r.Form["session"]
	if message := ownerSelectionProblem(len(selected)); message != "" {
		redirectSelf(w, r, back, "error", message)
		return
	}
	sessions := make([]sessioningest.SessionRef, 0, len(selected))
	for _, encoded := range selected {
		session, err := decodeSessionSelection(encoded)
		if err != nil {
			http.Error(w, "invalid session selection", http.StatusBadRequest)
			return
		}
		sessions = append(sessions, session)
	}
	principal, _ := PrincipalFromContext(r.Context())
	result, err := s.sessionPrivacy.SetSessions(r.Context(), principal.Email, sessions, raw == "true")
	if err != nil {
		if errors.Is(err, sessioningest.ErrInvalidSessionSelection) || errors.Is(err, sessioningest.ErrSessionNotFound) {
			redirectSelf(w, r, back, "error", "A selected session is no longer listed, so nothing changed. Choose again from the list below.")
			return
		}
		s.logger.Error("session privacy page update failed", "owner", principal.Email, "err", err)
		http.Error(w, "agent session privacy update failed", http.StatusInternalServerError)
		return
	}
	redirectSelf(w, r, back, "notice", ownerPrivacyNotice(result.SessionUpdates, result.OwnerOnly))
}

// ownerSelectionProblem says what is wrong with a selection of n sessions,
// empty when nothing is.
func ownerSelectionProblem(n int) string {
	switch {
	case n == 0:
		return "Select at least one session first."
	case n > 200:
		return "Select at most 200 sessions at a time."
	}
	return ""
}

func ownerPrivacyNotice(updated int, ownerOnly bool) string {
	if ownerOnly {
		return plural(updated, "session") + " can now be found only by you."
	}
	return plural(updated, "session") + " can now be found by everyone who can search Diderot."
}

// ownerSessionsReturn is the page of My sessions a form came from.
func ownerSessionsReturn(before string) url.Values {
	values := url.Values{}
	if value, err := parseBefore(before); err == nil && value > 0 {
		values.Set("before", strconv.FormatInt(value, 10))
	}
	return values
}

func redirectSelf(w http.ResponseWriter, r *http.Request, values url.Values, key, message string) {
	values.Set(key, message)
	http.Redirect(w, r, sessionsPath+"?"+values.Encode(), http.StatusSeeOther)
}

func encodeSessionSelection(session sessioningest.SessionRef) string {
	return base64.RawURLEncoding.EncodeToString([]byte(session.Client + "\x00" + session.SessionID))
}

func decodeSessionSelection(encoded string) (sessioningest.SessionRef, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return sessioningest.SessionRef{}, err
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return sessioningest.SessionRef{}, fmt.Errorf("invalid session selection")
	}
	return sessioningest.SessionRef{Client: parts[0], SessionID: parts[1]}, nil
}

func (s *Server) issueSelfCSRFToken(w http.ResponseWriter, r *http.Request) (string, error) {
	return issueCSRFTokenCookie(w, r, selfCSRFCookieName, sessionsPath, s.sessionCookieSecure)
}

func (s *Server) requireSelfMutation(w http.ResponseWriter, r *http.Request) bool {
	return requireFormMutation(w, r, s.publicOrigin, selfCSRFCookieName)
}

func parseBefore(raw string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("invalid before")
	}
	return value, nil
}

// sessionViewerURL is the session viewer's origin, empty while there is none.
func (s *Server) sessionViewerURL() string {
	if s.viewerBaseURL == nil {
		return ""
	}
	return s.viewerBaseURL()
}

func ownerSessionViewerURL(base, sessionID string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/sessions/" + url.PathEscape(sessionID)
}

func displaySessionClient(client string) string {
	if client == "pi" {
		return "prime-agent"
	}
	return client
}

// denySelfService tells a signed-in account outside the self-service groups
// what My sessions needs, in the shell, instead of a bare 403.
func (s *Server) denySelfService(w http.ResponseWriter, r *http.Request, principal *auth.Principal) {
	data := selfDeniedData{Place: "My sessions", Setting: "DIDEROT_SELF_SERVICE_GROUPS", Groups: sortedGroups(s.selfGroups)}
	if s.isAdmin(principal) {
		data.Elsewhere = &deniedLink{Label: "Go to the admin pages", Href: "/admin/"}
	}
	s.renderDenied(w, r, principal, data)
}

// denyAdmin tells a signed-in account outside the administrator groups what
// the admin pages need, and points it at My sessions when it can open that.
func (s *Server) denyAdmin(w http.ResponseWriter, r *http.Request, principal *auth.Principal) {
	data := selfDeniedData{Place: "the admin pages", Setting: "DIDEROT_ADMIN_GROUPS", Groups: sortedGroups(s.adminGroups)}
	if principal.HasAnyGroup(s.selfGroups) {
		data.Elsewhere = &deniedLink{Label: "Go to My sessions", Href: sessionsPath}
		data.SessionsLink = "link"
	}
	s.renderDenied(w, r, principal, data)
}

func (s *Server) renderDenied(w http.ResponseWriter, r *http.Request, principal *auth.Principal, data selfDeniedData) {
	r = s.withSignOutToken(w, r)
	data.Title, data.Principal, data.Version = "Access needed", *principal, s.version
	data.SignOutURL, data.SignOutToken = s.signOutURL(), signOutTokenFromContext(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	if err := selfDeniedPage.ExecuteTemplate(w, "self_layout", data); err != nil {
		s.logger.Error("access denied page render failed", "err", err)
	}
}

func sortedGroups(set map[string]struct{}) []string {
	groups := make([]string, 0, len(set))
	for group := range set {
		groups = append(groups, group)
	}
	slices.Sort(groups)
	return groups
}
