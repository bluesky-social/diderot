package adminui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bluesky-social/diderot/internal/adminaction"
	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/adminjob"
	notioncx "github.com/bluesky-social/diderot/internal/connector/notion"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
)

// The Slack channel pages.
const (
	slackConnectorPath = connectorsPath + "/" + slackcx.ConnectorName
	slackChannelsPath  = slackConnectorPath + "/channels"
)

// slackChannelPageSize is how many channels one page of the list shows.
const slackChannelPageSize = 50

var (
	slackChannelsPage = mustPage("slack_channels.html")
	slackSourcePage   = mustPage("source_slack.html")
)

// SlackSourceControl is what the Slack channel page acts through
// (*adminapi.Server): the Slack actions, and what they need.
type SlackSourceControl interface {
	adminaction.Slack
	adminaction.SlackDocuments
	// ResolveSlackChannel finds a channel by name or ID, as Slack has it.
	ResolveSlackChannel(ctx context.Context, nameOrID string) (channelID, name string, err error)
	// SlackOn reports whether the Slack connector is on.
	SlackOn() bool
	// SlackUserTokenSet reports whether Slack has the user token that
	// inviting and removing the bot act as.
	SlackUserTokenSet() bool
}

// slackTabs are the tabs of the Slack connector's pages: its settings, and
// its channels.
func slackTabs(channels bool) *tabSet {
	return &tabSet{Label: "Slack", Tabs: []navTab{
		{Label: "Settings", Href: slackConnectorPath, Current: !channels},
		{Label: "Channels", Href: slackChannelsPath, Current: channels},
	}}
}

// slackChannelFilter is one view of the channel list: its key in the URL's
// filter, its tab label, and the rows it keeps.
type slackChannelFilter struct {
	Key, Label string
	where      string
}

// slackChannelFilters are the channel list's views, in tab order. Every
// where clause reads the columns of slackChannelsCTE.
var slackChannelFilters = []slackChannelFilter{
	{Key: "", Label: "All", where: "true"},
	{Key: "ask", Label: "Ask on", where: "ask"},
	{Key: "documents", Label: "Documents on", where: "documents"},
	{Key: "not-member", Label: "Bot not in channel", where: "NOT member AND NOT blocked AND NOT archived"},
	{Key: "errors", Label: "Can't be read", where: "error <> ''"},
	{Key: "external", Label: "Shared outside", where: "external"},
	{Key: "blocked", Label: "Blocked", where: "blocked"},
	{Key: "archived", Label: "Archived", where: "archived"},
}

func slackFilter(key string) (slackChannelFilter, bool) {
	for _, filter := range slackChannelFilters {
		if filter.Key == key {
			return filter, true
		}
	}
	return slackChannelFilter{}, false
}

// slackChannelsCTE is every channel Diderot knows, from any of the Slack
// tables, with what the pages show of it. The bot is a member of a channel
// it reads. A channel it read once and no longer does is one the bot left
// or was removed from (or, for a few minutes, one just unblocked or
// rejoined: the Slack worker turns it back on by itself). Only a channel
// the bot reads can fail to be read. $1 is a search of names and IDs, or ”.
const slackChannelsCTE = `
	WITH ids AS (
	  SELECT channel_id FROM slack_channel_info
	  UNION SELECT channel_id FROM slack_channel_sync
	  UNION SELECT channel_id FROM slack_channel_config
	  UNION SELECT channel_id FROM slack_ingest_blacklist
	), channels AS (
	  SELECT ids.channel_id, COALESCE(i.name, '') AS name,
	         COALESCE(i.is_private, false) AS private, COALESCE(i.is_archived, false) AS archived,
	         COALESCE(c.has_external_members, false) AS external,
	         b.channel_id IS NOT NULL AS blocked,
	         COALESCE(ss.ingest_enabled, false) AND b.channel_id IS NULL AS member,
	         ss.channel_id IS NOT NULL AND NOT ss.ingest_enabled AND b.channel_id IS NULL AS bot_left,
	         COALESCE(c.ask_enabled, false) AS ask, COALESCE(c.doc_ingest_enabled, false) AS documents,
	         ss.last_synced_at, ss.in_progress_at IS NOT NULL AS reading,
	         CASE WHEN COALESCE(ss.ingest_enabled, false) AND b.channel_id IS NULL
	              THEN COALESCE(ss.last_error, '') ELSE '' END AS error
	    FROM ids
	    LEFT JOIN slack_channel_info i USING (channel_id)
	    LEFT JOIN slack_channel_sync ss USING (channel_id)
	    LEFT JOIN slack_channel_config c USING (channel_id)
	    LEFT JOIN slack_ingest_blacklist b USING (channel_id)
	   WHERE $1 = '' OR strpos(lower(COALESCE(i.name, '')), lower($1)) > 0
	                 OR strpos(lower(ids.channel_id), lower($1)) > 0
	)`

