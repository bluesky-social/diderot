package ask

import (
	"context"
	"slices"

	"github.com/bluesky-social/diderot/internal/retrieve"
)

// Visibility-scope plumbing for the ask loop. The per-turn
// allowedScopes set is computed by the surface (slack consumer:
// from the channel the bot was mentioned in) and travels to the
// tool executors via context — the same pattern as the citation
// accumulator. The model never sees or controls it: RetrieveTool /
// The native list/expand tools fish it out of ctx and inject it below the
// model's tool arguments.
//
// nil (no value installed) grants no access for standalone tool users.
// AskService.ExecuteTurn rejects a missing/empty boundary on every surface
// and installs the request's explicit set before any tool can run.

// scopesCtxKey is the unexported context key for the per-turn
// allowed-scopes set.
type scopesCtxKey struct{}

// WithAllowedScopes installs the visibility boundary for tool calls. The Ask
// state machine uses it after ingress has supplied TurnRequest.AllowedScopes;
// standalone tool callers may use it directly. Retrieve/list/expand inject the
// value below the model's arguments, so the model can narrow but never widen
// the boundary.
func WithAllowedScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, scopesCtxKey{}, slices.Clone(scopes))
}

// ScopeGranter adds to a reader's scopes the scopes sources grant to them
// (source.ScopeGranter).
type ScopeGranter interface {
	GrantScopes(ctx context.Context, scopes []string) []string
}

type answeringContextKey struct{}

// withConversationsBeingAnswered records the conversation a run is answering
// (a Slack thread, for a session that has one) so that retrieve and list
// leave it out. The thread is given to the model directly as thread context;
// coming back from a search as well, it would be the question posing as
// evidence for its own answer.
func withConversationsBeingAnswered(ctx context.Context, conversations []retrieve.Conversation) context.Context {
	if len(conversations) == 0 {
		return ctx
	}
	return context.WithValue(ctx, answeringContextKey{}, conversations)
}

func conversationsBeingAnswered(ctx context.Context) []retrieve.Conversation {
	conversations, _ := ctx.Value(answeringContextKey{}).([]retrieve.Conversation)
	return conversations
}

// scopesFromContext returns the installed allowed-scopes set, or
// nil when the surface didn't install one.
func scopesFromContext(ctx context.Context) []string {
	return AllowedScopesFromContext(ctx)
}

func AllowedScopesFromContext(ctx context.Context) []string {
	s, _ := ctx.Value(scopesCtxKey{}).([]string)
	return slices.Clone(s)
}
