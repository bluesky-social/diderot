package adminui

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/adminaction"
	discordcx "github.com/bluesky-social/diderot/internal/connector/discord"
	"github.com/bluesky-social/diderot/internal/settings"
)

const discordChannelsSection = discordcx.SectionName

// discordSectionView is the "discord-channels" section: every channel the
// bot can see, by server, and the ones Diderot indexes.
type discordSectionView struct {
	// ReadAt is when the servers were last read; nil before the first.
	ReadAt *time.Time
	// Error says why the last read of the servers failed.
	Error  string
	Guilds []discordGuildView
	// Gone are chosen channels Discord no longer shows the bot, and
	// GoneGuilds servers it has left where Ask is still on or trusted.
	Gone       []discordChannelView
	GoneGuilds []discordGuildView
	Chosen     int
	// AskDirectMessages says Ask answers direct messages; InviteURL adds
	// the bot to a server with what it needs.
	AskDirectMessages bool
	InviteURL         string
	CSRF              string
}

type discordGuildView struct {
	ID, Name string
	// Ask says Ask answers in the server; Trusted that its members are the
	// organisation's own people.
	Ask, Trusted bool
	Channels     []discordChannelView
	CSRF         string
}

type discordChannelView struct {
	ID, Name, Kind, Category string
	// Private says @everyone can't view it in Discord; Readable that the
	// bot may read its history.
	Private, Readable bool
	Chosen, Removing  bool
	// Visibility and Policy are the chosen visibility ("everyone" or
	// "policy") and the policy scope's name.
	Visibility, Policy string
	Messages           int64
	LastRead           *time.Time
	// Reading says its whole history is waiting to be read, for the first
	// time or again.
	Reading bool
	Error   string
	AddedBy string
	// GuildAsk says Ask is on in its server, and Ask that it answers here.
	GuildAsk, Ask bool
	// CSRF is the page's token, for the row's forms.
	CSRF string
}

// setCSRF gives every row's forms the page's token.
func (v *discordSectionView) setCSRF(token string) {
	v.CSRF = token
	for i := range v.Guilds {
		v.Guilds[i].CSRF = token
		for j := range v.Guilds[i].Channels {
			v.Guilds[i].Channels[j].CSRF = token
		}
	}
	for i := range v.GoneGuilds {
		v.GoneGuilds[i].CSRF = token
	}
	for i := range v.Gone {
		v.Gone[i].CSRF = token
	}
}

func (s *Server) discordChannels() discordcx.Channels {
	return discordcx.ChannelsFor(s.sourceDeps.For(discordcx.ConnectorName))
}

func (s *Server) loadDiscordSection(r *http.Request, _ settings.View) (connectorSections, error) {
	o, err := s.discordChannels().Overview(r.Context())
	if err != nil {
		return connectorSections{}, err
	}
	view := &discordSectionView{Error: o.Directory.Error, AskDirectMessages: o.Ask.DirectMessages, InviteURL: o.Directory.InviteURL()}
	if !o.Directory.ReadAt.IsZero() {
		view.ReadAt = &o.Directory.ReadAt
	}
	seen := map[string]bool{}
	describe := func(c discordChannelView) discordChannelView {
		selected, chosen := o.Selections[c.ID]
		if !chosen {
			return c
		}
		seen[c.ID] = true
		c.Chosen, c.Removing, c.AddedBy = true, selected.Removing, selected.AddedBy
		c.Visibility, c.Policy = discordcx.VisibilityEveryone, ""
		if name, ok := strings.CutPrefix(selected.Scope, "corpus:"); ok {
			c.Visibility, c.Policy = discordcx.VisibilityPolicy, name
		}
		p := o.Progress[c.ID]
		c.Messages, c.LastRead, c.Error = p.Messages, p.RecentReadAt, p.Problem()
		c.Reading = p.FullReadAt == nil || (selected.RereadAt != nil && selected.RereadAt.After(*p.FullReadAt))
		if !selected.Removing {
			view.Chosen++
		}
		return c
	}
	for _, g := range o.Directory.Guilds {
		guild := discordGuildView{ID: g.ID, Name: g.Name, Ask: o.Ask.Guilds[g.ID], Trusted: o.Ask.Trusted[g.ID]}
		for _, c := range g.Channels {
			guild.Channels = append(guild.Channels, describe(discordChannelView{
				ID: c.ID, Name: c.Name, Kind: c.Kind(), Category: c.Category, Private: c.Private, Readable: c.Readable,
				GuildAsk: guild.Ask, Ask: guild.Ask && !o.Ask.ChannelsOff[c.ID],
			}))
		}
		view.Guilds = append(view.Guilds, guild)
	}
	listed := map[string]bool{}
	for _, g := range o.Directory.Guilds {
		listed[g.ID] = true
	}
	for id := range o.Ask.Guilds {
		if !listed[id] {
			listed[id] = true
			view.GoneGuilds = append(view.GoneGuilds, discordGuildView{ID: id, Name: id, Ask: true, Trusted: o.Ask.Trusted[id]})
		}
	}
	for id := range o.Ask.Trusted {
		if !listed[id] {
			view.GoneGuilds = append(view.GoneGuilds, discordGuildView{ID: id, Name: id, Trusted: true})
		}
	}
	slices.SortFunc(view.GoneGuilds, func(a, b discordGuildView) int { return strings.Compare(a.ID, b.ID) })
	for id, selected := range o.Selections {
		if !seen[id] {
			view.Gone = append(view.Gone, describe(discordChannelView{ID: id, Name: selected.Name}))
		}
	}
	return connectorSections{Discord: view}, nil
}