// slackChannelCounts counts the channels in each of the list's views, and
// those the bot reads.
type slackChannelCounts struct {
	All, Member, Ask, Documents, NotMember, Errors, External, Blocked, Archived int64
}

// view is the count of the view with key.
func (c *slackChannelCounts) view(key string) *int64 {
	switch key {
	case "ask":
		return &c.Ask
	case "documents":
		return &c.Documents
	case "not-member":
		return &c.NotMember
	case "errors":
		return &c.Errors
	case "external":
		return &c.External
	case "blocked":
		return &c.Blocked
	case "archived":
		return &c.Archived
	}
	return &c.All
}

// of is the count of the view with key.
func (c slackChannelCounts) of(key string) int64 { return *c.view(key) }

// slackChannelCounts counts the channels search matches.
func (s *Server) slackChannelCounts(ctx context.Context, search string) (slackChannelCounts, error) {
	var counts slackChannelCounts
	selects := []string{"count(*) FILTER (WHERE member)"}
	targets := []any{&counts.Member}
	for _, filter := range slackChannelFilters {
		selects = append(selects, "count(*) FILTER (WHERE "+filter.where+")")
		targets = append(targets, counts.view(filter.Key))
	}
	err := s.pool.QueryRow(ctx, slackChannelsCTE+` SELECT `+strings.Join(selects, ", ")+` FROM channels`, search).Scan(targets...)
	if err != nil {
		return counts, fmt.Errorf("count Slack channels: %w", err)
	}
	return counts, nil
}

// slackChannel is one channel as the list shows it.
type slackChannel struct {
	ID, Name          string
	Private, Archived bool
	External          bool
	// Left is a channel Diderot read until the bot left it or was removed.
	Member, Left, Blocked   bool
	AskEnabled, DocsEnabled bool
	LastSync                *time.Time
	Reading                 bool
	LastError               string
	Messages                int64
}

// channelState is a channel's state as the pages show it.
type channelState struct {
	Tone, Word string
	// Text says it in a sentence.
	Text string
}

// State is how Diderot stands with the channel: blocked, archived, unable
// to read it, not in it, or reading it.
func (c slackChannel) State() channelState {
	switch {
	case c.Blocked:
		return channelState{toneNeutral, "blocked", "Blocked: Diderot reads nothing new in it."}
	case c.Archived:
		return channelState{toneNeutral, "archived", "Archived in Slack."}
	case c.LastError != "":
		return channelState{toneBad, "error", "Diderot can't read it."}
	case c.Left:
		return channelState{toneNeutral, "not in channel", "The bot left it or was removed, so Diderot reads nothing new in it."}
	case !c.Member:
		return channelState{toneNeutral, "not in channel", "The bot is not in it, so nothing in it is indexed."}
	}
	return channelState{toneGood, "ok", "The bot is in it, and Diderot reads it."}
}

// Label is the channel's name as Slack writes it, or its ID while its name
// is unknown.
func (c slackChannel) Label() string {
	if c.Name == "" {
		return c.ID
	}
	return "#" + c.Name
}

type slackChannelsData struct {
	pageData
	// Lead is what the Slack connector does, as its own page says it.
	Lead     string
	Tabs     *tabSet
	Search   string
	Filter   string
	Filters  tabSet
	Channels []slackChannel
	// Total is how many channels the view holds; Page which page of them
	// this is, from 1.
	Total int64
	Page  int
	// Pages is the row under the table.
	Pages     pagination
	SlackOn   bool
	Connected bool
	// UserTokenSet says Slack has the user token that adding the bot to a
	// channel acts as.
	UserTokenSet bool
	CSRFToken    string
	Notice       string
	Error        string
}

