package discord

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// SectionName is the channel section of Discord's page.
const SectionName = "discord-channels"

// Info is Discord's facts as a source.
func (Connector) Info() source.Info {
	return source.Info{
		EventSource: ConnectorName, Title: "Discord", ResultNoun: "Discord",
		Summary:         "Messages in the Discord channels you choose.",
		ToolDescription: "messages, threads and forum posts from the Discord server channels this deployment indexes.",
		ActorRole:       identity.RoleMessageAuthor,
		IdentityNamespace: func(string) string {
			return "discord_user"
		},
		Threads: true,
	}
}

// Section is the channel chooser on Discord's page.
func (Connector) Section() string { return SectionName }

// Intervals of the leader's workers; tests shorten them.
var (
	controlInterval   = 15 * time.Second
	tailInterval      = 5 * time.Minute
	directoryInterval = 15 * time.Minute
)

// Run holds the source's workers for a leader term without Ask.
func (c Connector) Run(ctx context.Context, deps source.Deps, resolved settings.Resolved) error {
	return c.RunWithAsk(ctx, deps, resolved, nil)
}

// RunWithAsk holds the source's workers for a leader term: the directory
// reader, the control loop that acts on the administrators' channel
// choices, the tail and daily re-read, and the gateway with its live
// events. It lends its attachment fetcher to the caption worker and, while
// Index documents is on, runs a document worker. With ask, it answers in
// Discord too: it registers Ask's commands, routes the gateway's mentions,
// direct messages and interactions to Ask, and sweeps the progress of
// turns that died.
func (c Connector) RunWithAsk(ctx context.Context, deps source.Deps, resolved settings.Resolved, ask *Ask) error {
	token := resolved.Secret(FieldBotToken)
	if token == "" {
		return errNoToken
	}
	client := NewClient(c.APIBase, token)
	r := newReader(client, deps, resolved.Int(FieldHistoryDays))
	if err := r.load(ctx); err != nil {
		return err
	}
	bot, err := client.currentUser(ctx)
	if err != nil {
		return fmt.Errorf("discord: read the bot: %w", err)
	}
	// The bot's own posts are Ask's answers, which are never indexed.
	r.self = bot.ID
	var asker *asker
	if ask != nil && ask.Router != nil {
		if asker, err = newAsker(ctx, client, deps, r, ask); err != nil {
			return err
		}
		defer asker.wait()
	}
	// Until this run's gateway connects, live updates are not running.
	saveGatewayStatus(ctx, deps, gatewayStatus{})
	fetcher := NewFileFetcher(client)
	release := deps.Files.LendImages(ctx, fetcher)
	defer release()

	sup := worker.NewSupervisor(deps.Logger, deps.Metrics)
	refresh := make(chan struct{}, 1)
	requestRefresh := func() {
		select {
		case refresh <- struct{}{}:
		default:
		}
	}
	sup.AddFunc("discord-directory", func(ctx context.Context) error {
		ticker := time.NewTicker(directoryInterval)
		defer ticker.Stop()
		for {
			if err := r.refreshDirectory(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				deps.Logger.Warn("discord: reading servers and channels failed", "err", err)
				deps.Metrics.Failed()
			}
			// Let a burst of channel changes settle before reading again.
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			case <-refresh:
				if sleep(ctx, 5*time.Second) != nil {
					return nil
				}
			}
		}
	})
	sup.AddRunner(worker.Runner{Name: "discord-control", Interval: controlInterval, BootTick: true, Run: r.control})
	// The tail runs at once too: messages posted while no run was
	// connected are read before the first interval.
	sup.AddRunner(worker.Runner{Name: "discord-tail", Interval: tailInterval, BootTick: true, Run: r.tailAll})
	events := make(chan dispatch, 1024)
	l := &live{r: r, ask: asker, refreshDirectory: requestRefresh, reacted: map[[2]string]bool{}}
	gw := &gateway{
		token: token, intents: gatewayIntents, url: client.gatewayURL, logger: deps.Logger, events: events,
		status: func(s gatewayStatus) { saveGatewayStatus(ctx, deps, s) },
	}
	sup.AddFunc("discord-gateway", func(ctx context.Context) error {
		err := gw.run(ctx)
		var fatal *FatalGatewayError
		if errors.As(err, &fatal) {
			deps.Logger.Error("discord: live updates stopped", "err", err)
			deps.Metrics.Failed()
			// Keep the run: history reads go on, and the page says why
			// live updates stopped.
			<-ctx.Done()
		}
		return nil
	})
	sup.AddFunc("discord-live", func(ctx context.Context) error {
		l.run(ctx, events)
		return nil
	})
	if resolved.Bool(FieldDocIngest) {
		closeDocuments := deps.Files.AddDocumentWorker(sup, "discord-doc-extract", documentGate{}, fetcher)
		defer closeDocuments()
	}
	if asker != nil {
		// Registering the same commands again changes nothing, so it is
		// done at every start, and again now and then in case it failed.
		sup.AddRunner(worker.Runner{Name: "discord-ask-commands", Interval: 6 * time.Hour, BootTick: true, Run: asker.registerCommands})
		sup.AddFunc("discord-ask-sweep", func(ctx context.Context) error {
			asker.engine.RunSweep(ctx, nil)
			return nil
		})
	}
	deps.Logger.Info("discord connector running", "history_days", resolved.Int(FieldHistoryDays),
		"doc_ingest", resolved.Bool(FieldDocIngest), "ask", asker != nil)
	sup.Run(ctx)
	return nil
}

