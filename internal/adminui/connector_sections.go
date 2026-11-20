package adminui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/adminaction"
	notioncx "github.com/bluesky-social/diderot/internal/connector/notion"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// NotionRootsStore is the read side of the worker's managed-root store.
type NotionRootsStore interface {
	List(context.Context) ([]notioncx.Root, error)
	ListDiscoveries(context.Context) ([]notioncx.DiscoveryStatus, error)
	Add(context.Context, string, string, string) (string, error)
	Remove(context.Context, string) (bool, error)
}

// The sections the admin UI renders, by the name a source's SectionProvider
// gives it.
const (
	slackChannelsSection = "slack-channels"
	notionRootsSection   = "notion-roots"
)

// connectorSections are a source's own sections on its page, which its
// SectionProvider names. At most one is set, for the source the page shows.
type connectorSections struct {
	// Slack is the "slack-channels" section: how many channels the bot is
	// in, and the way to them.
	Slack *slackChannelCounts
	// Notion is the "notion-roots" section: the pages Diderot indexes.
	Notion *notionSourceView
	// Discord is the "discord-channels" section: the channels Diderot
	// indexes, and the others the bot can see.
	Discord *discordSectionView
}

// sections load each section the admin UI renders. Slack's shows only
// while Slack is on; Notion's pages can be chosen before it is.
var sections = map[string]func(*Server, *http.Request, settings.View) (connectorSections, error){
	slackChannelsSection:   (*Server).loadSlackSection,
	notionRootsSection:     (*Server).loadNotionSection,
	discordChannelsSection: (*Server).loadDiscordSection,
}

// HasSection reports whether the admin UI renders the section named name.
func HasSection(name string) bool {
	_, ok := sections[name]
	return ok
}

// sectionOf is the section the named connector's page has, or "".
func (s *Server) sectionOf(name string) string {
	src, ok := s.registries.Sources.Named(name)
	if !ok {
		return ""
	}
	if provider, provides := src.(source.SectionProvider); provides {
		return provider.Section()
	}
	return ""
}

type notionRootView struct {
	Type, ID, Label string
	// Status is the last walk's: "complete", "failed", or "" before the
	// first.
	Status   string
	AddedBy  string
	AddedAt  time.Time
	LastWalk *time.Time
	Error    string
}

type notionDiscoveryView struct {
	Type, ID, Title, URL string
	LastSeen             time.Time
	Covered              bool
}

type notionSourceView struct {
	Roots []notionRootView
	// Discoveries are one page of the pages shared with the integration
	// that Search matches, those not indexed first. Shared counts all of
	// them, and Uncovered those no root covers.
	Discoveries []notionDiscoveryView
	Shared      int
	Uncovered   int
	// Search is the text the shared pages are narrowed to, and Searched
	// says the list was searched or paged, so the page shows it open.
	Search   string
	Searched bool
	Pages    pagination
	// Mutable reports that roots can be added and removed here.
	Mutable bool
}

// notionDiscoveryPageSize is how many pages shared with the integration
// one page of the list shows.
const notionDiscoveryPageSize = 25

// The query parameters of the shared pages list.
const (
	notionSharedSearch = "shared"
	notionSharedPage   = "shared_page"
)

func (s *Server) mountConnectorSectionRoutes(mux Mux) {
	mux.Handle("POST /admin/connectors/notion/roots", s.authedBrowser(http.HandlerFunc(s.handleNotionRootAdd)))
	mux.Handle("POST /admin/connectors/notion/roots/remove", s.authedBrowser(http.HandlerFunc(s.handleNotionRootRemove)))
	mux.Handle("POST /admin/connectors/{name}/backfill", s.authedBrowser(http.HandlerFunc(s.handleConnectorBackfill)))
	mux.Handle("POST /admin/connectors/discord/channels", s.authedBrowser(http.HandlerFunc(s.handleDiscordChannelChoose)))
	mux.Handle("POST /admin/connectors/discord/channels/remove", s.authedBrowser(http.HandlerFunc(s.handleDiscordChannelRemove)))
	mux.Handle("POST /admin/connectors/discord/channels/reread", s.authedBrowser(http.HandlerFunc(s.handleDiscordChannelReread)))
	mux.Handle("POST /admin/connectors/discord/ask/guild", s.authedBrowser(http.HandlerFunc(s.handleDiscordAskGuild)))
	mux.Handle("POST /admin/connectors/discord/ask/trusted", s.authedBrowser(http.HandlerFunc(s.handleDiscordTrustedGuild)))
	mux.Handle("POST /admin/connectors/discord/ask/channel", s.authedBrowser(http.HandlerFunc(s.handleDiscordAskChannel)))
	mux.Handle("POST /admin/connectors/discord/ask/dms", s.authedBrowser(http.HandlerFunc(s.handleDiscordAskDirectMessages)))
}

// loadConnectorSections reads the section of the connector's page, if it is
// a source that has one.
func (s *Server) loadConnectorSections(r *http.Request, view settings.View) (connectorSections, error) {
	name := s.sectionOf(view.Name)
	if name == "" {
		return connectorSections{}, nil
	}
	load, ok := sections[name]
	if !ok {
		return connectorSections{}, fmt.Errorf("the admin UI has no %q section", name)
	}
	return load(s, r, view)
}

func (s *Server) loadSlackSection(r *http.Request, view settings.View) (connectorSections, error) {
	if !view.Enabled {
		return connectorSections{}, nil
	}
	counts, err := s.slackChannelCounts(r.Context(), "")
	if err != nil {
		return connectorSections{}, err
	}
	return connectorSections{Slack: &counts}, nil
}

