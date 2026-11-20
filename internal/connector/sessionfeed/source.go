package sessionfeed

import (
	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/source"
)

// Info is the session feed's facts as a source. Its events are every agent
// session, whether the feed or a push delivered it.
func (Connector) Info() source.Info {
	return source.Info{
		EventSource: "agent_session", Title: "Session feed", ResultNoun: "Work session",
		Summary: "Summaries of coding-agent sessions from a session feed.",
		ToolDescription: "canonical summaries of redacted coding-agent session transcripts. " +
			"Use agent_session for technical, engineering, research, review, and operational work, " +
			"including reasoning and resolutions that may not appear in slack.",
		ActorRole:         identity.RoleSessionOwner,
		IdentityNamespace: func(string) string { return "agent_session" },
	}
}
