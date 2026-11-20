package discord

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/bluesky-social/diderot/internal/events"
)

// What an answer may read. Discord gives a bot no email, so an asker has no
// Diderot identity and only Discord can say who may read what: an answer
// reads public content and the chosen channels its audience can read in
// Discord, never more because of who asked. Permissions are read from
// Discord at each turn, never cached, and a read that fails grants nothing.

// guildAccess is one guild's roles and channels, for computing who may read
// what in it.
type guildAccess struct {
	guildID   string
	rolePerms map[string]int64
	channels  map[string]channel
}

func (a *asker) readGuildAccess(ctx context.Context, guildID string) (guildAccess, error) {
	roles, err := a.client.guildRoles(ctx, guildID)
	if err != nil {
		return guildAccess{}, err
	}
	channels, err := a.client.guildChannels(ctx, guildID)
	if err != nil {
		return guildAccess{}, err
	}
	g := guildAccess{guildID: guildID, rolePerms: map[string]int64{}, channels: map[string]channel{}}
	for _, r := range roles {
		g.rolePerms[r.ID] = parsePerms(r.Permissions)
	}
	for _, c := range channels {
		g.channels[c.ID] = c
	}
	return g, nil
}

const readPerms = permViewChannel | permReadMessageHistory

// open reports whether every member of the guild can read a channel's
// history: @everyone may view it and read its history, and no overwrite
// denies either to anyone. It errs to less: a deny that would not matter
// still closes the channel.
func (g guildAccess) open(channelID string) bool {
	c, ok := g.channels[channelID]
	if !ok {
		return false
	}
	if channelPermissions(g.guildID, g.rolePerms, nil, "", c.PermissionOverwrites)&readPerms != readPerms {
		return false
	}
	for _, o := range c.PermissionOverwrites {
		if o.ID != g.guildID && parsePerms(o.Deny)&readPerms != 0 {
			return false
		}
	}
	return true
}

// viewersRead reports whether everyone who can view a channel can also read
// its history, so that an answer posted there shows its audience nothing
// they could not scroll back to. Discord ORs a member's roles together, so
// it is enough that @everyone, and @everyone with any one role, never views
// without reading, provided no overwrite denies the history (a deny could
// take it from a member whose view comes from another role) and no member
// overwrite grants the view alone.
func (g guildAccess) viewersRead(channelID string) bool {
	c, ok := g.channels[channelID]
	if !ok {
		return false
	}
	for _, o := range c.PermissionOverwrites {
		if parsePerms(o.Deny)&permReadMessageHistory != 0 {
			return false
		}
		if o.Type == 1 && parsePerms(o.Allow)&permViewChannel != 0 && parsePerms(o.Allow)&permReadMessageHistory == 0 {
			return false
		}
	}
	viewsWithoutReading := func(roles []string) bool {
		p := channelPermissions(g.guildID, g.rolePerms, roles, "", c.PermissionOverwrites)
		return p&permViewChannel != 0 && p&permReadMessageHistory == 0
	}
	if viewsWithoutReading(nil) {
		return false
	}
	for roleID := range g.rolePerms {
		if roleID != g.guildID && viewsWithoutReading([]string{roleID}) {
			return false
		}
	}
	return true
}

// memberReads reports whether a member can view a channel and read its
// history.
func (g guildAccess) memberReads(m member, userID, channelID string) bool {
	c, ok := g.channels[channelID]
	if !ok {
		return false
	}
	return channelPermissions(g.guildID, g.rolePerms, m.Roles, userID, c.PermissionOverwrites)&readPerms == readPerms
}

