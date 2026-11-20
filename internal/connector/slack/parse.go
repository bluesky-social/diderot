// Package slack is the Slack connector: a socket-mode bot that
// subscribes to message events across every channel the bot has
// been invited to, parses each incoming event into events.Event
// shape, and feeds it through events.DBSink.
//
// Parse logic lives in parse.go (pure function over the raw Slack
// JSON). Mention / channel / usergroup / URL resolution is in
// directory.go. The socket-mode client + consumer goroutine live
// in slack.go; backfill via conversations.history lands separately.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
)

// rawMessage is the minimal subset of Slack's message event wire
// format the parser cares about. Slack sends a LOT more fields —
// attachments, blocks, reactions, bot-specific metadata — which we
// preserve wholesale in events.Raw for debugging and future
// enrichment, but don't branch on at ingest time.
type rawMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Channel string `json:"channel"`
	User    string `json:"user"`
	BotID   string `json:"bot_id"`
	// BotProfile is present on newer app-authored messages where the
	// legacy username field is absent. Name is presentation only; BotID
	// remains the stable source identity.
	BotProfile *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"bot_profile"`
	// Username is the display name a classic webhook/integration sets
	// on a bot_message (Slack has no user record to resolve for these).
	// Empty for human messages. Used as the event Actor for bot_message
	// so alerts are attributable ("Alertmanager", "GitHub") in retrieval.
	Username        string      `json:"username"`
	Text            string      `json:"text"`
	TS              string      `json:"ts"`
	ThreadTS        string      `json:"thread_ts"`
	EventTS         string      `json:"event_ts"`
	DeletedTS       string      `json:"deleted_ts"`
	Message         *rawMessage `json:"message"`
	PreviousMessage *rawMessage `json:"previous_message"`
	Edited          *struct {
		User string `json:"user"`
		TS   string `json:"ts"`
	} `json:"edited"`
	// Files is populated for messages that carry file uploads:
	// image pastes, doc shares, code snippets, etc. For a pure
	// file-share with no text, Slack sets Text="" and Files has
	// one or more entries. We synthesize body text from filenames
	// + titles + permalinks so these events still contribute to
	// retrieval.
	Files []rawFile `json:"files"`
	// Attachments is how Slack delivers link unfurls — the preview
	// cards for bsky posts, articles, etc. They're added
	// asynchronously after a message is posted (Slack crawls the URL,
	// then re-sends the message as a message_changed carrying the
	// attachment). We fold their human-readable fields into the body
	// so a linked post is findable by what it *says*, not just its
	// URL. Note the async, ephemeral nature: an unfurl can vanish on a
	// later edit if Slack re-sends the message without it, and because
	// body is rebuilt from the raw event every time, our stored body
	// tracks whatever Slack last told us — which is the behavior we
	// want (don't index preview text a reader can no longer see).
	Attachments []rawAttachment `json:"attachments"`
}

// rawAttachment is the subset of a Slack message attachment we fold
// into body text. Slack attachments carry a lot of media/layout fields
// (image_url, blocks, colors, icons, ts); we keep only the fields that
// carry retrieval signal: which service the link points at, who
// authored it, and its title/preview text. See renderAttachments.
type rawAttachment struct {
	ServiceName   string `json:"service_name"`
	Footer        string `json:"footer"`
	AuthorName    string `json:"author_name"`
	AuthorSubname string `json:"author_subname"`
	Title         string `json:"title"`
	Text          string `json:"text"`
	Fallback      string `json:"fallback"`
	FromURL       string `json:"from_url"`
}

// rawFile is the minimal shape we care about off Slack's file
// object. Slack actually returns ~30 fields per file including
// thumbnails, mimetype, creator; we only need enough to produce a
// searchable stub, a citable URL, and a record for the
// slack_files / event_attachments tables that drive the FileWorker.
type rawFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Mimetype string `json:"mimetype"`
	// Mode is Slack's storage class for the file, and it — not the
	// mimetype — is what decides whether there are bytes to fetch:
	//
	//   hosted    Slack stores the bytes; url_private works.
	//   external  a Google Drive / Dropbox link. Slack has NO bytes,
	//             only a URL. Fetching
	//             them would need Workspace OAuth and a different
	//             permission model entirely.
	//   snippet   content is inline in the message object itself
	//             (preview / plain_text), not a separate file.
	//   email     a forwarded email Slack rendered to HTML.
	//   quip      Slack Canvas / Quip doc; separate API surface.
	//
	// Document ingest filters on hosted for exactly this reason.
	Mode      string `json:"mode"`
	Size      int64  `json:"size"`
	URLPriv   string `json:"url_private"`
	Permalink string `json:"permalink"`
}

// ParseResult captures the two outcomes a message-event parse can
// produce: an Event to upsert, or a (source, externalID) tuple to
// tombstone. Mutually exclusive. A parse that returns both fields
// zero is a bug; callers should log and move on.
type ParseResult struct {
	// Event, when non-nil, is ready to pass to events.DBSink.Upsert.
	Event *events.Event
	// Delete, when true, means the caller should call
	// events.DBSink.Delete(Source, ExternalID).
	Delete     bool
	Source     string
	ExternalID string
}

// Resolver is the dependency ParseMessage uses to rewrite Slack
// mention refs into human-readable text. Optional: pass nil to get
// the raw wire format through untouched (useful for dry-run parsing
// in tests). In production always pass a real *Directory — the
// embedder scores much better on "@alice said X" than "@U12345 said
// X", and passing nil leaves HTML entities like &gt; visible too.
type Resolver interface {
	Resolve(ctx context.Context, text string) string
}

// ParseMessage turns one Slack message / message_changed /
// message_deleted event into a ParseResult. workspaceHost is the
// Slack workspace hostname (e.g. "example.slack.com") used to build
// the permalink URL stored as events.URL. If resolver is non-nil,
// mention refs in the body are rewritten to human form before
// events.Body is populated; the raw payload stays untouched in
// events.Raw.
//
// Errors are reserved for malformed payloads that don't look like
// Slack message events at all. Reasonable parse outcomes that we
// intentionally want to skip (bot self-messages on reaction events,
// channel_join subtypes we don't want to ingest, etc.) return a
// zero ParseResult with err == nil; callers check for that.
func ParseMessage(
	ctx context.Context,
	raw []byte,
	workspaceHost string,
	resolver Resolver,
) (ParseResult, error) {
	var m rawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ParseResult{}, fmt.Errorf("unmarshal slack event: %w", err)
	}

	var (
		channel     = m.Channel
		userID      string
		text        string
		ts          string
		deleted     bool
		threadTS    string
		isEdit      bool
		botUsername string
		botID       string
	)

	switch m.Subtype {
	case "message_changed":
		// The LIVE Events API's delta shape for this same deletion: Slack
		// sends a message_changed wrapper whose inner message is
		// subtype=tombstone when a socket-mode connection is live to see
		// the delete happen. Treat it exactly like message_deleted;
		// otherwise the literal placeholder "This message was deleted."
		// becomes searchable live corpus content.
		//
		// This is NOT the shape conversations.history / conversations.replies
		// return for a deleted-but-still-anchoring thread root — that's an
		// unwrapped top-level subtype=tombstone (the "tombstone" case below).
		// A history/sync re-walk hits the unwrapped shape, never this one.
		if m.Message != nil && m.Message.Subtype == "tombstone" {
			ts = m.Message.TS
			deleted = true
			src := m.PreviousMessage
			if src == nil {
				src = m.Message
			}
			if ts == "" && src != nil {
				ts = src.TS
			}
			if src != nil {
				userID = src.User
				threadTS = src.ThreadTS
			}
			break
		}
		// slack-go >= 0.16's MessageEvent.UnmarshalJSON backfills a
		// synthetic non-nil Message from the top-level payload when the
		// raw event has no "message" key, so a nil check no longer
		// detects a malformed message_changed. A real edit always
		// carries the edited author + body; the synthetic stand-in has
		// both empty (only ts/subtype/channel are copied down). Treat
		// "no author and no text" as the malformed shape and drop it,
		// which is what the pre-bump nil check did.
		if m.Message == nil || (m.Message.User == "" && botIdentity(m.Message) == "" && m.Message.Text == "") {
			return ParseResult{}, fmt.Errorf("message_changed without message body")
		}
		userID = m.Message.User
		botID = botIdentity(m.Message)
		botUsername = botDisplayName(m.Message)
		text = m.Message.Text
		ts = m.Message.TS
		threadTS = m.Message.ThreadTS
		isEdit = true
	case "message_deleted":
		ts = m.DeletedTS
		deleted = true
		// Slack puts the deleted message's original content in
		// `previous_message`, not `message`. Fall back to `message`
		// for forward-compat in case the shape ever changes.
		src := m.PreviousMessage
		if src == nil {
			src = m.Message
		}
		if src != nil {
			userID = src.User
			threadTS = src.ThreadTS
		}
	case "tombstone":
		// The REST/history-read snapshot shape: conversations.history and
		// conversations.replies return a thread root that was deleted while
		// it still had at least one live reply as a plain top-level message
		// AT THAT ts — not wrapped in message_changed — with
		// subtype="tombstone", user="USLACKBOT", hidden=true, and
		// reply_count/latest_reply still reflecting the live children.
		// Slack must keep an anchor there for the surviving replies; this
		// is the current state of that ts, not a delta. Treat it exactly
		// like message_deleted/the wrapped tombstone above so the tail
		// sweep's history re-walk (and the thread worker's replies walk,
		// and backfill) can repair a delete that was missed live.
		ts = m.TS
		deleted = true
		threadTS = m.ThreadTS
	case "", "thread_broadcast", "file_share":
		// Plain user message, a thread reply also posted to the
		// channel, or a file upload with a comment. All three are
		// ingest-worthy user content with the same top-level field
		// shape. file_share matters more than it looks: messages
		// posted via the files upload API (files.completeUploadExternal
		// / UploadFileV2) and some client drag-and-drop flows arrive
		// with this subtype, and skipping them would make the live path,
		// including ask routing, blind to them: the tail sweep would
		// ingest the text minutes later, but a mention in a file_share
		// message would never be answered. Anything else (channel_join,
		// channel_leave, topic changes) stays skippable — add subtypes
		// explicitly when one matters rather than ingesting everything
		// and regretting it.
		userID = m.User
		botID = botIdentity(&m)
		botUsername = botDisplayName(&m)
		text = m.Text
		ts = m.TS
		threadTS = m.ThreadTS
	case "bot_message":
		// Classic webhook/integration output: Alertmanager, CI, GitHub,
		// Grafana, Sentry. Same top-level field shape as a plain message
		// EXCEPT there's no `user` — the author lives in `bot_id` +
		// `username` — and the body is usually empty with all content in
		// attachments[] (folded into the body below via renderAttachments).
		// Skipping these would leave bot-heavy alerts channels mostly
		// uningested and orphan the human replies that thread off a
		// dropped alert root. ActorID
		// carries bot_id as the stable source identity while
		// slack_message_details.user_id deliberately remains empty (that
		// side table is used for human ask routing). Carry username or
		// bot_profile.name as the display actor when Slack supplies one.
		// diderot's own posts are NOT bot_message — it posts as its bot
		// *user* via chat.postMessage, so the selfUserID filter still
		// catches them; adding this subtype doesn't re-ingest our own
		// status spam.
		text = m.Text
		ts = m.TS
		threadTS = m.ThreadTS
		botID = botIdentity(&m)
		// Slackbot is the one classic bot_message shape with a Slack user
		// id but no bot_id. Preserve that stable source id too.
		if botID == "" && m.User == "USLACKBOT" {
			botID = m.User
		}
		botUsername = botDisplayName(&m)
	default:
		// Unhandled subtype — deliberately skip. Returning a zero
		// ParseResult tells the caller "no-op, don't log as error".
		return ParseResult{}, nil
	}
	// Slackbot sometimes appears as user=USLACKBOT without bot_id, including
	// inside message_changed. It is a bot source identity, not a directory
	// user or human asker.
	if botID == "" && userID == "USLACKBOT" {
		botID = userID
		userID = ""
	}
	if botUsername != "" && botID == "" && userID == "" {
		return ParseResult{}, fmt.Errorf("bot message missing stable identity")
	}

	if ts == "" || channel == "" {
		return ParseResult{}, fmt.Errorf("message missing ts or channel")
	}

	extID := externalID(channel, ts)

	if deleted {
		return ParseResult{
			Delete:     true,
			Source:     "slack",
			ExternalID: extID,
		}, nil
	}

	occurredAt, err := tsToTime(ts)
	if err != nil {
		return ParseResult{}, err
	}

	if resolver != nil {
		text = resolver.Resolve(ctx, text)
	}

	// Fold link unfurls into the body so a linked bsky post / article
	// is findable by its content, not just its bare URL. The author's
	// own words lead and the preview text follows — a message that is
	// *just* a link reads as "<url>\n\n[link] <preview>". Rebuilt from
	// raw on every event, so it tracks Slack's current state (an unfurl
	// dropped on edit correctly drops here). Computed before the
	// empty-body fallback so an unfurl counts as real content and we
	// don't stamp the "(empty slack message)" placeholder over it.
	unfurls := renderAttachments(collectAttachments(&m))

	// Slack messages sometimes have genuinely empty text — pure image
	// share, pure reaction trigger. The events sink requires a
	// non-empty Body. For file shares, synthesize body text from the
	// files array so the event carries real retrieval signal
	// (filenames like "k8s-cluster-errors.png" are searchable). For
	// everything else, fall back to a placeholder — unless an unfurl
	// gave us content, in which case the unfurl block IS the body.
	//
	// The emptiness check is against SanitizeText(text), not text
	// itself: a message whose text is only a NUL byte (or other
	// content the sink would sanitize to nothing) is TrimSpace-non-empty
	// (NUL isn't whitespace) but has nothing to index, same as a truly
	// empty one. Missing this left such a message's Body as the raw NUL
	// byte all the way to Sink.Upsert, which now sanitizes it to ""
	// and fails validate()'s "Body is required" check — forever, since
	// the message's content never changes. The per-message ingest loop
	// (ingestRawMessage's callers) counts and continues on that error,
	// so it isn't a stall, but the message would never be indexed.
	if strings.TrimSpace(events.SanitizeText(text)) == "" {
		if files := collectFiles(&m); len(files) > 0 {
			text = renderFilesBody(files)
		} else if unfurls == "" {
			text = "(empty slack message)"
		}
	}

	if unfurls != "" {
		if strings.TrimSpace(events.SanitizeText(text)) == "" {
			text = unfurls
		} else {
			text = text + "\n\n" + unfurls
		}
	}

	// Actor (display name) is populated by the caller after parse
	// via Directory.LookupUser — ParseMessage stays pure so the
	// backfill + reembed paths can reuse it without dragging a
	// Slack API dependency through. Empty Actor is valid; the
	// embedder contextual prefix falls back gracefully.
	ev := &events.Event{
		Source:     "slack",
		ExternalID: extID,
		OccurredAt: occurredAt,
		ActorID:    userID,
		ThreadID:   threadKey(channel, threadTS, ts),
		Title:      "", // Slack messages don't have a natural title
		Body:       text,
		URL:        permalink(workspaceHost, channel, ts, threadTS),
		Raw:        json.RawMessage(raw),
	}
	// A newer app-authored message can carry both a bot_id and its bot-user
	// `user` id. Prefer the user id in that shape: it participates in the
	// normal Slack directory and self-message filtering. Classic bot_message
	// events and bot edits have no user id, so bot_id is their identity.
	isBotActor := botID != "" && userID == ""
	if isBotActor {
		ev.ActorID = botID
		ev.ActorCanonical = SlackBotActorCanonical(botID)
	}
	// bot_message has no `user` for the caller's Directory lookup to
	// resolve, but `username` is the human-readable sender the webhook
	// set. Stamp it directly so alerts are attributable in retrieval;
	// the caller's lookup is a no-op when ActorID is empty and leaves
	// this value intact.
	if isBotActor && botUsername != "" {
		ev.Actor = botUsername
	}
	_ = isEdit // reserved for the slack_message_details side-table row
	return ParseResult{Event: ev}, nil
}

func botDisplayName(m *rawMessage) string {
	if m == nil {
		return ""
	}
	if m.Username != "" {
		return m.Username
	}
	if m.BotProfile != nil {
		return m.BotProfile.Name
	}
	return ""
}

func botIdentity(m *rawMessage) string {
	if m == nil {
		return ""
	}
	if m.BotID != "" {
		return m.BotID
	}
	if m.BotProfile != nil {
		return m.BotProfile.ID
	}
	return ""
}

// externalID is the events.external_id value for a Slack message:
// `<channel_id>:<ts>`. Stable across edits (edits come in as
// message_changed events with the same ts) so ON CONFLICT on
// UNIQUE(source, external_id) correctly lands them as an UPDATE.
func externalID(channel, ts string) string {
	return channel + ":" + ts
}

// tsToTime parses Slack's "seconds.microseconds" string timestamp
// into a time.Time. Microseconds are six digits; Slack right-pads
// with zeros if fewer are present.
func tsToTime(ts string) (time.Time, error) {
	dot := strings.IndexByte(ts, '.')
	var secs, micros int64
	var err error
	if dot < 0 {
		secs, err = strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse ts seconds: %w", err)
		}
	} else {
		secs, err = strconv.ParseInt(ts[:dot], 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse ts seconds: %w", err)
		}
		microsStr := ts[dot+1:]
		for len(microsStr) < 6 {
			microsStr += "0"
		}
		microsStr = microsStr[:6]
		micros, err = strconv.ParseInt(microsStr, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse ts micros: %w", err)
		}
	}
	return time.Unix(secs, micros*1000).UTC(), nil
}

// permalink builds the same canonical Slack deep-link returned by
// chat.getPermalink (https://docs.slack.dev/reference/methods/chat.getPermalink/).
// Host is the workspace subdomain (for example,
// "example.slack.com"); ts is flattened in the archive path. Slack
// includes thread_ts + cid whenever the message belongs to a thread,
// including on the thread root where threadTS == ts.
func permalink(host, channel, ts, threadTS string) string {
	if host == "" {
		return ""
	}
	base := fmt.Sprintf("https://%s/archives/%s/p%s",
		host, channel, strings.ReplaceAll(ts, ".", ""))
	if threadTS == "" {
		return base
	}
	// Preserve Slack's own parameter order. Query ordering is not
	// semantically significant, but byte-identical output makes stored
	// values directly comparable with chat.getPermalink responses.
	return base + "?thread_ts=" + url.QueryEscape(threadTS) +
		"&cid=" + url.QueryEscape(channel)
}

// threadKey returns the value stored in events.thread_id. For thread
// replies and thread_broadcasts, this is `channel:thread_ts`. For
// the root message of a thread, Slack doesn't include a thread_ts
// even though one exists, so we fall back to `channel:ts`. For
// standalone messages (no thread at all) we return "" so the
// events.thread_id partial index excludes them.
//
// Using the channel-prefixed form (rather than bare thread_ts) is
// what makes events.thread_id unique across channels; two different
// threads that happen to start at the same microsecond-precision ts
// in different channels wouldn't collide on retrieval.
func threadKey(channel, threadTS, ts string) string {
	if threadTS == "" {
		return ""
	}
	return channel + ":" + threadTS
}

// MessageDetails is the slack_message_details row shape. Returned
// alongside ParseResult so the consumer knows what to write into the
// side table after the events.Event lands.
//
// Attachments is the list of file uploads on the message in source
// order. The consumer uses it to populate the slack_files /
// event_attachments tables that drive the FileWorker (S3 mirror +
// VLM caption). Empty for messages without files. Order is
// preserved so the bridge row's attachment_idx is stable across
// re-ingests.
type MessageDetails struct {
	ChannelID string
	UserID    string
	ThreadTS  string
	IsEdit    bool
	// EditedTS is the edit-event timestamp from message.edited.ts, present
	// only on message_changed payloads that carry an edited sub-object.
	// Empty for plain messages, message_deleted, and message_changed payloads
	// that lack the edited field (rare but defensive). Used by ask routing to
	// form a composite claim key (origTS + ":" + editedTS) so distinct edits
	// each claim a fresh slot while redeliveries of the same edit dedup.
	EditedTS    string
	HasFiles    bool
	Attachments []FileAttachment
}

// FileAttachment is the per-file projection of rawFile shared by the
// ingest sink (event_images reconcile) and the admin fetch surface
// (filename + permalink rendering). URLPriv stays on the events.raw
// blob; the FileWorker reads url_private out of raw when it needs to
// fetch bytes.
type FileAttachment struct {
	FileID   string
	Name     string
	Title    string
	Mimetype string
	// Mode is Slack's storage class ("hosted", "external", "snippet",
	// "email", "quip"). See rawFile.Mode — it decides whether bytes are
	// fetchable at all, which is what document ingest filters on.
	Mode      string
	Size      int64
	Permalink string
}

// DetailsFrom extracts the side-table row from a parsed raw message.
// Callers run this after events.DBSink.Upsert returns the event id,
// then INSERT ... ON CONFLICT DO UPDATE into slack_message_details.
func DetailsFrom(raw []byte) (MessageDetails, error) {
	var m rawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return MessageDetails{}, fmt.Errorf("unmarshal: %w", err)
	}
	files := collectFiles(&m)
	d := MessageDetails{
		ChannelID:   m.Channel,
		HasFiles:    len(files) > 0,
		Attachments: projectAttachments(files),
	}
	switch m.Subtype {
	case "message_changed":
		if m.Message != nil {
			if !isBotAuthored(m.Message) {
				d.UserID = m.Message.User
			}
			d.ThreadTS = m.Message.ThreadTS
			if m.Message.Edited != nil {
				d.EditedTS = m.Message.Edited.TS
			}
		}
		d.IsEdit = true
	case "message_deleted":
		src := m.PreviousMessage
		if src == nil {
			src = m.Message
		}
		if src != nil {
			if !isBotAuthored(src) {
				d.UserID = src.User
			}
			d.ThreadTS = src.ThreadTS
		}
	default:
		if !isBotAuthored(&m) {
			d.UserID = m.User
		}
		d.ThreadTS = m.ThreadTS
	}
	return d, nil
}

// isBotAuthored protects slack_message_details.user_id's human-only
// contract. Slack has emitted bot_id, bot_profile, legacy username,
// subtype=bot_message, and Slackbot's user=USLACKBOT over time.
func isBotAuthored(m *rawMessage) bool {
	return m != nil && (m.BotID != "" || m.BotProfile != nil || m.Username != "" ||
		m.Subtype == "bot_message" || m.User == "USLACKBOT")
}

// projectAttachments turns the parsed rawFile list into the DB-side
// FileAttachment shape. Skips entries without a file_id (Slack
// occasionally wraps a tombstoned file with empty fields; nothing
// useful to record). Returns nil for empty input so the caller's
// IsEdit/HasFiles distinction stays clean.
func projectAttachments(files []rawFile) []FileAttachment {
	if len(files) == 0 {
		return nil
	}
	out := make([]FileAttachment, 0, len(files))
	for _, f := range files {
		if f.ID == "" {
			continue
		}
		out = append(out, FileAttachment{
			FileID:    f.ID,
			Name:      f.Name,
			Title:     f.Title,
			Mimetype:  f.Mimetype,
			Mode:      f.Mode,
			Size:      f.Size,
			Permalink: f.Permalink,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// collectFiles returns the effective files slice on a message event
// regardless of subtype. Plain messages put files at the top level;
// several subtypes nest them under .message, and message_deleted
// carries them on .previous_message. Nil if no files anywhere.
//
// The nested fallback is deliberately NOT gated on a subtype
// allowlist. Slack nests files under .message for more subtypes
// than it documents (file_share among them) and adds subtypes
// without notice; an allowlist silently drops the attachments of
// any subtype it does not name (no has_files stamp, no event_images
// rows, nothing reaches the caption pipeline). So the shape of this
// function has to be "find the files wherever they are," not "know
// every subtype that nests them."
//
// Order matters: top-level first (the common case and the most
// specific), then the nested shapes. message_deleted is called out
// explicitly because it is the one subtype where BOTH nested shapes
// can be present and they mean different things — .previous_message
// is the pre-delete state (the files that were actually shared) while
// a .message on a delete envelope is the tombstone. Preferring
// .previous_message there preserves the behavior the old
// subtype-switch had; every other subtype prefers .message.
func collectFiles(m *rawMessage) []rawFile {
	if m == nil {
		return nil
	}
	if len(m.Files) > 0 {
		return m.Files
	}
	if m.Subtype == "message_deleted" {
		if m.PreviousMessage != nil && len(m.PreviousMessage.Files) > 0 {
			return m.PreviousMessage.Files
		}
	}
	if m.Message != nil && len(m.Message.Files) > 0 {
		return m.Message.Files
	}
	if m.PreviousMessage != nil && len(m.PreviousMessage.Files) > 0 {
		return m.PreviousMessage.Files
	}
	return nil
}

// collectAttachments pulls the unfurl attachments off whichever nested
// message shape applies, mirroring collectFiles — including its shape:
// "find them wherever they are," not "know every subtype that nests
// them." Switching on subtype == "message_changed" would silently
// drop unfurls nested under any other subtype, the same failure
// collectFiles avoids for files.
//
// message_deleted is not special-cased the way collectFiles does it: a
// deleted message is tombstoned before body composition runs, so its
// unfurls are never rendered. The .previous_message fallback below is
// therefore unreachable on deletes in practice and exists only so a
// future subtype that nests this way doesn't lose data.
func collectAttachments(m *rawMessage) []rawAttachment {
	if m == nil {
		return nil
	}
	if len(m.Attachments) > 0 {
		return m.Attachments
	}
	if m.Message != nil && len(m.Message.Attachments) > 0 {
		return m.Message.Attachments
	}
	if m.PreviousMessage != nil && len(m.PreviousMessage.Attachments) > 0 {
		return m.PreviousMessage.Attachments
	}
	return nil
}

// renderFilesBody builds a searchable body string from a files list.
// Each file contributes a line like:
//
//	[file] k8s-cluster-errors.png — screenshot of dashboard
//	<https://example.slack.com/files/.../k8s-cluster-errors.png>
//
// Name + title carry the retrieval signal (filenames are
// information-dense in practice); the permalink is there so
// synthesis can cite the file. url_private is deliberately NOT
// surfaced — it's an auth-gated bytes URL that 302s through
// Slack auth and is useless to anyone reading body text. If
// Slack omits the permalink (rare but possible on some bot
// uploads), we emit just the label; the parent message's URL
// is always available as the click-through.
func renderFilesBody(files []rawFile) string {
	var b strings.Builder
	b.WriteString("[file share]")
	for _, f := range files {
		label := f.Name
		if f.Title != "" && f.Title != f.Name {
			label = f.Name + " — " + f.Title
		}
		if label == "" {
			label = "(unnamed)"
		}
		b.WriteString("\n")
		b.WriteString(label)
		if f.Permalink != "" {
			b.WriteString("\n")
			b.WriteString(f.Permalink)
		}
	}
	return b.String()
}

// renderAttachments turns Slack link-unfurl attachments into body text
// the embedder and the summarizer model can use. Each attachment that
// carries textual signal contributes a block like:
//
//	[link] bluesky — Alice Example (@alice.example.com)
//	the release notes for this week are up, take a look …
//	https://bsky.app/profile/alice.example.com/post/3kexamplepost
//
// The header line names the source (service_name/footer) and author so
// the model has provenance; then the preview text (the actual linked
// content — a bsky post's words, an article's lede); then from_url so
// synthesis can cite the underlying link rather than the Slack message.
// Media/layout fields (image_url, icons, colors, ts) are dropped — no
// retrieval value. Attachments with no title/text/fallback (e.g. a bare
// image unfurl) carry no signal and are skipped entirely; if that
// leaves nothing, we return "" and the caller appends nothing.
func renderAttachments(atts []rawAttachment) string {
	var blocks []string
	for _, a := range atts {
		// Prefer the structured fields; fall back to Slack's own
		// `fallback` (a pre-rendered "<author>: <text>" string) only
		// when we have neither a title nor body text of our own.
		title := strings.TrimSpace(a.Title)
		body := strings.TrimSpace(a.Text)
		if title == "" && body == "" {
			body = strings.TrimSpace(a.Fallback)
		}
		if title == "" && body == "" {
			continue // no textual signal — skip (pure media unfurl)
		}

		// Header: "[link] <source> — <author> (<subname>)". Every piece
		// is optional; assemble only what's present so we never emit
		// dangling separators.
		source := firstNonEmpty(a.ServiceName, a.Footer)
		author := strings.TrimSpace(a.AuthorName)
		if sub := strings.TrimSpace(a.AuthorSubname); sub != "" {
			if author != "" {
				author = author + " (" + sub + ")"
			} else {
				author = sub
			}
		}
		header := "[link]"
		if source != "" {
			header += " " + source
		}
		if author != "" {
			if source != "" {
				header += " — " + author
			} else {
				header += " " + author
			}
		}

		var b strings.Builder
		b.WriteString(header)
		if title != "" {
			b.WriteString("\n")
			b.WriteString(title)
		}
		if body != "" {
			b.WriteString("\n")
			b.WriteString(body)
		}
		if url := strings.TrimSpace(a.FromURL); url != "" {
			b.WriteString("\n")
			b.WriteString(url)
		}
		blocks = append(blocks, b.String())
	}
	return strings.Join(blocks, "\n\n")
}

// firstNonEmpty returns the first argument that is non-empty after
// trimming, or "" if all are empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