func (s *Server) handleSlackChannels(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	data := slackChannelsData{
		pageData: s.page(r, "Slack channels", navConnectors), Tabs: slackTabs(true),
		Lead:   s.connectorSummaryText(slackcx.ConnectorName),
		Search: strings.TrimSpace(query.Get("q")), Filter: strings.TrimSpace(query.Get("filter")), Page: 1,
		Filters: tabSet{Label: "Show channels"},
		Pages:   pagination{BackLabel: "Previous", ForwardLabel: "Next"},
	}
	data.Stylesheet = "connectors.css"
	filter, ok := slackFilter(data.Filter)
	if !ok {
		filter, data.Filter = slackChannelFilters[0], ""
	}
	if page, err := strconv.Atoi(query.Get("page")); err == nil && page > 1 {
		data.Page = page
	}
	data.Notice, data.Error = cleanNotice(query.Get("notice")), cleanNotice(query.Get("error"))
	if s.sourceControl != nil {
		data.SlackOn = s.sourceControl.SlackOn()
		data.UserTokenSet = s.sourceControl.SlackUserTokenSet()
	}
	ctx := r.Context()
	counts, err := s.slackChannelCounts(ctx, data.Search)
	if err != nil {
		s.sourcePageError(w, "Slack channels", err)
		return
	}
	link := func(filter string, page int) string {
		values := url.Values{}
		if data.Search != "" {
			values.Set("q", data.Search)
		}
		if filter != "" {
			values.Set("filter", filter)
		}
		if page > 1 {
			values.Set("page", strconv.Itoa(page))
		}
		if len(values) == 0 {
			return slackChannelsPath
		}
		return slackChannelsPath + "?" + values.Encode()
	}
	for _, view := range slackChannelFilters {
		count := counts.of(view.Key)
		if view.Key != "" && count == 0 && view.Key != data.Filter {
			continue
		}
		data.Filters.Tabs = append(data.Filters.Tabs, navTab{
			Label: view.Label, Href: link(view.Key, 1), Count: count, Counted: true, Current: view.Key == data.Filter,
		})
	}
	data.Total = counts.of(data.Filter)
	// A page past the last shows the last.
	data.Page = min(data.Page, max(1, int((data.Total+slackChannelPageSize-1)/slackChannelPageSize)))
	if data.Channels, err = s.loadSlackChannels(ctx, data.Search, filter, data.Page); err != nil {
		s.sourcePageError(w, "Slack channels", err)
		return
	}
	first := int64(data.Page-1)*slackChannelPageSize + 1
	switch {
	case len(data.Channels) == 0:
	case data.Total == 1:
		data.Pages.Summary = "1 channel"
	case data.Total <= slackChannelPageSize:
		data.Pages.Summary = fmt.Sprintf("%d channels", data.Total)
	default:
		data.Pages.Summary = fmt.Sprintf("%d–%d of %d channels", first, first+int64(len(data.Channels))-1, data.Total)
	}
	if data.Page > 1 {
		data.Pages.BackURL = link(data.Filter, data.Page-1)
	}
	if first+int64(len(data.Channels))-1 < data.Total {
		data.Pages.ForwardURL = link(data.Filter, data.Page+1)
	}
	// A search that matches nothing still shows the search, not the empty
	// install's page.
	data.Connected = counts.All > 0 || data.Search != ""
	if data.CSRFToken, err = issueCSRFToken(w, r); err != nil {
		http.Error(w, "CSRF token failed", http.StatusInternalServerError)
		return
	}
	s.render(w, slackChannelsPage, data)
}

