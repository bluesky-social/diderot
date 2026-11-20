package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// prefixMaxLen caps the context prefix at this many runes. Long
// prefixes eat into the embedder's effective context window and
// dilute the signal from the actual content. 250 runes is plenty
// for "channel_name + thread_subject + actor".
const prefixMaxLen = 250

// PrefixBuilder composes the event_chunks.context_prefix string for
// a slack message. Looks up the channel name via Directory (cached,
// with negative-cache fallback) and — for thread replies — fetches
// the thread-root message body from Postgres to give the chunk a
// topic anchor.
//
// Design of the prefix content. `by @<actor>` is included when the
// caller has a resolved display name — critical for "what has alice
// been working on" style queries, where the vector needs the
// author's identity baked in. Actor-less messages (Directory miss)
// fall back to the author-free form.
//
//	Top-level message:
//	  "Slack message in #<channel-name> by @<actor>:"
//	  "Slack message in #<channel-name>:"                   (no actor)
//
//	Thread reply (thread root is in our DB):
//	  "Slack reply in #<channel-name> by @<actor> to thread \"<root-excerpt>\":"
//	  "Slack reply in #<channel-name> to thread \"<root-excerpt>\":"
//
//	Thread reply (thread root NOT in our DB — race or out-of-scope):
//	  "Slack reply in #<channel-name> by @<actor>:"
//	  "Slack reply in #<channel-name>:"
//
//	DM or unknown channel name:
//	  "Slack message by @<actor>:"
//	  "Slack message:"
//
// The prefix is prepended to each chunk's content at embed time.
// Raw content is preserved verbatim in events.body /
// event_chunks.content so citations show the original text.
type PrefixBuilder struct {
	dir  *Directory
	pool *pgxpool.Pool
}

// NewPrefixBuilder constructs a PrefixBuilder. Both args are required;
// nil panics on first call. Pass a shared Directory so channel-name
// lookups benefit from the same TTL cache the mention resolver uses.
func NewPrefixBuilder(dir *Directory, pool *pgxpool.Pool) *PrefixBuilder {
	return &PrefixBuilder{dir: dir, pool: pool}
}

// Build returns the context prefix for a message in channelID with
// threadTS. Pass threadTS="" for top-level messages. actor is the
// already-resolved display name (caller gets this from
// Directory.LookupUser); empty string means "unknown" and drops
// the "by @..." fragment.
//
// Errors from Slack/DB lookups are swallowed — a degraded prefix
// (falling back to channel ID or skipping the thread excerpt) is
// always preferable to failing the upsert over a prefix we could
// have stored as NULL anyway.
func (b *PrefixBuilder) Build(ctx context.Context, channelID, threadTS, ts, actor string) string {
	channel := b.channelLabel(ctx, channelID)
	by := ""
	if actor != "" {
		by = fmt.Sprintf(" by @%s", actor)
	}

	// Top-level message (or thread root — threadTS may equal ts).
	if threadTS == "" || threadTS == ts {
		if channel == "" {
			return fmt.Sprintf("Slack message%s:", by)
		}
		return fmt.Sprintf("Slack message in %s%s:", channel, by)
	}

	// Thread reply. Fetch the thread-root's body for the subject
	// excerpt. The root's external_id is channel:thread_ts in our
	// schema.
	excerpt := b.threadRootExcerpt(ctx, channelID, threadTS)
	switch {
	case channel == "" && excerpt == "":
		return fmt.Sprintf("Slack reply%s:", by)
	case channel == "" && excerpt != "":
		return fmt.Sprintf("Slack reply%s to thread \"%s\":", by, excerpt)
	case channel != "" && excerpt == "":
		return fmt.Sprintf("Slack reply in %s%s:", channel, by)
	default:
		return fmt.Sprintf("Slack reply in %s%s to thread \"%s\":", channel, by, excerpt)
	}
}

// channelLabel returns "#channel-name" when we can resolve it, the
// bare channel ID in angle brackets otherwise. Empty string for
// DMs (channel IDs starting with D) since there's no meaningful
// channel name to surface.
func (b *PrefixBuilder) channelLabel(ctx context.Context, channelID string) string {
	if channelID == "" {
		return ""
	}
	if strings.HasPrefix(channelID, "D") {
		// IM channel: the "channel name" is the DM counterparty, which
		// adds nothing a person reading the prefix would recognize.
		return ""
	}
	name, err := b.dir.LookupChannel(ctx, channelID)
	if err != nil || name == "" {
		return "#" + channelID
	}
	return "#" + name
}

// threadRootExcerpt fetches the first ~120 runes of the thread-root
// message's body. Used as a stand-in for "thread subject" since
// Slack threads don't have a title; the first message sets the
// topic. Returns "" on any failure or if the root isn't in our DB
// yet (e.g., during backfill running in reverse-chronological order,
// or for threads started before the bot joined).
func (b *PrefixBuilder) threadRootExcerpt(ctx context.Context, channelID, threadTS string) string {
	externalID := channelID + ":" + threadTS

	var body string
	err := b.pool.QueryRow(ctx,
		`SELECT body FROM events
		  WHERE source = 'slack' AND external_id = $1`,
		externalID,
	).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		return ""
	}
	return truncateRunes(strings.ReplaceAll(body, "\n", " "), 120)
}

// truncateRunes safely trims to n runes (not bytes). Byte-level
// slicing would risk splitting a multi-byte codepoint, producing
// invalid UTF-8 that would also break downstream embedders.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
