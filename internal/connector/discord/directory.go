package discord

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// readDirectory reads every guild the bot is in, its roles, and the
// channels it has that the connector could index, with whether each is
// private in Discord and whether the bot can read it.
func readDirectory(ctx context.Context, client *Client) (Directory, error) {
	bot, err := client.currentUser(ctx)
	if err != nil {
		return Directory{}, err
	}
	guilds, err := client.guilds(ctx)
	if err != nil {
		return Directory{}, err
	}
	app, err := client.currentApplication(ctx)
	if err != nil {
		return Directory{}, err
	}
	dir := Directory{ReadAt: time.Now().UTC(), Bot: bot.displayName(), ApplicationID: app.ID}
	for _, g := range guilds {
		entry, err := readGuild(ctx, client, g, bot.ID)
		if err != nil {
			return Directory{}, fmt.Errorf("server %s: %w", g.Name, err)
		}
		dir.Guilds = append(dir.Guilds, entry)
	}
	slices.SortFunc(dir.Guilds, func(a, b GuildEntry) int {
		if a.Name != b.Name {
			if a.Name < b.Name {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	return dir, nil
}

func readGuild(ctx context.Context, client *Client, g guild, botID string) (GuildEntry, error) {
	roles, err := client.guildRoles(ctx, g.ID)
	if err != nil {
		return GuildEntry{}, err
	}
	channels, err := client.guildChannels(ctx, g.ID)
	if err != nil {
		return GuildEntry{}, err
	}
	botMember, err := client.guildMember(ctx, g.ID, botID)
	if err != nil {
		return GuildEntry{}, err
	}
	entry := GuildEntry{ID: g.ID, Name: g.Name, Roles: map[string]string{}}
	rolePerms := map[string]int64{}
	for _, r := range roles {
		entry.Roles[r.ID] = r.Name
		rolePerms[r.ID] = parsePerms(r.Permissions)
	}
	categories := map[string]string{}
	for _, c := range channels {
		if c.Type == channelCategory {
			categories[c.ID] = c.Name
		}
	}
	for _, c := range channels {
		if !indexable(c.Type) {
			continue
		}
		everyone := channelPermissions(g.ID, rolePerms, nil, "", c.PermissionOverwrites)
		bot := channelPermissions(g.ID, rolePerms, botMember.Roles, botID, c.PermissionOverwrites)
		entry.Channels = append(entry.Channels, ChannelEntry{
			ID: c.ID, Name: c.Name, Type: c.Type, Category: categories[c.ParentID], Position: c.Position,
			Private:  everyone&permViewChannel == 0,
			Readable: bot&permViewChannel != 0 && bot&permReadMessageHistory != 0,
		})
	}
	slices.SortFunc(entry.Channels, func(a, b ChannelEntry) int {
		if a.Category != b.Category {
			if a.Category < b.Category {
				return -1
			}
			return 1
		}
		return a.Position - b.Position
	})
	return entry, nil
}

func parsePerms(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// channelPermissions computes a member's permissions in a channel, as
// Discord does: the @everyone role (whose ID is the guild's) and the
// member's roles, then the channel's @everyone, role, and member
// overwrites. Without a member (memberID ""), it is what @everyone alone
// has.
func channelPermissions(guildID string, rolePerms map[string]int64, memberRoles []string, memberID string, overwrites []overwrite) int64 {
	perms := rolePerms[guildID]
	for _, r := range memberRoles {
		perms |= rolePerms[r]
	}
	if perms&permAdministrator != 0 {
		return ^int64(0)
	}
	byID := map[string]overwrite{}
	for _, o := range overwrites {
		byID[o.ID] = o
	}
	if o, ok := byID[guildID]; ok {
		perms &^= parsePerms(o.Deny)
		perms |= parsePerms(o.Allow)
	}
	var allow, deny int64
	for _, r := range memberRoles {
		if o, ok := byID[r]; ok && o.Type == 0 {
			allow |= parsePerms(o.Allow)
			deny |= parsePerms(o.Deny)
		}
	}
	perms &^= deny
	perms |= allow
	if memberID != "" {
		if o, ok := byID[memberID]; ok && o.Type == 1 {
			perms &^= parsePerms(o.Deny)
			perms |= parsePerms(o.Allow)
		}
	}
	return perms
}