// saveGatewayStatus records a change of the live connection.
func saveGatewayStatus(ctx context.Context, deps source.Deps, s gatewayStatus) {
	var state GatewayState
	err := deps.State.Update(ctx, gatewayKey, &state, func(bool) error {
		if state.Connected != s.Connected || state.Since.IsZero() {
			state.Since = time.Now().UTC()
		}
		state.Connected, state.Error, state.Fatal = s.Connected, "", false
		if s.Err != nil {
			state.Error = s.Err.Error()
			var fatal *FatalGatewayError
			if errors.As(s.Err, &fatal) {
				state.Error, state.Fatal = fatal.Reason, true
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		deps.Logger.Warn("discord: saving the gateway status failed", "err", err)
	}
}

// documentGate is the per-channel opt-in of the document worker. Discord
// documents are opted in by the connector's Index documents setting, so the
// worker never asks it.
type documentGate struct{}

func (documentGate) DocIngestEnabled(context.Context, string) (bool, error) { return false, nil }

// Backfill describes the page's re-read of every chosen channel.
func (Connector) Backfill() source.Backfill {
	return source.Backfill{
		Summary: "Read the whole history of every chosen channel again, as far as the history setting reaches. Edits and deletions the live connection missed are corrected.",
		Confirm: "Read every chosen channel again", Action: "Read again",
	}
}

// RequestBackfill asks the leader to read every chosen channel again.
func (Connector) RequestBackfill(ctx context.Context, deps source.Deps, _ settings.Values) (time.Time, error) {
	now := time.Now().UTC()
	return now, deps.State.Set(ctx, rereadAllKey, now)
}

// GrantScopes is the chosen channels' scopes a reader holding scopes may
// read: a channel whose visibility is everyone who can use Diderot to a
// reader with internal, one whose visibility is a policy scope to a reader
// with that scope.
func (Connector) GrantScopes(ctx context.Context, deps source.Deps, scopes []string) ([]string, error) {
	var selections map[string]Selection
	if _, err := deps.State.Get(ctx, selectionKey, &selections); err != nil {
		return nil, err
	}
	var granted []string
	for _, id := range selectedIDs(selections) {
		if slices.Contains(scopes, selections[id].Scope) {
			granted = append(granted, ChannelScope(id))
		}
	}
	return granted, nil
}

var (
	// messageLink finds the guild and channel in a Discord link.
	messageLink = regexp.MustCompile(`discord(?:app)?\.com/channels/(\d+)/(\d+)`)
	snowflake   = regexp.MustCompile(`^\d{5,}$`)
)

// FindChannels finds the chosen channels entry names: a channel or thread
// link, a channel ID, or a channel name, with or without '#'. A thread's
// link or ID finds its channel.
func (Connector) FindChannels(ctx context.Context, deps source.Deps, entry string) ([]source.Channel, error) {
	var selections map[string]Selection
	if _, err := deps.State.Get(ctx, selectionKey, &selections); err != nil {
		return nil, err
	}
	var progress map[string]Progress
	if _, err := deps.State.Get(ctx, progressKey, &progress); err != nil {
		return nil, err
	}
	var directory Directory
	if _, err := deps.State.Get(ctx, directoryKey, &directory); err != nil {
		return nil, err
	}
	id := ""
	if m := messageLink.FindStringSubmatch(entry); m != nil {
		id = m[2]
	} else if snowflake.MatchString(entry) {
		id = entry
	}
	if id != "" {
		// A thread names its channel.
		for channelID, p := range progress {
			if _, ok := p.Threads[id]; ok {
				id = channelID
			}
		}
	}
	name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(entry), "#"))
	var found []source.Channel
	for _, channelID := range selectedIDs(selections) {
		s := selections[channelID]
		current := s.Name
		if _, ch, ok := directory.channel(channelID); ok {
			current = ch.Name
		}
		if channelID != id && strings.ToLower(current) != name {
			continue
		}
		found = append(found, source.Channel{ExternalIDPrefix: prefix(s.GuildID, channelID), Scope: ChannelScope(channelID)})
	}
	return found, nil
}