func (s *Server) handleDiscordChannelChoose(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	channelID := strings.TrimSpace(r.Form.Get("channel"))
	visibility := r.Form.Get("visibility")
	_, err := adminaction.ChooseDiscordChannel(r.Context(), s.audit(r), s.discordChannels(), channelID, visibility,
		r.Form.Get("policy"), r.Form.Get("confirm_private") == "yes")
	success := "Channel chosen. Diderot reads its history within a minute, then keeps it up to date."
	if r.Form.Get("change") == "yes" {
		success = "Visibility changed."
	}
	s.finishSectionMutation(w, r, discordcx.ConnectorName, err, success)
}

func (s *Server) handleDiscordChannelRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	if r.Form.Get("confirm") != "remove" {
		s.redirectConnector(w, r, discordcx.ConnectorName, "Nothing was removed: tick the box to confirm.", true)
		return
	}
	_, err := adminaction.RemoveDiscordChannel(r.Context(), s.audit(r), s.discordChannels(), strings.TrimSpace(r.Form.Get("channel")))
	s.finishSectionMutation(w, r, discordcx.ConnectorName, err, "Channel removed. Its messages leave search within a minute.")
}

func (s *Server) handleDiscordChannelReread(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	_, err := adminaction.RereadDiscordChannel(r.Context(), s.audit(r), s.discordChannels(), strings.TrimSpace(r.Form.Get("channel")))
	s.finishSectionMutation(w, r, discordcx.ConnectorName, err, "Re-read requested. Diderot reads the channel's history again within a minute.")
}

func (s *Server) handleDiscordAskGuild(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	on := r.Form.Get("on") == "yes"
	err := adminaction.SetDiscordAskInGuild(r.Context(), s.audit(r), s.discordChannels(), strings.TrimSpace(r.Form.Get("guild")), on)
	success := "Ask is off in the server."
	if on {
		success = "Ask is on in the server: it answers mentions, /ask and the summary commands in every channel you leave on."
	}
	s.finishSectionMutation(w, r, discordcx.ConnectorName, err, success)
}

func (s *Server) handleDiscordTrustedGuild(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	trusted := r.Form.Get("trusted") == "yes"
	err := adminaction.SetDiscordTrustedGuild(r.Context(), s.audit(r), s.discordChannels(), strings.TrimSpace(r.Form.Get("guild")), trusted)
	success := "The server is no longer trusted: Ask there answers only from what its members can read in Discord."
	if trusted {
		success = "The server is trusted: Ask there can answer from internal content, for anyone who is a member of it."
	}
	s.finishSectionMutation(w, r, discordcx.ConnectorName, err, success)
}

func (s *Server) handleDiscordAskChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	on := r.Form.Get("on") == "yes"
	err := adminaction.SetDiscordAskInChannel(r.Context(), s.audit(r), s.discordChannels(), strings.TrimSpace(r.Form.Get("channel")), on)
	success := "Ask is off in the channel and its threads."
	if on {
		success = "Ask is on in the channel and its threads."
	}
	s.finishSectionMutation(w, r, discordcx.ConnectorName, err, success)
}

func (s *Server) handleDiscordAskDirectMessages(w http.ResponseWriter, r *http.Request) {
	if !s.requireMutation(w, r) {
		return
	}
	on := r.Form.Get("on") == "yes"
	err := adminaction.SetDiscordAskDirectMessages(r.Context(), s.audit(r), s.discordChannels(), on)
	success := "Ask no longer answers direct messages."
	if on {
		success = "Ask answers direct messages from members of the servers where it is on."
	}
	s.finishSectionMutation(w, r, discordcx.ConnectorName, err, success)
}

// discordChoiceError is what the page says about a refused channel choice.
func discordChoiceError(err error) (string, bool) {
	var choice *discordcx.ChoiceError
	if errors.As(err, &choice) {
		return choice.Message, true
	}
	return "", false
}
