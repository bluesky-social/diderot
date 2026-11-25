package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/asktool"
	"github.com/bluesky-social/diderot/internal/attachments/docs"
	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/automation"
	"github.com/bluesky-social/diderot/internal/config"
	discordcx "github.com/bluesky-social/diderot/internal/connector/discord"
	linearcx "github.com/bluesky-social/diderot/internal/connector/linear"
	markdowncx "github.com/bluesky-social/diderot/internal/connector/markdown"
	notioncx "github.com/bluesky-social/diderot/internal/connector/notion"
	"github.com/bluesky-social/diderot/internal/connector/personal"
	"github.com/bluesky-social/diderot/internal/connector/sessionfeed"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	zendeskcx "github.com/bluesky-social/diderot/internal/connector/zendesk"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// sourceRegistry is every source this binary indexes, in the order the tools
// and pages list them; the corpus tools' sources filter offers them in this
// order. The chat sources come first, together: Ask searches a few sources
// a round in this order, and a chat source listed last went unsearched. Slack's Test reads the slack_users directory through pool. Slack and
// the session feed run with what the leader term lends them through term,
// and Slack with slack's wiring too; the session feed expands through feed,
// the feed's client while that connector is enabled, and push, the pushed
// sessions' store (nil unless push ingest is on); its page says what
// summaries says stops them. fileStorage says the
// deployment has the file storage that Slack's and Zendesk's Index documents
// needs, which their Tests check.
func sourceRegistry(pool *pgxpool.Pool, term *settings.Live[leaderTerm], slack *slackWiring, feed func() *sessionfeed.Client,
	push *sessioningest.PushStore, summaries *sessioningest.SummaryControl, fileStorage bool,
) source.Registry {
	return source.NewRegistry(
		&slackSource{Connector: slackcx.Connector{Pool: pool, FileStorage: fileStorage}, term: term, wiring: slack},
		&discordSource{Connector: discordcx.Connector{FileStorage: fileStorage}, term: term},
		&sessionFeedSource{term: term, feed: feed, push: push, summaries: summaries},
		notioncx.Connector{},
		linearcx.Connector{},
		zendeskcx.Connector{FileStorage: fileStorage},
		markdowncx.Connector{},
		personal.IMAP{},
		personal.CalDAV{},
	)
}

// sourceChannels finds the channels a list channels entry names among the
// sources that have channels of their own (source.ChannelFinder).
type sourceChannels struct {
	sources source.Registry
	deps    source.Deps
}

func (c sourceChannels) FindChannels(ctx context.Context, entry string) ([]retrieve.ChannelMatch, error) {
	var matches []retrieve.ChannelMatch
	for _, src := range c.sources.Sources() {
		finder, ok := src.(source.ChannelFinder)
		if !ok {
			continue
		}
		found, err := finder.FindChannels(ctx, c.deps.For(src.Name()), entry)
		if err != nil {
			return nil, fmt.Errorf("%s channels: %w", src.Name(), err)
		}
		for _, ch := range found {
			matches = append(matches, retrieve.ChannelMatch{Source: src.Info().EventSource, ExternalIDPrefix: ch.ExternalIDPrefix, Scope: ch.Scope})
		}
	}
	return matches, nil
}

// connectorRegistry is every connector this binary knows, in the order pages
// and the API list them: the sources, then the tools, then the automations.
func connectorRegistry(sources source.Registry, tools asktool.Registry, automations automation.Registry) *settings.Registry {
	var defs []settings.Definition
	for _, src := range sources.Sources() {
		defs = append(defs, src)
	}
	for _, tool := range tools.Tools() {
		defs = append(defs, tool)
	}
	for _, automation := range automations.Automations() {
		defs = append(defs, automation)
	}
	return settings.NewRegistry(defs...)
}

// sourceRunner runs a source under the settings supervisor with its deps,
// starting its base metrics and counting a run that ends with an error.
type sourceRunner struct {
	source.Source
	deps source.Deps
}

func (r sourceRunner) Run(ctx context.Context, s settings.Resolved) error {
	r.deps.Metrics.Started()
	err := r.Source.Run(ctx, r.deps, s)
	if err != nil && ctx.Err() == nil {
		r.deps.Metrics.Failed()
	}
	return err
}

// sourceFiles lends one source's fetchers to the attachment workers: its
// image fetcher to the caption worker's slot for the source, and its
// document fetcher to a document worker of its own.
type sourceFiles struct {
	eventSource string
	images      *lentImages
	cfg         *config.Config
	models      *inferenceModels
	pool        *pgxpool.Pool
	mirror      files.S3Mirror
	logger      *slog.Logger
}

func (f *sourceFiles) LendImages(run context.Context, fetcher files.SourceFetcher) func() {
	return f.images.Lend(run, fetcher)
}

func (f *sourceFiles) AddDocumentWorker(sup *worker.Supervisor, name string, gate docs.GateChecker, fetcher files.Fetcher) func() {
	return registerDocumentWorker(sup, name, f.cfg, f.models, f.pool, f.mirror, f.logger, gate,
		map[string]files.Fetcher{f.eventSource: fetcher})
}
