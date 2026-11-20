package main

import (
	"context"
	"log/slog"
	"slices"

	"github.com/bluesky-social/diderot/internal/askchat"
	discordcx "github.com/bluesky-social/diderot/internal/connector/discord"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// discordSource is the Discord source, whose Run answers in Discord with
// what the leader term lends it: its Ask, and the thread-summary model.
type discordSource struct {
	discordcx.Connector
	term *settings.Live[leaderTerm]
}

func (s *discordSource) Run(ctx context.Context, deps source.Deps, resolved settings.Resolved) error {
	term, ok := s.term.Get()
	if !ok {
		return errNoLeaderTerm
	}
	var ask *discordcx.Ask
	if term.ask != nil {
		ask = &discordcx.Ask{Router: term.ask}
		if threadSummariesEnabled(term.models) {
			sum, err := askchat.NewSummarizer(summarizerConfig(term.models.summarizer, discordcx.SummaryPrompt))
			if err != nil {
				deps.Logger.Error("summarizer: init failed; Discord's summary commands will say they are not set up", "err", err)
			} else {
				ask.Summarizer = sum
			}
		}
	}
	return s.RunWithAsk(ctx, deps, resolved, ask)
}

// sourceScopeGrants adds to a reader's scopes the scopes the sources grant
// them (source.ScopeGranter): a Discord channel's to the readers its
// visibility names. A source whose grants can't be read grants nothing.
type sourceScopeGrants struct {
	sources source.Registry
	deps    source.Deps
	logger  *slog.Logger
}

func (g sourceScopeGrants) GrantScopes(ctx context.Context, scopes []string) []string {
	out := slices.Clone(scopes)
	for _, src := range g.sources.Sources() {
		granter, ok := src.(source.ScopeGranter)
		if !ok {
			continue
		}
		granted, err := granter.GrantScopes(ctx, g.deps.For(src.Name()), scopes)
		if err != nil {
			g.logger.Warn("scopes: reading a source's grants failed; it grants none", "source", src.Name(), "err", err)
			continue
		}
		for _, scope := range granted {
			if !slices.Contains(out, scope) {
				out = append(out, scope)
			}
		}
	}
	return out
}