// handleSlackChannelInvite adds the bot to a channel named by its name, ID
// or link, which need not be one Diderot knows yet.
func (s *Server) handleSlackChannelInvite(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	if s.sourceControl == nil {
		http.Error(w, "slack source controls unavailable", http.StatusServiceUnavailable)
		return
	}
	refuse := func(why string) { redirectWithResult(w, r, slackChannelsPath, why, true) }
	key := slackChannelKey(r.Form.Get("channel"))
	if key == "" {
		refuse("The bot was not added: enter the channel's name, ID or link.")
		return
	}
	id, name, err := s.sourceControl.ResolveSlackChannel(r.Context(), key)
	switch {
	case errors.Is(err, adminapi.ErrSlackOff):
		refuse("The bot was not added: Slack is off.")
		return
	case errors.Is(err, adminapi.ErrNoSlackUserToken):
		refuse("The bot was not added: adding it from here needs a Slack user token. Add one in Slack settings, or invite the bot from Slack.")
		return
	case errors.Is(err, slackcx.ErrChannelNotFound):
		refuse("The bot was not added: Slack has no channel " + cleanNotice(key) + " that the user token can see.")
		return
	case err != nil:
		s.logger.Warn("admin Slack channel lookup failed", "channel", key, "err", err)
		refuse("The bot was not added: Slack could not look the channel up. Try again in a minute.")
		return
	}
	label := id
	if name != "" {
		label = "#" + name
	}
	already, err := adminaction.SetSlackMembership(r.Context(), s.audit(r), s.sourceControl, id, true)
	if s.auditFailed(w, err) {
		return
	}
	switch {
	case err != nil:
		s.logger.Warn("admin Slack channel invite failed", "channel_id", id, "err", err)
		refuse("The bot was not added to " + label + ": " + publicSourceError(err) + ".")
	case already:
		redirectWithResult(w, r, slackChannelsPath, "The bot was already in "+label+".", false)
	default:
		redirectWithResult(w, r, slackChannelsPath, "The bot is in "+label+" now. Diderot starts reading it within a few minutes, and lists it here then.", false)
	}
}

// slackChannelKey is the channel a form names, as Slack looks it up: the
// ID out of a link, or the name without its #.
func slackChannelKey(input string) string {
	key := strings.TrimSpace(input)
	if _, rest, ok := strings.Cut(key, "/archives/"); ok {
		key, _, _ = strings.Cut(rest, "/")
		key, _, _ = strings.Cut(key, "?")
	}
	return strings.TrimPrefix(key, "#")
}

// loadSlackChannels reads one page of the channels search and filter keep,
// by name.
func (s *Server) loadSlackChannels(ctx context.Context, search string, filter slackChannelFilter, page int) ([]slackChannel, error) {
	rows, err := s.pool.Query(ctx, slackChannelsCTE+`, page AS (
		  SELECT * FROM channels WHERE `+filter.where+`
		   ORDER BY name = '', lower(name), channel_id
		   LIMIT $2 OFFSET $3
		)
		SELECT page.channel_id, page.name, page.private, page.archived, page.external,
		       page.member, page.bot_left, page.blocked, page.ask, page.documents,
		       page.last_synced_at, page.reading, page.error,
		       (SELECT count(*) FROM slack_message_details d JOIN events e ON e.id = d.event_id
		         WHERE d.channel_id = page.channel_id AND NOT e.deleted)
		  FROM page
		 ORDER BY page.name = '', lower(page.name), page.channel_id`,
		search, slackChannelPageSize, (page-1)*slackChannelPageSize)
	if err != nil {
		return nil, fmt.Errorf("list Slack channels: %w", err)
	}
	defer rows.Close()
	var channels []slackChannel
	for rows.Next() {
		var c slackChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Private, &c.Archived, &c.External,
			&c.Member, &c.Left, &c.Blocked, &c.AskEnabled, &c.DocsEnabled,
			&c.LastSync, &c.Reading, &c.LastError, &c.Messages); err != nil {
			return nil, fmt.Errorf("scan Slack channel: %w", err)
		}
		c.LastError = compactText(c.LastError, 240)
		channels = append(channels, c)
	}
	return channels, rows.Err()
}

// slackChannelDetail is one channel as its page shows it.
type slackChannelDetail struct {
	slackChannel
	AllowExternal bool
	// ExternalMembers are the people from other organizations in it.
	ExternalMembers []slackPerson
	UpdatedBy       string
	UpdatedAt       *time.Time
	BlockReason     string
	BlockedBy       string
	BlockedAt       *time.Time
	Documents       int64
	Images          int64
	AskSessions     int64
}

// slackPerson is a Slack user, by name where the directory has one.
type slackPerson struct {
	ID, Name string
}

