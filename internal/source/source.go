// Package source defines a source, one place Diderot indexes, and the
// registry of the sources a build knows.
package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/attachments/docs"
	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// Source is one place Diderot indexes.
type Source interface {
	settings.Definition

	// Info is fixed facts about the source.
	Info() Info

	// Run holds the source's workers for one leader term with these
	// settings, until ctx ends. The settings supervisor restarts it when
	// they change.
	Run(ctx context.Context, deps Deps, s settings.Resolved) error

	// Expand returns the fuller context of one of its events, as a reader
	// with scopes may see it. It returns ErrNotExpandable when there is
	// none, and events.ErrEventNotFound when the event is gone.
	Expand(ctx context.Context, deps Deps, eventID int64, scopes []string) (Expanded, error)
}

// StatusReporter is a source whose page shows how it is doing.
type StatusReporter interface {
	// Status reads it, on any replica, through deps.
	Status(ctx context.Context, deps Deps) (settings.Status, error)
}

// SectionProvider is a source whose page has a section of its own, which the
// admin UI renders for it: Slack's channel counts, Notion's managed roots.
type SectionProvider interface {
	// Section names the section.
	Section() string
}

// ScopeGranter is a source whose events carry scopes of its own, which it
// grants to readers who hold the scopes they are visible to: a Discord
// channel's scope to everyone with internal, when its visibility is
// everyone who can use Diderot.
type ScopeGranter interface {
	// GrantScopes returns the source's scopes that a reader holding scopes
	// may read, or none.
	GrantScopes(ctx context.Context, deps Deps, scopes []string) ([]string, error)
}

// ChannelFinder is a source whose events belong to named channels, which the
// list tool's channels filter can name.
type ChannelFinder interface {
	// FindChannels returns the channels entry may mean: a channel's name,
	// with or without '#', its ID, or a link to it or one of its messages.
	// It returns none when it knows no such channel.
	FindChannels(ctx context.Context, deps Deps, entry string) ([]Channel, error)
}

// Channel is one channel a ChannelFinder found.
type Channel struct {
	// ExternalIDPrefix starts the external ID of every event in the
	// channel, and of no other event.
	ExternalIDPrefix string
	// Scope is the scope of the channel's events: whoever may read it may
	// list the channel.
	Scope string
}

// Backfiller is a source that re-reads what it indexes on request, from its
// page's backfill form.
type Backfiller interface {
	// Backfill describes the request on the source's page and its
	// arguments.
	Backfill() Backfill
	// RequestBackfill records a backfill, for the source's Run to work
	// through, with args, which hold a trimmed string for each of its
	// fields, and returns when it was recorded.
	RequestBackfill(ctx context.Context, deps Deps, args settings.Values) (time.Time, error)
}

// Backfill describes a source's backfill on its page.
type Backfill struct {
	// Summary says what a backfill re-reads, in a sentence.
	Summary string
	// Confirm labels the box that confirms it, and Action its button.
	Confirm, Action string
	// Fields are its arguments: optional text fields, whose Help shows
	// under the box.
	Fields []settings.Field
}

// Info is fixed facts about a source.
type Info struct {
	// EventSource is the events.source value of its events. It never
	// changes once used.
	EventSource string
	// Title is what pages call it ("Session feed"); ResultNoun is the
	// progress line's name for one of its results ("Work session").
	Title, ResultNoun string
	// Summary says what it indexes in a few words, for the connector pages
	// ("Messages in the Slack channels the bot is in.").
	Summary string
	// ToolDescription is its sentence in the retrieve and list tools'
	// sources parameters and its line in the {{SOURCES}} prompt variable. It
	// is one line, and it is part of the model's tool surface.
	ToolDescription string
	// ActorRole and IdentityNamespace project its stored actors into the
	// identity contract (internal/identity): what an event's actor did, and
	// the namespace of the actor's source identity, given the actor's
	// stored canonical value. A source whose events have no actor leaves
	// both unset.
	ActorRole         string
	IdentityNamespace func(canonical string) string
	// Threads says its events form conversations: an event's ThreadID is
	// its conversation's, and a conversation's root event has its own
	// external ID as its ThreadID. The tools mark a result's thread, and
	// expand reads the conversation.
	Threads bool
}

