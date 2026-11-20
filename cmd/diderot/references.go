package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/events"
)

// sourceReferenceResolver is the production exact-reference adapter. It
// resolves a handle through authoritative events indexes, applies the
// response audience scope, and only then calls the existing source expander.
// It never calls retrieve: an explicit handle is a prerequisite, not a search
// hint.
type sourceReferenceResolver struct {
	lookup   exactReferenceLookup
	expander ask.SourceExpander
}

type exactReferenceLookup interface {
	SlackEventID(context.Context, string, string) (int64, error)
	SlackEventContent(context.Context, int64) (source, url, body string, err error)
	LinearEventID(context.Context, string) (int64, error)
	ZendeskEventID(context.Context, string, []string) (int64, error)
	SessionEventID(context.Context, string) (int64, error)
	EventIDReference(context.Context, int64) (int64, error)
	ChunkEventID(context.Context, int64) (int64, error)
	EventScope(context.Context, int64) (string, error)
}

func newSourceReferenceResolver(lookup exactReferenceLookup, expander ask.SourceExpander) *sourceReferenceResolver {
	return &sourceReferenceResolver{lookup: lookup, expander: expander}
}

func (r *sourceReferenceResolver) PrepareReferences(
	ctx context.Context,
	refs []ask.CorpusReference,
	allowedScopes []string,
) (ask.ReferencePrerequisiteState, error) {
	ctx = ask.WithAllowedScopes(ctx, allowedScopes)
	state := ask.ReferencePrerequisiteState{References: append([]ask.CorpusReference(nil), refs...)}
	if r.lookup == nil || r.expander == nil {
		return state, errors.New("exact reference prerequisites are not configured")
	}
	for _, ref := range refs {
		eventID, err := r.resolveEventID(ctx, ref)
		if err != nil {
			state.Unavailable = append(state.Unavailable, ref)
			if ref.Role == ask.ReferencePrimary {
				state.PrimaryBlocked = true
			}
			continue
		}
		scope, err := r.lookup.EventScope(ctx, eventID)
		if err != nil || !scopeAllowed(scope, allowedScopes) {
			// Missing, deleted, and out-of-audience rows intentionally take
			// exactly the same state path. No URL or scope detail is returned
			// to the model or caller.
			state.Unavailable = append(state.Unavailable, ref)
			if ref.Role == ask.ReferencePrimary {
				state.PrimaryBlocked = true
			}
			continue
		}
		if ref.Kind == ask.ReferenceSlackPermalink {
			// A permalink identifies one exact message, while Slack expand
			// intentionally adds the surrounding thread or channel window.
			// Preserve both contracts: put the canonical message first so it
			// cannot disappear in a truncated expansion, then add conversation
			// context when expansion succeeds.
			source, url, exact, readErr := r.lookup.SlackEventContent(ctx, eventID)
			if readErr != nil || exact == "" {
				state.Unavailable = append(state.Unavailable, ref)
				if ref.Role == ask.ReferencePrimary {
					state.PrimaryBlocked = true
				}
				continue
			}
			content := "Exact linked Slack message:\n" + exact
			truncated := false
			if expanded, expandErr := r.expander.Expand(ctx, eventID); expandErr == nil && expanded.Content != "" {
				// The whole reference shares expand's budget, but only the
				// context gives way: the exact message is never cut, so it
				// is reserved and the context is dropped if nothing fits.
				const contextHeader = "\n\nExpanded Slack conversation context:\n"
				window, capped := ask.CapExpandContent(expanded.Content, len(content)+len(contextHeader))
				if window != "" {
					content += contextHeader + window
				}
				truncated = expanded.Truncated || capped
			}
			state.Resolved = append(state.Resolved, ask.ResolvedReference{
				Reference: ref, EventID: eventID, Source: source, URL: url, Content: content, Truncated: truncated,
			})
			continue
		}
		result, err := r.expander.Expand(ctx, eventID)
		if err != nil || result.Content == "" {
			state.Unavailable = append(state.Unavailable, ref)
			if ref.Role == ask.ReferencePrimary {
				state.PrimaryBlocked = true
			}
			continue
		}
		content, capped := ask.CapExpandContent(result.Content, 0)
		state.Resolved = append(state.Resolved, ask.ResolvedReference{
			Reference: ref,
			EventID:   eventID,
			Source:    result.Source,
			Content:   content,
			Truncated: result.Truncated || capped,
		})
	}
	return state, nil
}

func (r *sourceReferenceResolver) resolveEventID(ctx context.Context, ref ask.CorpusReference) (int64, error) {
	switch ref.Kind {
	case ask.ReferenceSlackPermalink:
		return r.lookup.SlackEventID(ctx, ref.ChannelID, ref.MessageTS)
	case ask.ReferenceLinearIssue:
		return r.lookup.LinearEventID(ctx, ref.Value)
	case ask.ReferenceZendeskURL:
		return r.lookup.ZendeskEventID(ctx, ref.Value, ask.AllowedScopesFromContext(ctx))
	case ask.ReferenceSessionUUID:
		return r.lookup.SessionEventID(ctx, ref.Value)
	case ask.ReferenceEventID:
		id, err := strconv.ParseInt(ref.Value, 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("invalid event id")
		}
		return r.lookup.EventIDReference(ctx, id)
	case ask.ReferenceChunkID:
		id, err := strconv.ParseInt(ref.Value, 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("invalid chunk id")
		}
		return r.lookup.ChunkEventID(ctx, id)
	default:
		return 0, fmt.Errorf("unsupported exact reference kind %q", ref.Kind)
	}
}

func scopeAllowed(scope string, allowedScopes []string) bool {
	return events.ScopeAllowed(scope, allowedScopes)
}

var _ ask.ReferencePrerequisiteResolver = (*sourceReferenceResolver)(nil)