type slackSourceData struct {
	pageData
	Channel   slackChannelDetail
	CSRFToken string
	Notice    string
	Error     string
	// SlackOn enables every control but the image rebuild, which works on
	// stored messages; UserTokenSet also gates inviting and removing the
	// bot.
	SlackOn      bool
	UserTokenSet bool
	// DocumentsOff reports that the Slack connector reads no documents in
	// any channel, so this channel's setting has no effect.
	DocumentsOff bool
	// SlackURL opens the channel in Slack, when the workspace is known.
	SlackURL string
	// ImageReplay is this channel's latest image rebuild, nil when it has
	// had none; ImageReplayElsewhere, that another channel's is running.
	ImageReplay          *slackImageReplay
	ImageReplayElsewhere bool
}

// channelForm is a one-button form that posts one value to an action of
// the channel's.
type channelForm struct {
	ID, CSRFToken, Path, Field, Value, Label string
}

// Action is the form that posts value as field to the channel's action at
// path, labelled label.
func (d slackSourceData) Action(path, field, value, label string) channelForm {
	return channelForm{ID: d.Channel.ID, CSRFToken: d.CSRFToken, Path: path, Field: field, Value: value, Label: label}
}

// Switch is the form that turns one of the channel's settings the other
// way: Ask, documents, or, once allowed, asking from outside the
// organization.
func (d slackSourceData) Switch(field string) channelForm {
	on := map[string]bool{
		adminaction.SlackConfigAsk: d.Channel.AskEnabled, adminaction.SlackConfigDocuments: d.Channel.DocsEnabled,
		adminaction.SlackConfigExternal: d.Channel.AllowExternal,
	}[field]
	label := "Turn on"
	switch {
	case on && field == adminaction.SlackConfigExternal:
		label = "Stop allowing"
	case on:
		label = "Turn off"
	}
	return channelForm{ID: d.Channel.ID, CSRFToken: d.CSRFToken, Path: "config", Field: field,
		Value: strconv.FormatBool(!on), Label: label}
}

func (s *Server) handleSlackSource(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !validSlackChannelID(id) {
		s.renderNotFound(w, r, missingChannel)
		return
	}
	ctx := r.Context()
	detail, err := s.loadSlackSource(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		s.renderNotFound(w, r, missingChannel)
		return
	}
	if err != nil {
		s.sourcePageError(w, "Slack channel", err)
		return
	}
	data := slackSourceData{pageData: s.page(r, detail.Label(), navConnectors), Channel: detail}
	data.Stylesheet = "connectors.css"
	data.Script = "source_slack.js"
	data.Notice, data.Error = cleanNotice(r.URL.Query().Get("notice")), cleanNotice(r.URL.Query().Get("error"))
	if s.sourceControl != nil {
		data.SlackOn = s.sourceControl.SlackOn()
		data.UserTokenSet = s.sourceControl.SlackUserTokenSet()
	}
	if s.connectors != nil {
		if view, err := s.connectors.View(ctx, slackcx.ConnectorName); err == nil && view.Enabled {
			on, _ := view.Settings[slackcx.FieldDocIngest].(bool)
			data.DocumentsOff = !on
		}
	}
	if host := s.slackWorkspaceHost(); host != "" {
		data.SlackURL = "https://" + host + "/archives/" + id
	}
	data.ImageReplay, err = s.loadSlackImageReplay(ctx, id)
	if err != nil {
		s.sourcePageError(w, "Slack image rebuild", err)
		return
	}
	activeOwner, active, err := s.loadActiveSlackImageReplay(ctx)
	if err != nil {
		s.sourcePageError(w, "Slack image rebuild", err)
		return
	}
	data.ImageReplayElsewhere = active && activeOwner != id
	csrfToken, err := issueCSRFToken(w, r)
	if err != nil {
		s.sourcePageError(w, "CSRF token", err)
		return
	}
	data.CSRFToken = csrfToken
	s.render(w, slackSourcePage, data)
}

