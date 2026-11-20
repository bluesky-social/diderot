package adminui

import (
	"sync"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

// promptGroup is one job a set of prompts serves, as the Prompts page lists
// them: its heading, one sentence on when the prompts are used, and each
// prompt's key and name, in reading order.
type promptGroup struct {
	ID, Title, Summary string
	Prompts            []namedPrompt
}

type namedPrompt struct{ Key, Name string }

// promptGroups name every prompt Diderot reads. A key a pushed catalog adds
// that is not here lists under otherPrompts by its key.
var promptGroups = []promptGroup{
	{ID: "ask", Title: "Ask: how Diderot answers", Summary: "Every answer starts from these, wherever the question was asked.", Prompts: []namedPrompt{
		{promptcatalog.AskBase, "Role and research"},
		{promptcatalog.AskOrganization, "About your organization"},
		{promptcatalog.AskAnswerDiscipline, "Answer rules"},
		{promptcatalog.AskTerminalOutput, "Handing back the answer"},
	}},
	{ID: "surfaces", Title: "Ask in Slack and over the API", Summary: "How an answer is formatted for where it is read.", Prompts: []namedPrompt{
		{promptcatalog.AskSurfaceSlack, "Answers in Slack"},
		{promptcatalog.AskSurfaceHTTP, "Answers over the API and MCP"},
	}},
	{ID: "observability", Title: "Observability assistant", Summary: "Used when Ask hands a question about live metrics, logs, traces or errors to its observability assistant, which it can do only while observability tools are set up.", Prompts: []namedPrompt{
		{promptcatalog.AskObservabilityDelegation, "Handing a question to the assistant"},
		{promptcatalog.AskObservabilityRole, "The assistant's role"},
		{promptcatalog.AskObservabilityDiscipline, "Investigation rules"},
		{promptcatalog.AskObservabilityReport, "Report format"},
		{promptcatalog.AskObservabilitySentryDiscipline, "Using Sentry"},
		{promptcatalog.AskObservabilityGrafanaDiscipline, "Using Grafana and Tempo"},
		{promptcatalog.AskObservabilityGrafanaReport, "Grafana panel images"},
		{promptcatalog.AskObservabilityAlert, "Investigating an alert"},
		{promptcatalog.AskObservabilitySLO, "Investigating an SLO"},
		{promptcatalog.AskObservabilityIncident, "Investigating an incident"},
		{promptcatalog.AskObservabilitySentry, "Investigating a Sentry issue"},
		{promptcatalog.AskObservabilityExplore, "Finding the right data"},
		{promptcatalog.AskObservabilityPromQL, "Writing PromQL"},
		{promptcatalog.AskObservabilityLogQL, "Writing LogQL"},
		{promptcatalog.AskObservabilityTraceQL, "Writing TraceQL"},
	}},
	{ID: "reports", Title: "Scheduled reports", Summary: "The built-in questions a scheduled report can ask.", Prompts: []namedPrompt{
		{promptcatalog.ScheduledIndividualDaily, "Daily update for one person"},
		{promptcatalog.ScheduledTeamWeekly, "Weekly team update"},
	}},
	{ID: "summaries", Title: "Summaries and titles", Summary: "The titles and summaries Diderot writes for conversations and agent sessions.", Prompts: []namedPrompt{
		{promptcatalog.AskTitle, "Conversation titles"},
		{promptcatalog.SessionTitle, "Agent session titles"},
		{promptcatalog.SummarySessionMap, "Agent session summaries, part by part"},
		{promptcatalog.SummarySessionReduce, "Agent session summaries, combined"},
	}},
	{ID: "files", Title: "Documents and images", Summary: "Used when Diderot reads the files and images people share.", Prompts: []namedPrompt{
		{promptcatalog.ConnectorDocumentExtract, "Reading scanned pages"},
		{promptcatalog.ConnectorImageCaption, "Describing images"},
	}},
}

var otherPrompts = promptGroup{ID: "other", Title: "Other prompts", Summary: "Prompts in the live version that this version of Diderot does not use."}

var promptNames = func() map[string]struct{ name, group string } {
	names := map[string]struct{ name, group string }{}
	for _, group := range promptGroups {
		for _, prompt := range group.Prompts {
			names[prompt.Key] = struct{ name, group string }{prompt.Name, group.ID}
		}
	}
	return names
}()

// promptName is how pages name a prompt: its name, or else its key.
func promptName(key string) string {
	if named, ok := promptNames[key]; ok {
		return named.name
	}
	return key
}

// shippedDescriptions are the descriptions of the example prompts this build
// ships, by key.
var shippedDescriptions = sync.OnceValue(func() map[string]string {
	descriptions := map[string]string{}
	for _, prompt := range promptcatalog.MustEmbedded().Prompts {
		descriptions[prompt.Key] = prompt.Description
	}
	return descriptions
})

// promptDescription says what a prompt is for. A version stores the
// descriptions it was saved with, and a change to descriptions alone never
// makes a new version live, so for a prompt this build ships the build's own
// description is the current one; the stored one serves any other prompt.
func promptDescription(prompt promptcatalog.Prompt) string {
	if description := shippedDescriptions()[prompt.Key]; description != "" {
		return description
	}
	return prompt.Description
}

// promptGroupOf is the group a prompt lists under.
func promptGroupOf(key string) promptGroup {
	if named, ok := promptNames[key]; ok {
		for _, group := range promptGroups {
			if group.ID == named.group {
				return group
			}
		}
	}
	return otherPrompts
}

// promptVariableMeanings say what each variable is replaced with.
var promptVariableMeanings = map[string]string{
	promptcatalog.VarOrgName:            `Your organization's name, from Settings › Organization. Until one is saved, "the organization".`,
	promptcatalog.VarOrgDescription:     "The one-line description from Settings › Organization. Empty until one is saved.",
	promptcatalog.VarMaxTitleRunes:      "The longest title, in characters, that a title prompt may ask for.",
	promptcatalog.VarSources:            "One line for each kind of source Diderot can search, saying what it holds.",
	promptcatalog.VarSlackWorkspaceName: "Your Slack workspace's name, which the Slack connector's test finds. Empty while Slack is off.",
	promptcatalog.VarGrafanaDatasources: "One line naming the Grafana datasources the observability assistant's queries always use, which the Grafana connection's settings pin. Empty when none is pinned.",
}