func (s *Server) loadNotionSection(r *http.Request, _ settings.View) (connectorSections, error) {
	view := &notionSourceView{Mutable: s.notionRoots != nil}
	if s.notionRoots == nil {
		return connectorSections{Notion: view}, nil
	}
	ctx := r.Context()
	roots, err := s.notionRoots.List(ctx)
	if err != nil {
		return connectorSections{}, fmt.Errorf("notion roots: %w", err)
	}
	for _, root := range roots {
		view.Roots = append(view.Roots, notionRootView{
			Type: string(root.ObjectType), ID: root.ObjectID, Label: root.Label, Status: root.LastStatus,
			AddedBy: root.AddedBy, AddedAt: root.AddedAt, LastWalk: root.LastWalkedAt,
			Error: compactText(root.LastError, 240),
		})
	}
	discoveries, err := s.notionRoots.ListDiscoveries(ctx)
	if err != nil {
		return connectorSections{}, fmt.Errorf("notion discoveries: %w", err)
	}
	query := r.URL.Query()
	view.Search = strings.TrimSpace(query.Get(notionSharedSearch))
	view.Shared = len(discoveries)
	needle := strings.ToLower(view.Search)
	var matching []notionDiscoveryView
	for _, item := range discoveries {
		if !item.Covered {
			view.Uncovered++
		}
		if needle != "" && !strings.Contains(strings.ToLower(item.Title), needle) &&
			!strings.Contains(strings.ToLower(item.ObjectID), needle) && !strings.Contains(strings.ToLower(item.URL), needle) {
			continue
		}
		matching = append(matching, notionDiscoveryView{
			Type: string(item.ObjectType), ID: item.ObjectID, Title: item.Title,
			URL: item.URL, LastSeen: item.LastSeen, Covered: item.Covered,
		})
	}
	// A page past the last shows the last.
	page, _ := strconv.Atoi(query.Get(notionSharedPage))
	page = min(max(page, 1), max(1, (len(matching)+notionDiscoveryPageSize-1)/notionDiscoveryPageSize))
	view.Searched = view.Search != "" || query.Has(notionSharedPage)
	first := (page - 1) * notionDiscoveryPageSize
	last := min(first+notionDiscoveryPageSize, len(matching))
	view.Discoveries = matching[first:last]
	link := func(page int) string {
		values := url.Values{notionSharedPage: {strconv.Itoa(page)}}
		if view.Search != "" {
			values.Set(notionSharedSearch, view.Search)
		}
		return "?" + values.Encode() + "#notion-shared"
	}
	view.Pages = pagination{BackLabel: "Previous", ForwardLabel: "Next"}
	switch {
	case len(matching) == 0:
	case len(matching) <= notionDiscoveryPageSize:
		view.Pages.Summary = plural(len(matching), "page")
	default:
		view.Pages.Summary = fmt.Sprintf("%d–%d of %d pages", first+1, last, len(matching))
	}
	if page > 1 {
		view.Pages.BackURL = link(page - 1)
	}
	if last < len(matching) {
		view.Pages.ForwardURL = link(page + 1)
	}
	return connectorSections{Notion: view}, nil
}

func (s *Server) handleNotionRootAdd(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	input, label := strings.TrimSpace(r.Form.Get("input")), strings.TrimSpace(r.Form.Get("label"))
	if input == "" {
		redirectWithResult(w, r, connectorsPath+"/"+notioncx.ConnectorName, "Paste the address of a Notion page or database to add it.", true)
		return
	}
	_, err := adminaction.AddNotionRoot(r.Context(), s.audit(r), s.notionRoots, input, label)
	s.finishSectionMutation(w, r, notioncx.ConnectorName, err, "Page added. Diderot reads it and the pages under it within a day, or now with Read now.")
}

func (s *Server) handleNotionRootRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	input := strings.TrimSpace(r.Form.Get("input"))
	if input == "" || r.Form.Get("confirm") != "remove" {
		s.redirectConnector(w, r, notioncx.ConnectorName, "Nothing was removed: tick the box to confirm.", true)
		return
	}
	_, err := adminaction.RemoveNotionRoot(r.Context(), s.audit(r), s.notionRoots, input)
	s.finishSectionMutation(w, r, notioncx.ConnectorName, err, "Page removed. It and the pages under it leave search the next time Diderot reads Notion.")
}

// handleConnectorBackfill requests a source's re-read with the form's
// arguments.
func (s *Server) handleConnectorBackfill(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	name := r.PathValue("name")
	src, ok := s.registries.Sources.Named(name)
	backfiller, backfills := src.(source.Backfiller)
	if !ok || !backfills {
		http.NotFound(w, r)
		return
	}
	if r.Form.Get("confirm") != "backfill" {
		s.redirectConnector(w, r, name, "Nothing was started: tick the box to confirm.", true)
		return
	}
	args := settings.Values{}
	for _, field := range backfiller.Backfill().Fields {
		args[field.Key] = strings.TrimSpace(r.Form.Get("field." + field.Key))
	}
	_, err := adminaction.RequestSourceBackfill(r.Context(), s.audit(r), backfiller, s.sourceDeps, name, args)
	s.finishSectionMutation(w, r, name, err, "Re-read requested. "+s.registries.Title(name)+" starts it within a few minutes.")
}

func (s *Server) finishSectionMutation(w http.ResponseWriter, r *http.Request, connector string, err error, success string) {
	if s.auditFailed(w, err) {
		return
	}
	if err != nil {
		s.logger.Warn("admin connector section mutation failed", "connector", connector, "err", err)
		s.redirectConnector(w, r, connector, "That didn't work: "+publicSourceError(err)+".", true)
		return
	}
	s.redirectConnector(w, r, connector, success, false)
}