// Deps is what every source is handed. Nothing in it is source-specific
// but State, which For sets.
//
// A source's base metrics (events written, last success, errors) count only
// what reaches them through Deps, so every Run keeps two rules:
//
//   - It writes events through Sink, which counts each event it inserts or
//     changes.
//   - It runs its work under a supervisor built with Metrics,
//     worker.NewSupervisor(deps.Logger, deps.Metrics), which counts each
//     tick of a worker.Runner as a success or a failure. Work that is not a
//     Runner's ticks, such as the passes of one long-running worker.Func,
//     calls Metrics.Succeeded or Metrics.Failed after each pass itself.
type Deps struct {
	Pool *pgxpool.Pool
	// Sink writes events; the one Run is handed counts what it writes in
	// the source's base metrics.
	Sink   *events.DBSink
	Logger *slog.Logger
	// Metrics registers the source's own metrics and counts its base ones.
	Metrics settings.Metrics
	// State is the source's own durable state.
	State State
	// Files lends the attachment workers the source's fetchers. Only Run
	// has it.
	Files Attachments
}

// For returns d as the source named name is handed it: with its own State.
func (d Deps) For(name string) Deps {
	d.State = NewState(d.Pool, name)
	return d
}

// Attachments is where a running source lends the attachment workers what
// they need to read its files.
type Attachments interface {
	// LendImages lends the caption worker fetcher for the source's images
	// until release is called; run is the lending Run's context, and a fetch
	// in progress ends when it does. While none is lent, the source's images
	// wait unclaimed.
	LendImages(run context.Context, fetcher files.SourceFetcher) (release func())
	// AddDocumentWorker adds to sup a worker, named name, that extracts the
	// text of the source's documents through fetcher, for the channels gate
	// opts in. It returns the cleanup to run once sup has stopped.
	AddDocumentWorker(sup *worker.Supervisor, name string, gate docs.GateChecker, fetcher files.Fetcher) (cleanup func())
}

// DocumentIngestCheck is the Test check of a source whose Index documents
// setting is on. The document worker keeps every attachment in file storage
// (DIDEROT_IMAGES_BUCKET) and does not start without it, so the setting
// would otherwise pass its Test and extract nothing. fileStorage says this
// deployment has it.
func DocumentIngestCheck(fileStorage bool) settings.Check {
	if !fileStorage {
		return settings.Check{Name: "Index documents",
			Detail: "Indexing documents needs file storage, and this deployment has none.",
			Remedy: "Set DIDEROT_IMAGES_BUCKET in the deployment, or turn Index documents off."}
	}
	return settings.Check{Name: "Index documents", Passed: true, Detail: "File storage is set up for attachments."}
}

// Expanded is the fuller context of one event.
type Expanded struct {
	Content string
	// Truncated says the source cut Content short.
	Truncated bool
	// Attachments are the files of the events Content covers. A source
	// whose Content is the one event leaves it nil, and the caller lists
	// that event's attachments itself.
	Attachments []events.Attachment
}

// ErrNotExpandable means a source has no fuller context for an event.
var ErrNotExpandable = errors.New("source does not support expand")

// Registry is the sources a build knows, in the order tools and pages list
// them. The zero value has none.
type Registry struct {
	sources []Source
}

var eventSourceName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// NewRegistry checks and registers sources. A source without a title,
// result noun, summary, or tool description, with only one of an actor role and an
// identity namespace, with a reused name or event source, or with a
// ToolDescription that is not one line panics: these are programming
// errors.
func NewRegistry(sources ...Source) Registry {
	names, eventSources := map[string]bool{}, map[string]bool{}
	for _, src := range sources {
		name, info := src.Name(), src.Info()
		if names[name] {
			panic(fmt.Sprintf("source: %q is registered twice", name))
		}
		names[name] = true
		if !eventSourceName.MatchString(info.EventSource) || eventSources[info.EventSource] {
			panic(fmt.Sprintf("source: %s: event source %q is invalid or registered twice", name, info.EventSource))
		}
		eventSources[info.EventSource] = true
		if info.Title == "" || info.ResultNoun == "" || info.Summary == "" || info.ToolDescription == "" {
			panic(fmt.Sprintf("source: %s: Info needs a title, result noun, summary, and tool description", name))
		}
		if (info.ActorRole == "") != (info.IdentityNamespace == nil) {
			panic(fmt.Sprintf("source: %s: Info needs both an actor role and an identity namespace, or neither", name))
		}
		if strings.ContainsAny(info.ToolDescription, "\r\n") || strings.TrimSpace(info.ToolDescription) != info.ToolDescription {
			panic(fmt.Sprintf("source: %s: the tool description must be one trimmed line", name))
		}
		if backfiller, ok := src.(Backfiller); ok {
			checkBackfill(name, backfiller.Backfill())
		}
	}
	return Registry{sources: sources}
}

