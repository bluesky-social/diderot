package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/events"
	modelidentity "github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/source"
)

// sourceExpander is the production ask.SourceExpander: it looks up an
// event's source and actor and hands the event to that source's Expand. An
// event of a source this binary doesn't register is not expandable.
type sourceExpander struct {
	sources source.Registry
	// deps is what each source's Expand reads with; Sink also looks up the
	// event.
	deps source.Deps
}

func newSourceExpander(sources source.Registry, deps source.Deps) *sourceExpander {
	return &sourceExpander{sources: sources, deps: deps}
}

// Expand resolves event_id to its source and that source's fuller context
// for the turn's scopes. An event the turn's scopes don't reach is not
// found, whatever its source checks itself. Otherwise the result always
// names the source and the event's actor, so a model or HTTP caller gets a
// structured "not supported" or "not found" rather than a bare error.
func (e *sourceExpander) Expand(ctx context.Context, eventID int64) (ask.ExpandResult, error) {
	if e.deps.Sink == nil {
		return ask.ExpandResult{}, errors.New("expand: no event lookup configured")
	}
	meta, err := e.deps.Sink.IdentityForEvent(ctx, eventID)
	switch {
	case errors.Is(err, events.ErrEventNotFound):
		return ask.ExpandResult{}, ask.ErrExpandEventNotFound
	case err != nil:
		return ask.ExpandResult{}, fmt.Errorf("event identity lookup: %w", err)
	}
	scopes := ask.AllowedScopesFromContext(ctx)
	if !events.ScopeAllowed(meta.Scope, scopes) {
		return ask.ExpandResult{}, ask.ErrExpandEventNotFound
	}
	result := ask.ExpandResult{
		Source: meta.Source,
		Actor:  modelidentity.FromEvent(e.sources.Infos().Of(meta.Source), meta.Actor, meta.ActorCanonical, meta.ActorID),
	}
	src, ok := e.sources.Lookup(meta.Source)
	if !ok {
		return result, ask.ErrSourceNotExpandable
	}
	expanded, err := src.Expand(ctx, e.deps.For(src.Name()), eventID, scopes)
	result.Content, result.Truncated = expanded.Content, expanded.Truncated
	result.Attachments = expanded.Attachments
	if err == nil && result.Attachments == nil && e.deps.Pool != nil {
		// Best-effort, like a source's own list: the content stands without it.
		result.Attachments, _ = events.ListAttachments(ctx, e.deps.Pool, []int64{eventID})
	}
	switch {
	case errors.Is(err, source.ErrNotExpandable):
		err = ask.ErrSourceNotExpandable
	case errors.Is(err, events.ErrEventNotFound):
		err = ask.ErrExpandEventNotFound
	}
	return result, err
}
