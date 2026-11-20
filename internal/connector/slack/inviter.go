package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/slack-go/slack"
)

// Inviter wraps the slack-go calls used by the admin invite
// endpoint. ResolveChannel turns a name-or-id into the canonical
// channel id (via the directory cache), and Invite calls
// conversations.invite under a USER token to add the bot to the
// channel.
//
// Two clients on purpose: the directory uses the bot client for
// reads (cheaper, doesn't need user-token scopes); the invite call
// uses the user client because xoxb tokens can't invite into
// private channels they aren't already members of. The user client
// is tied to a real human's identity (the operator's), so it can
// invite anywhere that human can.
type Inviter struct {
	dir        *Directory
	bot        *slack.Client
	userClient *slack.Client

	mu        sync.Mutex
	botUserID string
}

// NewInviter wires the directory, the bot-token client and the
// user-token client into an Inviter. The bot's user_id (the U...
// that gets invited into the channel) is learned from the bot
// client the first time it is needed, so building an Inviter
// calls nothing.
func NewInviter(dir *Directory, bot, userClient *slack.Client) *Inviter {
	return &Inviter{dir: dir, bot: bot, userClient: userClient}
}

// botID is the bot's user_id, from auth.test until one succeeds.
func (i *Inviter) botID(ctx context.Context) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.botUserID == "" {
		auth, err := i.bot.AuthTestContext(ctx)
		if err != nil {
			return "", fmt.Errorf("auth.test: %w", err)
		}
		i.botUserID = auth.UserID
	}
	return i.botUserID, nil
}

// ResolveChannel accepts a "C0123" id, "#name", or "name" and
// returns (channel_id, name). On id input the call is just a
// directory hit; on name input it walks conversations.list. The
// directory caches both directions so subsequent invocations are
// cheap.
func (i *Inviter) ResolveChannel(ctx context.Context, nameOrID string) (string, string, error) {
	if i == nil || i.dir == nil {
		return "", "", errors.New("inviter not configured")
	}
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return "", "", errors.New("empty channel reference")
	}
	// Channel IDs always start with C/G/D and are uppercase
	// alphanumeric. If it looks like an id, hit
	// conversations.info via the directory; otherwise treat as a
	// name.
	if looksLikeChannelID(nameOrID) {
		name, err := i.dir.LookupChannel(ctx, nameOrID)
		if err != nil {
			// Directory got something — could be a transient
			// fetch failure on a never-cached id. Surface as
			// "not found" so the caller can decide. Fall back to
			// returning the id without a name; the invite
			// itself is what matters.
			return nameOrID, "", nil
		}
		return nameOrID, name, nil
	}
	id, err := i.dir.LookupChannelByName(ctx, strings.TrimPrefix(nameOrID, "#"))
	if err != nil {
		return "", "", err
	}
	return id, strings.TrimPrefix(nameOrID, "#"), nil
}

// IsBotInChannel proxies to the directory's live membership check.
// Used by the admin backfill endpoint to validate "the bot is
// actually in this channel" before enqueueing — the queue worker
// would error out on conversations.history with not_in_channel
// otherwise, with a less actionable error.
func (i *Inviter) IsBotInChannel(ctx context.Context, channelID string) (bool, error) {
	if i == nil || i.dir == nil {
		return false, errors.New("inviter directory not configured")
	}
	return i.dir.IsBotInChannel(ctx, channelID)
}

