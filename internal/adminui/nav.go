package adminui

import (
	"context"
	"net/http"

	"github.com/bluesky-social/diderot/internal/auth"
)

// Primary navigation keys, as pageData.CurrentNav names them.
const (
	navOverview   = "overview"
	navAsk        = "asks"
	navConnectors = "connectors"
	navPrompts    = "prompts"
	navReports    = "reports"
	navOperations = "operations"
	navSettings   = "settings"
)

// Tab keys of the Ask, Operations and Settings sections, as pageData.CurrentTab
// names them. An empty CurrentTab is the section's first tab.
const (
	tabAskConversations = "conversations"
	tabAskFeedback      = "feedback"

	tabOperationsStatus   = "status"
	tabOperationsJobs     = "jobs"
	tabOperationsFailures = "failures"
	tabOperationsAudit    = "audit"

	tabSettingsSetup        = "setup"
	tabSettingsOrganization = "organization"
	tabSettingsModels       = "models"
	tabSettingsAccess       = "access"
)

// Routes other pages link to.
const (
	settingsPath     = "/admin/settings"
	organizationPath = "/admin/settings/organization"
	modelsPath       = "/admin/settings/models"
	accessPath       = "/admin/settings/access"
	auditPath        = "/admin/operations/audit"
)

type navItem struct {
	Key, Label, Href string
	Current          bool
}

// navTab is one link of a tab row.
type navTab struct {
	Key, Label, Href string
	Current          bool
	// Count is how many items the tab's view holds, shown after its label
	// when Counted.
	Count   int64
	Counted bool
}

// tabSet is the "tabs" partial's input: the tabs and the row's accessible
// name.
type tabSet struct {
	Label string
	Tabs  []navTab
}

var mainNav = []navItem{
	{Key: navOverview, Label: "Overview", Href: "/admin/"},
	{Key: navAsk, Label: "Ask", Href: "/admin/ask/sessions"},
	{Key: navConnectors, Label: "Connectors", Href: "/admin/connectors"},
	{Key: navPrompts, Label: "Prompts", Href: "/admin/prompts"},
	{Key: navReports, Label: "Reports", Href: "/admin/reports"},
	{Key: navOperations, Label: "Operations", Href: operationsPath},
	{Key: navSettings, Label: "Settings", Href: settingsPath},
}

// sectionTabs are the tab rows the layout draws under the header for every
// page of a section.
var sectionTabs = map[string]tabSet{
	navAsk: {Label: "Ask", Tabs: []navTab{
		{Key: tabAskConversations, Label: "Conversations", Href: askListPath},
		{Key: tabAskFeedback, Label: "Feedback", Href: askFeedbackPath},
	}},
	navOperations: {Label: "Operations", Tabs: []navTab{
		{Key: tabOperationsStatus, Label: "Status", Href: operationsPath},
		{Key: tabOperationsJobs, Label: "Jobs and maintenance", Href: jobsPath},
		{Key: tabOperationsFailures, Label: "Failures", Href: failuresPath},
		{Key: tabOperationsAudit, Label: "Audit log", Href: auditPath},
	}},
	navSettings: {Label: "Settings", Tabs: []navTab{
		{Key: tabSettingsSetup, Label: "Setup checklist", Href: settingsPath},
		{Key: tabSettingsOrganization, Label: "Organization", Href: organizationPath},
		{Key: tabSettingsModels, Label: "Models", Href: modelsPath},
		{Key: tabSettingsAccess, Label: "Access", Href: accessPath},
	}},
}

// pageData is what the layout needs from every admin page.
type pageData struct {
	Title      string
	CurrentNav string
	// CurrentTab picks the tab of CurrentNav's section, where it has tabs.
	CurrentTab string
	Principal  auth.Principal
	// OrgName is the saved organization name, shown beside the brand.
	OrgName    string
	Version    string
	GrafanaURL string
	Stylesheet string
	Script     string
	// SignOutURL and SignOutToken are the header's sign-out form, when the
	// built-in sign-in is active.
	SignOutURL   string
	SignOutToken string
	// InSetup and BootstrapAdminEnabled raise the banner every admin page
	// but the setup checklist shows.
	InSetup               bool
	BootstrapAdminEnabled bool
}

// MainNav is the primary navigation with the current section marked.
func (p pageData) MainNav() []navItem {
	items := make([]navItem, len(mainNav))
	for index, item := range mainNav {
		item.Current = item.Key == p.CurrentNav
		items[index] = item
	}
	return items
}

// SectionTabs is the current section's tab row, nil when it has none.
func (p pageData) SectionTabs() *tabSet {
	set, ok := sectionTabs[p.CurrentNav]
	if !ok {
		return nil
	}
	tabs := make([]navTab, len(set.Tabs))
	for index, tab := range set.Tabs {
		tab.Current = tab.Key == p.CurrentTab || (p.CurrentTab == "" && index == 0)
		tabs[index] = tab
	}
	return &tabSet{Label: set.Label, Tabs: tabs}
}

// banner is the one line the shell shows above a page.
type banner struct {
	Bad      bool
	Text     string
	LinkText string
	Href     string
}

// Banner is what the install needs from an administrator, nil when nothing.
// The setup checklist says all of it itself, so it shows none.
func (p pageData) Banner() *banner {
	if p.CurrentNav == navSettings && (p.CurrentTab == "" || p.CurrentTab == tabSettingsSetup) {
		return nil
	}
	switch {
	case p.InSetup:
		return &banner{
			Text:     "Diderot is still being set up: only administrators can use Ask.",
			LinkText: "Continue setup", Href: settingsPath,
		}
	case p.BootstrapAdminEnabled:
		return &banner{
			Bad:      true,
			Text:     "The bootstrap admin is on: anyone who can reach this page can sign in as admin / admin.",
			LinkText: "Turn it off", Href: settingsPath + "#bootstrap-admin",
		}
	}
	return nil
}

func (s *Server) page(r *http.Request, title, nav string) pageData {
	p, _ := PrincipalFromContext(r.Context())
	data := pageData{
		Title:        title,
		CurrentNav:   nav,
		Principal:    p,
		Version:      s.version,
		SignOutURL:   s.signOutURL(),
		SignOutToken: signOutTokenFromContext(r.Context()),
	}
	if s.setup != nil {
		org := s.setup.Org()
		data.OrgName = org.Name
		data.InSetup = !org.Live()
		data.BootstrapAdminEnabled = org.BootstrapAdminEnabled
		data.GrafanaURL = org.DashboardURL
	}
	return data
}

type signOutTokenKey struct{}

// withSignOutToken issues the sign-out form's double-submit token, when the
// built-in sign-in is active, and carries it to the page.
func (s *Server) withSignOutToken(w http.ResponseWriter, r *http.Request) *http.Request {
	if s.login == nil {
		return r
	}
	token, err := issueCSRFTokenCookie(w, r, signOutCSRFCookieName, "/", s.sessionCookieSecure)
	if err != nil {
		s.logger.Error("sign-out token failed", "err", err)
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), signOutTokenKey{}, token))
}

func signOutTokenFromContext(ctx context.Context) string {
	token, _ := ctx.Value(signOutTokenKey{}).(string)
	return token
}