// slackConfigNotices say what turning each of a channel's switches on or
// off did.
var slackConfigNotices = map[string][2]string{
	adminaction.SlackConfigAsk: {
		"Ask is off in this channel.",
		"Ask is on in this channel: Diderot answers when someone mentions it here.",
	},
	adminaction.SlackConfigDocuments: {
		"Documents posted in this channel are no longer indexed. Those already indexed stay searchable.",
		"Documents posted in this channel are indexed now, starting with those already posted.",
	},
	adminaction.SlackConfigExternal: {
		"Only people in your organization may ask in this channel.",
		"People outside your organization may now ask in this channel.",
	},
}

func (s *Server) handleSlackSourceConfig(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sourceMutationID(w, r)
	if !ok {
		return
	}
	field := r.Form.Get("field")
	notices, known := slackConfigNotices[field]
	if !known {
		http.Error(w, "unknown configuration field", http.StatusBadRequest)
		return
	}
	enabled, err := strconv.ParseBool(r.Form.Get("enabled"))
	if err != nil {
		http.Error(w, "invalid enabled state", http.StatusBadRequest)
		return
	}
	if field == adminaction.SlackConfigExternal && enabled && r.Form.Get("confirm") != "external" {
		s.finishSourceRedirect(w, r, id, "Nothing was changed: tick the box to confirm.", true)
		return
	}
	err = adminaction.SetSlackChannelConfig(r.Context(), s.audit(r), s.sourceControl, id, field, enabled)
	notice := notices[0]
	if enabled {
		notice = notices[1]
	}
	s.finishSourceMutation(w, r, err, id, notice)
}

func (s *Server) handleSlackSourceBlacklist(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sourceMutationID(w, r)
	if !ok {
		return
	}
	block, err := strconv.ParseBool(r.Form.Get("enabled"))
	if err != nil {
		http.Error(w, "invalid blocked state", http.StatusBadRequest)
		return
	}
	if !block {
		err = adminaction.UnblacklistSlackChannel(r.Context(), s.audit(r), s.sourceControl, id)
		s.finishSourceMutation(w, r, err, id, "This channel is unblocked. If the bot is in it, Diderot starts reading it again within a few minutes.")
		return
	}
	reason := strings.TrimSpace(r.Form.Get("reason"))
	if reason == "" {
		s.finishSourceRedirect(w, r, id, "Say why you are blocking this channel.", true)
		return
	}
	_, err = adminaction.BlacklistSlackChannel(r.Context(), s.audit(r), s.sourceControl, id, "", reason)
	s.finishSourceMutation(w, r, err, id, "This channel is blocked. Diderot reads nothing new in it.")
}

func (s *Server) handleSlackSourceMembership(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sourceMutationID(w, r)
	if !ok {
		return
	}
	joined, err := strconv.ParseBool(r.Form.Get("joined"))
	if err != nil {
		http.Error(w, "invalid membership state", http.StatusBadRequest)
		return
	}
	already, err := adminaction.SetSlackMembership(r.Context(), s.audit(r), s.sourceControl, id, joined)
	var notice string
	switch {
	case joined && already:
		notice = "The bot was already in this channel."
	case joined:
		notice = "The bot is in this channel now. Diderot starts reading it within a few minutes."
	case already:
		notice = "The bot was not in this channel."
	default:
		notice = "The bot left this channel. Diderot stops reading it; what it indexed stays searchable."
	}
	s.finishSourceMutation(w, r, err, id, notice)
}

func (s *Server) handleSlackSourceBackfill(w http.ResponseWriter, r *http.Request) {
	id, ok := s.sourceMutationID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	switch kind := strings.TrimSpace(r.Form.Get("kind")); kind {
	case "", "messages":
		_, err := adminaction.RequestSlackBackfill(ctx, s.audit(r), s.sourceControl, id)
		s.finishSourceMutation(w, r, err, id, "Reading this channel's history again. Messages appear in search as Diderot reads them.")
	case "documents":
		err := adminaction.RequestSlackDocumentBackfill(ctx, s.audit(r), s.sourceControl, id)
		s.finishSourceMutation(w, r, err, id, "Reading this channel's documents again.")
	case "images":
		if r.Form.Get("confirm") != "images" {
			s.finishSourceRedirect(w, r, id, "Nothing was started: tick the box to confirm.", true)
			return
		}
		_, err := adminaction.StartSlackImageReplay(ctx, s.audit(r), s.sourceControl, id)
		if errors.Is(err, adminjob.ErrActive) {
			s.finishSourceRedirect(w, r, id, "Image descriptions are already being rebuilt. Try again when that finishes.", true)
			return
		}
		s.finishSourceMutation(w, r, err, id, "Rebuilding this channel's image descriptions.")
	default:
		http.Error(w, "unknown backfill kind", http.StatusBadRequest)
	}
}