// Invite calls conversations.invite under the user token, adding
// the bot's user_id to the named channel. Returns the slack-go
// error verbatim so callers (handleChannelInvite) can branch on
// "already_in_channel".
//
// If the inviter is not currently a member of the channel
// (Slack's "not_in_channel" error), we do an automatic
// join → invite → leave dance:
//
//  1. conversations.join under the user token (works for any
//     PUBLIC channel; private would also need an existing
//     member to invite us, which is the case we'd be trying to
//     work around — so we don't try).
//  2. retry conversations.invite — the user is now a member.
//  3. conversations.leave to clean up so the user-token's owner
//     isn't left as a "ghost" member of every channel diderot
//     ever joined.
//
// Each side effect is observable to channel members ("@avery
// joined / left the channel" lines in history). That's the
// price of standard-plan Slack — admin.conversations.invite
// without a member precondition is Enterprise Grid only.
//
// Best-effort leave: if step 3 fails we still report the
// invite as successful — the bot IS in the channel, which is
// what the operator asked for. The stray membership row on
// the user can be cleaned up manually.
func (i *Inviter) Invite(ctx context.Context, channelID string) error {
	if i == nil || i.userClient == nil {
		return errors.New("inviter user client not configured")
	}
	botUserID, err := i.botID(ctx)
	if err != nil {
		return err
	}
	_, err = i.userClient.InviteUsersToConversationContext(ctx, channelID, botUserID)
	if err == nil {
		return nil
	}
	if !isNotInChannelErr(err) {
		return fmt.Errorf("conversations.invite: %w", err)
	}

	// not_in_channel → try the join-invite-leave dance.
	if _, _, _, joinErr := i.userClient.JoinConversationContext(ctx, channelID); joinErr != nil {
		return fmt.Errorf("conversations.invite: not_in_channel; auto-join failed: %w", joinErr)
	}
	if _, retryErr := i.userClient.InviteUsersToConversationContext(ctx, channelID, botUserID); retryErr != nil {
		// Clean up our membership before returning — leaving
		// the user joined to a channel they shouldn't be in is
		// worse than reporting the invite failure cleanly.
		_, _ = i.userClient.LeaveConversationContext(ctx, channelID)
		return fmt.Errorf("conversations.invite (after auto-join): %w", retryErr)
	}
	if _, leaveErr := i.userClient.LeaveConversationContext(ctx, channelID); leaveErr != nil {
		// Bot is in the channel; the operator's request
		// succeeded. Stray membership on the user's side is a
		// hygiene problem they can mop up later.
		return nil
	}
	return nil
}

// Kick removes the bot's user_id from the channel via
// conversations.kick under the user token. Symmetric with Invite:
// the operator's user-token authority is what makes this work for
// channels diderot's bot is in.
//
// Used by the blacklist enforcement flow: blacklist add stops
// future ingest from matching paths, but the bot is still a
// member of the channel and would still receive socket-mode
// MessageEvents (which the live-event gate then drops). Kick is
// the airtight version — bot is no longer a channel member, so
// no events arrive in the first place.
//
// Idempotent on "bot already not in channel": Slack returns
// not_in_channel which the caller treats as success.
//
// Required scope on the user token: channels:write (or
// channels:manage). The user must also have authority to remove
// the bot — being the inviter, being a workspace admin, or being
// the channel creator usually suffices.
func (i *Inviter) Kick(ctx context.Context, channelID string) error {
	if i == nil || i.userClient == nil {
		return errors.New("inviter user client not configured")
	}
	botUserID, err := i.botID(ctx)
	if err != nil {
		return err
	}
	if err := i.userClient.KickUserFromConversationContext(ctx, channelID, botUserID); err != nil {
		return fmt.Errorf("conversations.kick: %w", err)
	}
	return nil
}

// isNotInChannelErr inspects a slack-go error for the
// "not_in_channel" sentinel, which fires when the user-token
// owner isn't a member of the target channel. Used to gate the
// auto-join dance.
func isNotInChannelErr(err error) bool {
	if err == nil {
		return false
	}
	return containsCi(err.Error(), "not_in_channel")
}

// containsCi is a case-insensitive substring match. Slack error
// strings are stable + ASCII, so it is safe.
func containsCi(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			d := sub[j]
			if d >= 'A' && d <= 'Z' {
				d += 'a' - 'A'
			}
			if c != d {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// looksLikeChannelID reports whether s parses as a slack channel
// id. C... = public, G... = legacy private, D... = DM. Diderot
// doesn't invite into DMs (the conversations.invite call would
// 400) but we accept the prefix for consistency with the rest of
// the codebase.
func looksLikeChannelID(s string) bool {
	if len(s) < 2 {
		return false
	}
	if s[0] != 'C' && s[0] != 'G' && s[0] != 'D' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		isAlnum := (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z')
		if !isAlnum {
			return false
		}
	}
	return true
}
