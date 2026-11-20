package slack

import (
	"context"
	"strings"

	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/source"
)

// Info is Slack's facts as a source.
func (Connector) Info() source.Info {
	return source.Info{
		EventSource: "slack", Title: "Slack", ResultNoun: "Slack",
		Summary: "Messages in the Slack channels the bot is in.",
		ToolDescription: "indexed channel messages plus messages sent directly to Diderot; " +
			"one-to-one and group DMs between other people are never indexed.",
		ActorRole: identity.RoleMessageAuthor,
		IdentityNamespace: func(canonical string) string {
			if strings.HasPrefix(canonical, "slack-bot:") {
				return "slack_bot"
			}
			return "slack_user"
		},
		Threads: true,
	}
}

// Section is the channel counts on Slack's page, which lead to its
// channels.
func (Connector) Section() string { return "slack-channels" }

// Expand returns the message's thread, or for a message outside a thread
// the channel messages around it, as far as scopes may read them.
func (Connector) Expand(ctx context.Context, deps source.Deps, eventID int64, scopes []string) (source.Expanded, error) {
	return NewExpander(deps.Pool).Expand(ctx, eventID, scopes)
}
