package ask

import (
	"strings"

	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/source"
)

// testSources are the facts the five sources' packages declare, in
// cmd/diderot's registration order, which these tests can't import (the
// Slack connector imports ask). cmd/diderot's source surface golden pins
// what the real registry gives the tools.
var testSources = source.Infos{
	{
		EventSource: "slack", Title: "Slack", ResultNoun: "Slack",
		ToolDescription: "indexed channel messages plus messages sent directly to Diderot; " +
			"one-to-one and group DMs between other people are never indexed.",
		ActorRole:         identity.RoleMessageAuthor,
		IdentityNamespace: prefixedNamespace("slack-bot:", "slack_bot", "slack_user"),
		Threads:           true,
	},
	{
		EventSource: "agent_session", Title: "Session feed", ResultNoun: "Work session",
		ToolDescription: "canonical summaries of redacted coding-agent session transcripts. " +
			"Use agent_session for technical, engineering, research, review, and operational work, " +
			"including reasoning and resolutions that may not appear in slack.",
		ActorRole:         identity.RoleSessionOwner,
		IdentityNamespace: func(string) string { return "agent_session" },
	},
	{
		EventSource: "notion", Title: "Notion", ResultNoun: "Notion",
		ToolDescription: "internal Notion pages: docs, runbooks, specs, design/product docs, and structured database rows. " +
			"Use notion for documented decisions, processes, specs, and reference material.",
		ActorRole:         identity.RoleLastEditor,
		IdentityNamespace: prefixedNamespace("notion-bot:", "notion_bot", "notion_user"),
	},
	{
		EventSource: "linear", Title: "Linear", ResultNoun: "Linear",
		ToolDescription:   "engineering issue tracker: tasks, bugs, projects, and their discussion threads.",
		ActorRole:         identity.RoleIssueCreator,
		IdentityNamespace: func(string) string { return "linear_user" },
	},
	{
		EventSource: "zendesk", Title: "Zendesk", ResultNoun: "Zendesk",
		ToolDescription: "indexed customer support tickets, individual replies/internal notes, and Help Center articles with available attachment content. " +
			"Use zendesk for support reports and guidance, not engineering work items.",
		ActorRole:         identity.RoleEventActor,
		IdentityNamespace: func(string) string { return "zendesk_user" },
	},
	{
		EventSource: "discord", Title: "Discord", ResultNoun: "Discord",
		ToolDescription:   "messages, threads and forum posts from the Discord server channels this deployment indexes.",
		ActorRole:         identity.RoleMessageAuthor,
		IdentityNamespace: func(string) string { return "discord_user" },
		Threads:           true,
	},
}

func prefixedNamespace(prefix, prefixed, otherwise string) func(string) string {
	return func(canonical string) string {
		if strings.HasPrefix(canonical, prefix) {
			return prefixed
		}
		return otherwise
	}
}