// checkBackfill panics on a backfill without its page's words, or with an
// argument that is not one optional text field.
func checkBackfill(name string, backfill Backfill) {
	if backfill.Summary == "" || backfill.Confirm == "" || backfill.Action == "" {
		panic(fmt.Sprintf("source: %s: a backfill needs a summary, a confirmation, and an action", name))
	}
	keys := map[string]bool{}
	for _, field := range backfill.Fields {
		if !eventSourceName.MatchString(field.Key) || keys[field.Key] || field.Kind != settings.KindText || field.Required || field.Label == "" {
			panic(fmt.Sprintf("source: %s: backfill argument %q must be a labelled, optional text field with a unique lower-case key", name, field.Key))
		}
		// The backfill API answers with its own "requested_at" beside the
		// arguments.
		if field.Key == "requested_at" {
			panic(fmt.Sprintf("source: %s: backfill argument %q has the name of a field in the backfill API's response; give it another key", name, field.Key))
		}
		keys[field.Key] = true
	}
}

// Backfillers returns every registered source that backfills.
func (r Registry) Backfillers() []Source {
	var backfillers []Source
	for _, src := range r.sources {
		if _, ok := src.(Backfiller); ok {
			backfillers = append(backfillers, src)
		}
	}
	return backfillers
}

// Sources returns every registered source.
func (r Registry) Sources() []Source { return r.sources }

// Names returns every registered source's connector name.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r.sources))
	for _, src := range r.sources {
		names = append(names, src.Name())
	}
	return names
}

// Infos returns every registered source's facts.
func (r Registry) Infos() Infos {
	infos := make(Infos, 0, len(r.sources))
	for _, src := range r.sources {
		infos = append(infos, src.Info())
	}
	return infos
}

// Named returns the source named name.
func (r Registry) Named(name string) (Source, bool) {
	for _, src := range r.sources {
		if src.Name() == name {
			return src, true
		}
	}
	return nil, false
}

// Lookup returns the source whose events have eventSource.
func (r Registry) Lookup(eventSource string) (Source, bool) {
	for _, src := range r.sources {
		if src.Info().EventSource == eventSource {
			return src, true
		}
	}
	return nil, false
}

// Infos is facts about sources, in registration order.
type Infos []Info

// Of returns the facts of the source whose events have eventSource. For an
// event source no registered source has, it returns an Info with only
// EventSource set.
func (infos Infos) Of(eventSource string) Info {
	for _, info := range infos {
		if info.EventSource == eventSource {
			return info
		}
	}
	return Info{EventSource: eventSource}
}

// Threaded returns the event source of each source whose events form
// conversations (Info.Threads).
func (infos Infos) Threaded() []string {
	var names []string
	for _, info := range infos {
		if info.Threads {
			names = append(names, info.EventSource)
		}
	}
	return names
}

// EventSources returns each source's event source.
func (infos Infos) EventSources() []string {
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.EventSource)
	}
	return names
}

// PromptList is the value of the {{SOURCES}} prompt variable: one Markdown
// list item per source, naming its sources filter value and saying what it
// holds.
func (infos Infos) PromptList() string {
	lines := make([]string, 0, len(infos))
	for _, info := range infos {
		lines = append(lines, "- `"+info.EventSource+"` — "+info.ToolDescription)
	}
	return strings.Join(lines, "\n")
}