func (s *Server) sourceMutationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !s.requireMutation(w, r) {
		return "", false
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !validSlackChannelID(id) {
		http.Error(w, "invalid Slack channel id", http.StatusBadRequest)
		return "", false
	}
	return id, true
}

func (s *Server) finishSourceMutation(w http.ResponseWriter, r *http.Request, err error, id, success string) {
	if s.auditFailed(w, err) {
		return
	}
	if err != nil {
		s.logger.Warn("admin Slack source mutation failed", "channel_id", id, "err", err)
		s.finishSourceRedirect(w, r, id, "That didn't work: "+publicSourceError(err)+".", true)
		return
	}
	s.finishSourceRedirect(w, r, id, success, false)
}

func (s *Server) finishSourceRedirect(w http.ResponseWriter, r *http.Request, id, message string, refused bool) {
	redirectWithResult(w, r, slackChannelsPath+"/"+id, message, refused)
}

// publicSourceError is what the page says about a failed action: the
// reasons an operator can act on, and otherwise nothing of the error.
func publicSourceError(err error) string {
	if err == nil {
		return "unknown error"
	}
	var invalid *notioncx.ValidationError
	if message, ok := discordChoiceError(err); ok {
		return message
	}
	switch {
	case errors.Is(err, adminaction.ErrNotBlacklisted):
		return "this channel is not blocked"
	case errors.Is(err, adminaction.ErrBotNotInChannel):
		return "the bot is not in this channel. Invite it first"
	case errors.Is(err, slackcx.ErrChannelNotFound):
		return "Slack can't find this channel"
	case errors.As(err, &invalid):
		return "that is not a Notion page or database address (" + cleanNotice(invalid.Message) + ")"
	}
	text := cleanNotice(err.Error())
	switch text {
	case "document ingest is disabled for this channel; enable it before requesting a replay":
		return "turn on Index documents for this channel first"
	case "slack source controls unavailable", "slack membership control unavailable",
		"slack backfill control unavailable", "notion root store unavailable",
		adminapi.ErrSlackOff.Error(), adminapi.ErrNoSlackUserToken.Error(),
		"slack channel not found", "slack image replay is unavailable":
		return text
	}
	return "something went wrong; the audit log has the details"
}

