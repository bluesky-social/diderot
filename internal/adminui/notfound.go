package adminui

import "net/http"

var notFoundPage = mustPage("notfound.html")

// notFoundData is the one page every missing thing shows: an address
// nothing is at, or a conversation, report, prompt, channel or connector
// that does not exist.
type notFoundData struct {
	pageData
	// Heading names what is missing: "Report not found".
	Heading string
	// Detail says so in a sentence.
	Detail string
	// BackHref and BackLabel are the way on from here.
	BackHref  string
	BackLabel string
}

// missing describes one kind of thing a page can fail to find.
type missing struct {
	nav       string
	heading   string
	detail    string
	backHref  string
	backLabel string
}

var (
	missingPage         = missing{"", "Page not found", "Nothing is at this address.", "/admin/", "Go to the overview"}
	missingConversation = missing{navAsk, "Conversation not found", "No conversation has this address. It may have been deleted, or the link may be cut short.", askListPath, "All conversations"}
	missingReport       = missing{navReports, "Report not found", "No report has this address. It may have been deleted.", "/admin/reports", "All reports"}
	missingPrompt       = missing{navPrompts, "Prompt not found", "The live prompts have none with this key.", promptsPath, "All prompts"}
	missingVersion      = missing{navPrompts, "Version not found", "No version of the prompts has this number.", promptsPath + "/versions", "Version history"}
	missingLegacyPrompt = missing{navPrompts, "Prompt not found", "No earlier Ask prompt has this number.", promptsPath, "All prompts"}
	missingChannel      = missing{navConnectors, "Channel not found", "Diderot knows no Slack channel with this ID. A channel appears once the bot is in it.", slackChannelsPath, "All channels"}
	missingConnector    = missing{navConnectors, "Connector not found", "This version of Diderot has no connector with this name.", "/admin/connectors", "All connectors"}
	missingConnection   = missing{navConnectors, "Connection not found", "No tool connection has this name. It may have been removed.", "/admin/connectors#connections", "All connections"}
	missingProvider     = missing{navSettings, "Provider not found", "No model provider has this name. It may have been removed.", modelsPath, "Models"}
	missingModel        = missing{navSettings, "Model not found", "No job has been given this model, or it was removed.", modelsPath, "Models"}
)

// renderNotFound answers 404 with the shared page, in the shell.
func (s *Server) renderNotFound(w http.ResponseWriter, r *http.Request, what missing) {
	s.renderStatus(w, notFoundPage, notFoundData{
		pageData: s.page(r, what.heading, what.nav),
		Heading:  what.heading, Detail: what.detail, BackHref: what.backHref, BackLabel: what.backLabel,
	}, http.StatusNotFound)
}

// browsing reports a request that shows a page, as opposed to a form post.
func browsing(r *http.Request) bool {
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}