// chosenIn is the chosen channels of a guild.
func (a *asker) chosenIn(ctx context.Context, guildID string) ([]string, error) {
	var selections map[string]Selection
	if _, err := a.deps.State.Get(ctx, selectionKey, &selections); err != nil {
		return nil, err
	}
	var ids []string
	for _, id := range selectedIDs(selections) {
		if selections[id].GuildID == guildID {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// audience is what an answer posted in a guild channel may read.
type audience struct {
	// Scopes are public content, the guild's chosen channels every member
	// can read, and the channel itself when it is chosen and everyone who
	// can view it can read its history.
	Scopes []string
	// ReadsHistory says everyone who can view the channel can read its
	// history, so the thread's own earlier messages may be the turn's
	// context, chosen or not.
	ReadsHistory bool
}

// channelAudience is the audience of an answer posted in channelID (or one
// of its threads). In a trusted server whose channels only members read, it
// also reads internal: every viewer is a member.
func (a *asker) channelAudience(ctx context.Context, settings AskSettings, guildID, channelID string) (audience, error) {
	chosen, err := a.chosenIn(ctx, guildID)
	if err != nil {
		return audience{}, err
	}
	g, err := a.readGuildAccess(ctx, guildID)
	if err != nil {
		return audience{}, err
	}
	out := audience{Scopes: []string{events.ScopePublic}, ReadsHistory: g.viewersRead(channelID)}
	if settings.Trusted[guildID] && a.membersOnly(ctx, g, channelID) {
		out.Scopes = append(out.Scopes, events.ScopeInternal)
	}
	for _, id := range chosen {
		if g.open(id) || (id == channelID && out.ReadsHistory) {
			out.Scopes = append(out.Scopes, ChannelScope(id))
		}
	}
	return out, nil
}

// membersOnly reports whether only members of the guild read what is posted
// in channelID (or its threads), erring to no. Not in an announcement
// channel, whose posts can be published to the servers that follow it, nor
// in a server people can read without joining (from the server directory,
// or a preview before joining). A failed read is a no.
func (a *asker) membersOnly(ctx context.Context, g guildAccess, channelID string) bool {
	c, ok := g.channels[channelID]
	if !ok || c.Type == channelAnnouncement {
		return false
	}
	info, err := a.client.guild(ctx, g.guildID)
	if err != nil {
		a.logger.Warn("discord ask: reading a trusted server failed; the answer leaves internal content out", "guild", g.guildID, "err", err)
		return false
	}
	return !info.previewable()
}

// memberScopes is what an answer only userID reads may read: public
// content, and the chosen channels of the guilds where Ask is on that they
// can view and read the history of; and internal when they are a full
// member (past screening, not a guest) of a trusted one. member says they
// are in at least one of those guilds. A guild whose read fails contributes
// nothing.
func (a *asker) memberScopes(ctx context.Context, settings AskSettings, userID string) (scopes []string, isMember bool) {
	scopes = []string{events.ScopePublic}
	for _, guildID := range settings.guilds() {
		m, err := a.client.guildMember(ctx, guildID, userID)
		if isStatus(err, 404) {
			continue
		}
		if err != nil {
			a.logger.Warn("discord ask: reading a member failed; their answer leaves the server out", "guild", guildID, "err", err)
			continue
		}
		isMember = true
		if settings.Trusted[guildID] && m.full() && !slices.Contains(scopes, events.ScopeInternal) {
			scopes = append(scopes, events.ScopeInternal)
		}
		chosen, err := a.chosenIn(ctx, guildID)
		if err != nil || len(chosen) == 0 {
			continue
		}
		g, err := a.readGuildAccess(ctx, guildID)
		if err != nil {
			a.logger.Warn("discord ask: reading a server's permissions failed; the answer leaves it out", "guild", guildID, "err", err)
			continue
		}
		for _, id := range chosen {
			if g.memberReads(m, userID, id) {
				scopes = append(scopes, ChannelScope(id))
			}
		}
	}
	return scopes, isMember
}

// sessionKey is the session a conversation continues in a place for an
// audience reading scopes: one per set of scopes, so that a session only
// ever runs with the scopes it started with. When the audience's access
// changes, in either direction, the next question starts a new session, and
// no one can ask the model to repeat what an earlier turn read under access
// they no longer have.
func sessionKey(scopes []string) string {
	sorted := slices.Clone(scopes)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(slices.Compact(sorted), "\n")))
	return "s-" + hex.EncodeToString(sum[:8])
}