func (s *Server) loadSlackSource(ctx context.Context, id string) (slackChannelDetail, error) {
	var out slackChannelDetail
	var externalMembers []string
	err := s.pool.QueryRow(ctx, slackChannelsCTE+`
		SELECT ch.channel_id, ch.name, ch.private, ch.archived, ch.external,
		       ch.member, ch.bot_left, ch.blocked, ch.ask, ch.documents,
		       ch.last_synced_at, ch.reading, ch.error,
		       COALESCE(c.allow_external_ask, false), COALESCE(c.external_members, '{}'),
		       COALESCE(c.updated_by, ''), c.updated_at,
		       COALESCE(b.reason, ''), COALESCE(b.added_by, ''), b.added_at,
		       (SELECT count(*) FROM slack_message_details d JOIN events e ON e.id = d.event_id
		         WHERE d.channel_id = ch.channel_id AND NOT e.deleted),
		       (SELECT count(*) FROM slack_message_details d JOIN event_documents ed ON ed.event_id = d.event_id
		         WHERE d.channel_id = ch.channel_id),
		       (SELECT count(*) FROM slack_message_details d JOIN event_images ei ON ei.event_id = d.event_id
		         WHERE d.channel_id = ch.channel_id),
		       (SELECT count(DISTINCT session_id) FROM ask_slack_sessions a WHERE a.channel_id = ch.channel_id)
		  FROM channels ch
		  LEFT JOIN slack_channel_config c USING (channel_id)
		  LEFT JOIN slack_ingest_blacklist b USING (channel_id)
		 WHERE ch.channel_id = $2`, "", id).Scan(
		&out.ID, &out.Name, &out.Private, &out.Archived, &out.External,
		&out.Member, &out.Left, &out.Blocked, &out.AskEnabled, &out.DocsEnabled,
		&out.LastSync, &out.Reading, &out.LastError,
		&out.AllowExternal, &externalMembers, &out.UpdatedBy, &out.UpdatedAt,
		&out.BlockReason, &out.BlockedBy, &out.BlockedAt,
		&out.Messages, &out.Documents, &out.Images, &out.AskSessions)
	if err != nil {
		return out, err
	}
	out.LastError = compactText(out.LastError, 600)
	if len(externalMembers) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT member, COALESCE(NULLIF(u.real_name, ''), NULLIF(u.display_name, ''), NULLIF(u.name, ''), '')
		  FROM unnest($1::text[]) WITH ORDINALITY AS m(member, position)
		  LEFT JOIN slack_users u ON u.user_id = m.member
		 ORDER BY m.position`, externalMembers)
	if err != nil {
		return out, fmt.Errorf("external member names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var person slackPerson
		if err := rows.Scan(&person.ID, &person.Name); err != nil {
			return out, fmt.Errorf("external member names scan: %w", err)
		}
		out.ExternalMembers = append(out.ExternalMembers, person)
	}
	return out, rows.Err()
}

// slackImageReplay is one rebuild of a channel's image descriptions.
type slackImageReplay struct {
	ID          int64
	Tone, Word  string
	RequestedBy string
	RequestedAt time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	Progress    adminapi.SlackImageReplayProgress
	Error       string
}

// Running reports a rebuild that is waiting to start or under way.
func (j slackImageReplay) Running() bool { return j.Tone == toneAttention }

// Took is how long it has run, or ran.
func (j slackImageReplay) Took() time.Duration {
	if j.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if j.FinishedAt != nil {
		end = *j.FinishedAt
	}
	return end.Sub(*j.StartedAt)
}

// loadSlackImageReplay reads the channel's latest image rebuild, or nil.
func (s *Server) loadSlackImageReplay(ctx context.Context, channelID string) (*slackImageReplay, error) {
	var job slackImageReplay
	var status string
	var progress []byte
	var heartbeatAt time.Time
	var staleAfterSeconds int64
	err := s.pool.QueryRow(ctx, `
		SELECT id, status, requested_by, progress, requested_at, started_at, heartbeat_at,
		       stale_after_seconds, finished_at, COALESCE(error, '')
		  FROM admin_jobs
		 WHERE kind = 'slack_image_replay' AND request->>'channel_id' = $1
		 ORDER BY requested_at DESC, id DESC LIMIT 1`, channelID).Scan(
		&job.ID, &status, &job.RequestedBy, &progress, &job.RequestedAt, &job.StartedAt, &heartbeatAt,
		&staleAfterSeconds, &job.FinishedAt, &job.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest Slack image rebuild: %w", err)
	}
	_ = json.Unmarshal(progress, &job.Progress)
	job.Word, job.Tone = jobState(status, time.Since(heartbeatAt) > time.Duration(staleAfterSeconds)*time.Second)
	job.Error = compactText(job.Error, 600)
	return &job, nil
}

func (s *Server) loadActiveSlackImageReplay(ctx context.Context) (string, bool, error) {
	var owner string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(request->>'channel_id', '') FROM admin_jobs
		 WHERE kind='slack_image_replay' AND status IN ('queued', 'running')
		   AND heartbeat_at >= now() - make_interval(secs => stale_after_seconds)
		 ORDER BY requested_at DESC, id DESC LIMIT 1`).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("active Slack image rebuild: %w", err)
	}
	return owner, true, nil
}

func validSlackChannelID(value string) bool {
	if len(value) < 2 || (value[0] != 'C' && value[0] != 'G' && value[0] != 'D') {
		return false
	}
	for _, r := range value[1:] {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func (s *Server) sourcePageError(w http.ResponseWriter, operation string, err error) {
	s.logger.Error("admin sources failed", "operation", operation, "err", err)
	http.Error(w, operation+" failed", http.StatusInternalServerError)
}
