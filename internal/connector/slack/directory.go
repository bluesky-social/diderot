package slack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"
)

// directoryTTL is how stale a cached entry can be before we re-fetch
// from Slack. 7 days is cheap (all lookups are free per endpoint's
// Tier 2/3 budget) and catches renames without being pushy about it.
const directoryTTL = 7 * 24 * time.Hour

// externalFlagsTTL is the shorter window used by LookupUserInfo's
// external-flag freshness check. The base directoryTTL is long
// because rename storms are rare and the resolved-name path is
// only cosmetic; the external flags (is_stranger / is_restricted /
// is_ultra_restricted / team_id) gate ask-routing decisions, so
// we want them to converge on workspace changes within hours, not
// days. 6h matches externalMembersTTL in channel_config.go.
const externalFlagsTTL = 6 * time.Hour

// slackAPI is the minimum surface Directory needs from slack-go, so
// tests can inject a fake without spinning up an HTTP server. Kept
// unexported — production callers always use NewDirectory with a
// real *slack.Client.
type slackAPI interface {
	GetUserInfoContext(ctx context.Context, user string, options ...slack.GetUserInfoOption) (*slack.User, error)
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)
	GetUserGroupsContext(ctx context.Context, options ...slack.GetUserGroupsOption) ([]slack.UserGroup, error)
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) (channels []slack.Channel, nextCursor string, err error)
}

// slackBotAPI is kept separate from slackAPI so directory consumers that only
// exercise users/channels in tests do not need to implement bots.info.
type slackBotAPI interface {
	GetBotInfoContext(ctx context.Context, parameters slack.GetBotInfoParameters) (*slack.Bot, error)
}

// Directory resolves <@Uxxx>, <#Cxxx|name>, and <!subteam^Sxxx|name>
// references to human-readable strings. Backed by Postgres tables
// (slack_users, slack_bots, slack_channel_info, slack_usergroups) with a TTL —
// lazy fill on miss, re-fetch on expiry.
//
// Safe for concurrent use. In-process single-flight prevents the
// same ID from being fetched multiple times in parallel by different
// messages; without it, a burst of messages mentioning the same new
// user would fire N parallel API calls.
type Directory struct {
	pool *pgxpool.Pool
	api  slackAPI

	sf singleflight

	// limiter is the optional global Slack-API budget governor.
	// When non-nil, every API call from the Directory goes
	// through Wait → call → RecordResult. nil means "no proactive
	// pacing" — callers fall back to slack-go's per-request
	// behavior which is fine for tests but undesirable in production.
	limiter *Limiter
}

// NewDirectory is a thin constructor. The pool and api are both
// required; nil will panic on first use.
func NewDirectory(pool *pgxpool.Pool, api *slack.Client) *Directory {
	return &Directory{pool: pool, api: api, sf: newSingleflight()}
}

// newDirectoryWithAPI is the test-friendly constructor; takes a
// slackAPI interface so tests can inject a fake.
func newDirectoryWithAPI(pool *pgxpool.Pool, api slackAPI) *Directory {
	return &Directory{pool: pool, api: api, sf: newSingleflight()}
}

// WithLimiter wires the budget governor for every Slack API call
// the directory makes (conversations.info on cache miss, the
// conversations.list walk in LookupChannelByName, etc). Optional;
// nil keeps prior behavior.
func (d *Directory) WithLimiter(l *Limiter) *Directory {
	d.limiter = l
	return d
}

// limiterCall is the directory-side adapter to WithSlackRetry. fn is
// the API call closure; the return value is passed through.
// 429s are honored via Retry-After + retry; nil → RecordOk;
// non-429 errors → RecordOther + return.
func (d *Directory) limiterCall(ctx context.Context, method string, fn func() error) error {
	return WithSlackRetry(ctx, d.limiter, method, fn)
}

// isRateLimited classifies an err as a Slack 429 sentinel.
// Centralizes the errors.As check used by every limiterCall site.
func isRateLimited(err error) bool {
	var rle *slack.RateLimitedError
	return errors.As(err, &rle)
}

// Resolve rewrites a Slack-formatted body in place: every <@Uxxx>,
// <#Cxxx|name>, <!subteam^Sxxx|name>, and <url|anchor> is replaced
// with its human form, and HTML entities (&gt;, &lt;, &amp;) are
// unescaped to plain characters.
//
// Order matters: refs are resolved BEFORE entity unescape. Slack
// escapes user-typed < > & so they can't be confused with ref
// delimiters. If we unescaped first, a message like "foo &gt; bar"
// would become "foo > bar" and confuse the ref regexes. Once all
// refs are gone the unescape is safe because any remaining `<` /
// `>` / `&` are user content.
//
// Errors from the Slack API are non-fatal: the specific ref stays
// in its raw form (with markup stripped), a warning is logged
// upstream, and the rest of the message resolves. Never fails
// the whole message over a single bad resolution.
func (d *Directory) Resolve(ctx context.Context, text string) string {
	if text == "" {
		return text
	}
	text = d.resolveUsers(ctx, text)
	text = d.resolveChannels(ctx, text)
	text = d.resolveUsergroups(ctx, text)
	text = resolveURLs(text)
	text = unescapeEntities(text)
	return text
}

// unescapeEntities reverses Slack's HTML-entity escaping of the
// three chars that are load-bearing in their markup: &, <, >. Per
// Slack's "Formatting text for app surfaces" docs, these are the
// *only* entities Slack escapes; everything else (smart quotes,
// dashes, emoji) comes through as UTF-8. Order matters here too —
// &amp; must be last so decoded & doesn't get mistaken for the
// start of another entity.
func unescapeEntities(s string) string {
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

// ---- user resolution ----

// userRef matches <@Uxxx> or <@Wxxx> with an optional |display
// override. Regular workspace users have IDs starting with U;
// Enterprise Grid workspace users have IDs starting with W. The
// display override has been deprecated by Slack for years but we
// handle it defensively.
var userRef = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^<>\n]*)?>`)

func (d *Directory) resolveUsers(ctx context.Context, text string) string {
	return userRef.ReplaceAllStringFunc(text, func(match string) string {
		m := userRef.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		userID := m[1]
		name, err := d.LookupUser(ctx, userID)
		if err != nil || name == "" {
			return "@" + userID // fallback: keep the ID visible but stripped of Slack markup
		}
		return "@" + name
	})
}

// LookupUser returns the best human name we have for a Slack user
// id: display_name if set, name otherwise, empty string if neither.
// Cache-first; on miss or TTL expiry, hits Slack's users.info.
func (d *Directory) LookupUser(ctx context.Context, userID string) (string, error) {
	result, err, _ := d.sf.Do("user:"+userID, func() (any, error) {
		return d.lookupUserUncached(ctx, userID)
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

func (d *Directory) lookupUserUncached(ctx context.Context, userID string) (string, error) {
	// Name-only resolution honors the longer 7-day TTL (renames
	// are cosmetic + rare). The shared inner helper lets the
	// caller pick which TTL applies.
	u, err := d.lookupUserStruct(ctx, userID, directoryTTL, true)
	if err != nil {
		return "", err
	}
	return pickName(u.DisplayName, u.Name), nil
}

// UserInfo bundles the slack_users columns the consumer needs at
// runtime. Returned by LookupUserInfo so callers (the ask gate, in
// particular) can introspect external-membership flags without a
// separate query.
type UserInfo struct {
	UserID            string
	Name              string
	DisplayName       string
	RealName          string
	Email             string
	TeamID            string
	IsBot             bool
	IsStranger        bool
	IsRestricted      bool
	IsUltraRestricted bool
	// TZ is the IANA timezone name (e.g. "America/Los_Angeles")
	// reported by Slack's users.info. Used to resolve the asker's
	// local time for the asker-context block. Empty when
	// the cached row has none yet or when Slack didn't return
	// one (rare; bot accounts).
	TZ string
	// TZLabel is the human-readable form (e.g. "Pacific Daylight
	// Time"). For display only — DST math uses TZ via
	// time.LoadLocation.
	TZLabel string
	// TZOffset is seconds east of UTC at fetch time. Informational;
	// the formatter prefers time.LoadLocation(TZ) for accuracy
	// across DST boundaries.
	TZOffset int
	// IsNegativeCache is true when this UserInfo came from a
	// negative-cache tombstone row — a slack_users row written when
	// Slack returned a definitive not-found (user_not_found /
	// user_disabled) and we cached the absence with all profile
	// fields empty. Derived at read time from the row shape (no
	// dedicated column); set only on the cache-read path in
	// lookupUserStruct. Lets callers distinguish "we know nothing
	// because the user is gone" from "a bot with an empty profile"
	// without re-deriving the all-empty-fields sentinel themselves.
	IsNegativeCache bool
}

// BotInfo is the persistent directory record for a classic Slack bot identity.
// BotID is the stable source key; Name is mutable display metadata.
type BotInfo struct {
	BotID   string
	Name    string
	AppID   string
	UserID  string
	Deleted bool
}

// IsExternal reports whether the user should be treated as external
// to our org. Three signals: Slack Connect users (different team_id
// = is_stranger=true), multi-channel guests (is_restricted), and
// single-channel guests (is_ultra_restricted). Any one is enough.
//
// A negative-cache UserInfo (IsNegativeCache) has every flag false, so
// this returns false for it: false means "not known to be external",
// never "internal". A caller deciding access checks IsNegativeCache
// first and refuses, as the channel Ask gate, the enable-Ask button and
// the direct-message path do. The channel member scan does not count
// such a user as an external member.
func (u UserInfo) IsExternal() bool {
	return u.IsStranger || u.IsRestricted || u.IsUltraRestricted
}

// SlackActorCanonical is the canonical-actor sentinel format for a
// slack user when no email is available. Stable across runs so the
// retrieve filter (matches actor_canonical = ANY OR actor = ANY)
// can match by user-id when the operator has no other key. Format:
// "slack:<user_id>". Used by the slack ingest path; agent_session
// rows pass actor through directly because their actor is already
// email-shaped.
//
// Returns empty string for a negative-cache UserInfo (the row exists
// in slack_users but with all profile fields empty — Slack returned
// user_not_found at fetch time, and we cached the absence). Without
// this guard we'd write 'slack:<user_id>' for users we explicitly
// know nothing about, which (a) breaks the events sink's NoChange
// path on subsequent re-ingests when the actor is later resolved
// (positive cache replaces 'slack:U' with the email, triggering
// avoidable re-embeds) and (b) leaves a stable but useless canonical
// key in the corpus.
func SlackActorCanonical(ui UserInfo) string {
	if ui.Email != "" {
		return ui.Email
	}
	// Negative-cache row: the canonical lookup hit a slack_users row
	// where Slack told us "user_not_found" and we wrote empty
	// strings as a tombstone. Treat that as no canonical key.
	if ui.IsNegativeCache {
		return ""
	}
	if ui.UserID != "" {
		return "slack:" + ui.UserID
	}
	return ""
}

// SlackBotActorCanonical returns the stable canonical sentinel for a
// Slack bot identity. Bot ids do not have email addresses and are a
// distinct Slack identity namespace from bot-user ids returned by
// users.info, so keep the distinction explicit in the flat legacy key.
func SlackBotActorCanonical(botID string) string {
	if botID == "" {
		return ""
	}
	return "slack-bot:" + botID
}

// LookupBot returns cached or freshly fetched metadata for a classic Slack bot
// id. bots.info is not a bulk-list endpoint, so this follows the same lazy,
// singleflight, seven-day cache pattern as users.info. A stale positive name is
// preserved across refresh failures; definitive misses are negative-cached.
func (d *Directory) LookupBot(ctx context.Context, botID string) (BotInfo, error) {
	result, err, _ := d.sf.Do("bot:"+botID, func() (any, error) {
		return d.lookupBotUncached(ctx, botID)
	})
	if err != nil {
		return BotInfo{}, err
	}
	return result.(BotInfo), nil
}

func (d *Directory) lookupBotUncached(ctx context.Context, botID string) (BotInfo, error) {
	if botID == "" {
		return BotInfo{}, errors.New("bot id is empty")
	}

	info := BotInfo{BotID: botID}
	var fetchedAt time.Time
	err := d.pool.QueryRow(ctx,
		`SELECT coalesce(name, ''), coalesce(app_id, ''), coalesce(user_id, ''),
		        deleted, fetched_at
		   FROM slack_bots WHERE bot_id = $1`, botID,
	).Scan(&info.Name, &info.AppID, &info.UserID, &info.Deleted, &fetchedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Fetch below.
	case err != nil:
		return BotInfo{}, fmt.Errorf("bot cache lookup: %w", err)
	case time.Since(fetchedAt) < directoryTTL:
		// A successful bots.info response may legitimately have no name.
		// The row still proves the lookup completed; serving it avoids
		// repeatedly consuming the Tier-3 bots.info budget. Migration seeds
		// only named rows, so a fresh empty row is either this positive shape
		// or an explicit deleted/not-found tombstone.
		return info, nil
	}

	api, ok := d.api.(slackBotAPI)
	if !ok {
		return BotInfo{}, errors.New("slack API does not support bots.info")
	}
	var bot *slack.Bot
	fetchErr := d.limiterCall(ctx, methodBotInfo, func() error {
		var apiErr error
		bot, apiErr = api.GetBotInfoContext(ctx, slack.GetBotInfoParameters{Bot: botID})
		return apiErr
	})
	if fetchErr != nil {
		if !fetchedAt.IsZero() && info.Name != "" {
			if isDefinitiveNotFound(fetchErr) {
				info.Deleted = true
				_, _ = d.pool.Exec(ctx,
					`UPDATE slack_bots SET deleted = true, fetched_at = now() WHERE bot_id = $1`,
					botID)
			}
			return info, nil
		}
		if isDefinitiveNotFound(fetchErr) {
			_, _ = d.pool.Exec(ctx,
				`INSERT INTO slack_bots (bot_id, name, app_id, user_id, deleted, fetched_at)
				 VALUES ($1, '', '', '', true, now())
				 ON CONFLICT (bot_id) DO UPDATE SET deleted = true, fetched_at = now()`,
				botID)
		}
		return BotInfo{}, fetchErr
	}
	if bot == nil {
		return BotInfo{}, errors.New("bots.info returned no bot")
	}

	info = BotInfo{
		BotID:   botID,
		Name:    bot.Name,
		AppID:   bot.AppID,
		UserID:  bot.UserID,
		Deleted: bot.Deleted,
	}
	_, _ = d.pool.Exec(ctx,
		`INSERT INTO slack_bots (bot_id, name, app_id, user_id, deleted, fetched_at)
		 VALUES ($1, $2, $3, $4, $5, now())
		 ON CONFLICT (bot_id) DO UPDATE SET
		   name = EXCLUDED.name,
		   app_id = EXCLUDED.app_id,
		   user_id = EXCLUDED.user_id,
		   deleted = EXCLUDED.deleted,
		   fetched_at = now()`,
		info.BotID, info.Name, info.AppID, info.UserID, info.Deleted)
	return info, nil
}

// LookupUserInfo returns the cached or freshly-fetched user info
// row. Uses the shorter externalFlagsTTL because the external
// flags it surfaces gate ask-routing decisions and need to
// converge faster than rename resolution.
func (d *Directory) LookupUserInfo(ctx context.Context, userID string) (UserInfo, error) {
	result, err, _ := d.sf.Do("userinfo:"+userID, func() (any, error) {
		return d.lookupUserStruct(ctx, userID, externalFlagsTTL, true)
	})
	if err != nil {
		return UserInfo{}, err
	}
	return result.(UserInfo), nil
}

// LookupUserInfoStrict is the authorization-grade profile lookup used by
// direct messages. It accepts a cached Slack profile for at most the same
// five-minute revocation window as private-channel grants, then requires a
// successful users.info refresh. Unlike cosmetic and channel-gate lookups, a
// refresh failure never reuses stale identity or external-membership flags.
// A separate singleflight key prevents a concurrent permissive lookup from
// supplying its stale-fallback result to this strict caller.
func (d *Directory) LookupUserInfoStrict(ctx context.Context, userID string) (UserInfo, error) {
	result, err, _ := d.sf.Do("userinfo-strict:"+userID, func() (any, error) {
		return d.lookupUserStruct(ctx, userID, userScopeTTL, false)
	})
	if err != nil {
		return UserInfo{}, err
	}
	return result.(UserInfo), nil
}

// lookupUserStruct is the cache+fetch path shared by LookupUser and
// LookupUserInfo. ttl is how stale a cached row can be before we
// re-fetch from Slack — callers pass externalFlagsTTL when the
// freshness of the auth flags matters (LookupUserInfo gating ask
// routing) or directoryTTL when only the resolved name matters.
// allowStale controls the refresh-error posture. Cosmetic/channel callers may
// retain a stale positive row; authorization callers set it false and fail
// closed after their TTL. Returns the full row; LookupUser projects to a name.
func (d *Directory) lookupUserStruct(ctx context.Context, userID string, ttl time.Duration, allowStale bool) (UserInfo, error) {
	var (
		ui        UserInfo
		fetchedAt time.Time
	)
	ui.UserID = userID
	err := d.pool.QueryRow(ctx,
		`SELECT coalesce(name, ''), coalesce(display_name, ''),
		        coalesce(real_name, ''), coalesce(email, ''),
		        coalesce(team_id, ''), is_bot,
		        is_stranger, is_restricted, is_ultra_restricted,
		        coalesce(tz, ''), coalesce(tz_label, ''),
		        coalesce(tz_offset, 0),
		        fetched_at
		 FROM slack_users WHERE user_id = $1`, userID,
	).Scan(&ui.Name, &ui.DisplayName, &ui.RealName, &ui.Email,
		&ui.TeamID, &ui.IsBot,
		&ui.IsStranger, &ui.IsRestricted, &ui.IsUltraRestricted,
		&ui.TZ, &ui.TZLabel, &ui.TZOffset,
		&fetchedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to fetch
	case err != nil:
		return UserInfo{}, fmt.Errorf("cache lookup: %w", err)
	default:
		// A cached row with all profile names empty AND is_bot=false
		// is a negative-cache tombstone (the tombstone INSERT below
		// always writes is_bot=false). Stamp the flag here, on the
		// read path, so callers don't re-derive the sentinel. The
		// !IsBot guard disambiguates a cached profile-less bot (real
		// row, is_bot=true from the API) from a tombstone — without
		// it a profile-less bot read from cache would be treated as
		// a tombstone and lose its slack:<id> canonical key in
		// SlackActorCanonical.
		ui.IsNegativeCache = ui.Name == "" && ui.DisplayName == "" && ui.RealName == "" && !ui.IsBot
		if time.Since(fetchedAt) < ttl {
			return ui, nil
		}
		// Stale — re-fetch below. If re-fetch fails, we'll fall
		// back to the stale cache rather than returning empty.
	}

	var u *slack.User
	fetchErr := d.limiterCall(ctx, methodUserInfo, func() error {
		var err error
		u, err = d.api.GetUserInfoContext(ctx, userID)
		return err
	})
	if fetchErr != nil {
		// We have a stale positive cache row to fall back on when the
		// row existed (fetchedAt set — zero only on the ErrNoRows miss
		// path) and it isn't a negative-cache tombstone. Using the
		// IsNegativeCache flag rather than a name-emptiness check is
		// what keeps a cached profile-less bot (empty names, is_bot=
		// true) on the fallback path: a name-based check would treat
		// it as "no usable cache" and bubble the transient error up /
		// skip the deactivation flag-clear.
		if allowStale && !fetchedAt.IsZero() && !ui.IsNegativeCache {
			if isDefinitiveNotFound(fetchErr) {
				// Definitive not-found AND we have a stale
				// positive cache. The user has been deactivated
				// since we last saw them. Clear the auth flags
				// (a deactivated user can't be a current
				// external-member-of-channel for gate purposes)
				// and keep the name for cosmetic resolution.
				// Persist the cleared flags so future scans
				// don't re-do this dance.
				ui.IsStranger = false
				ui.IsRestricted = false
				ui.IsUltraRestricted = false
				_, _ = d.pool.Exec(ctx,
					`UPDATE slack_users SET
					   is_stranger=false,
					   is_restricted=false,
					   is_ultra_restricted=false,
					   fetched_at=now()
					 WHERE user_id=$1`,
					userID)
			}
			return ui, nil
		}
		if isDefinitiveNotFound(fetchErr) {
			_, _ = d.pool.Exec(ctx,
				`INSERT INTO slack_users (user_id, name, display_name, real_name, email, is_bot, team_id, is_stranger, is_restricted, is_ultra_restricted, fetched_at)
				 VALUES ($1, '', '', '', '', false, '', false, false, false, now())
				 ON CONFLICT (user_id) DO UPDATE SET fetched_at=now()`,
				userID)
		}
		return UserInfo{}, fetchErr
	}

	ui.Name = u.Name
	ui.DisplayName = u.Profile.DisplayName
	ui.RealName = u.RealName
	ui.Email = u.Profile.Email
	ui.TeamID = u.TeamID
	ui.IsBot = u.IsBot
	ui.IsStranger = u.IsStranger
	ui.IsRestricted = u.IsRestricted
	ui.IsUltraRestricted = u.IsUltraRestricted
	ui.TZ = u.TZ
	ui.TZLabel = u.TZLabel
	ui.TZOffset = u.TZOffset
	// Fresh API result supersedes any stale tombstone we may have
	// read above: a user that now resolves is not a negative-cache
	// hit. (Slack can still hand back a profile-less bot here; that
	// is correctly NOT a negative-cache row.)
	ui.IsNegativeCache = false
	if _, err := d.pool.Exec(ctx,
		`INSERT INTO slack_users
		     (user_id, name, display_name, real_name, email, is_bot,
		      team_id, is_stranger, is_restricted, is_ultra_restricted,
		      tz, tz_label, tz_offset, fetched_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now())
		 ON CONFLICT (user_id) DO UPDATE SET
		   name=EXCLUDED.name,
		   display_name=EXCLUDED.display_name,
		   real_name=EXCLUDED.real_name,
		   email=EXCLUDED.email,
		   is_bot=EXCLUDED.is_bot,
		   team_id=EXCLUDED.team_id,
		   is_stranger=EXCLUDED.is_stranger,
		   is_restricted=EXCLUDED.is_restricted,
		   is_ultra_restricted=EXCLUDED.is_ultra_restricted,
		   tz=EXCLUDED.tz,
		   tz_label=EXCLUDED.tz_label,
		   tz_offset=EXCLUDED.tz_offset,
		   fetched_at=now()`,
		userID, u.Name, u.Profile.DisplayName, u.RealName,
		u.Profile.Email, u.IsBot, u.TeamID,
		u.IsStranger, u.IsRestricted, u.IsUltraRestricted,
		u.TZ, u.TZLabel, u.TZOffset,
	); err != nil {
		// Cache write failure is non-fatal — we still have the
		// resolved info this round, and next lookup will retry.
		return ui, nil
	}
	return ui, nil
}

func pickName(displayName, name string) string {
	if displayName != "" {
		return displayName
	}
	return name
}

// isDefinitiveNotFound reports whether a Slack API error is the kind
// that will stay true for a while ("user_not_found",
// "channel_not_found") vs a transient failure (network, 5xx, rate
// limit). Only the former should populate the negative cache —
// locking in a negative entry on a transient blip would make the
// entity unresolvable for the full TTL window (7 days).
//
// slack-go returns SlackErrorResponse for API-surfaced errors with
// a specific Err code. Network/5xx/rate-limit errors surface as
// other types (url.Error, slack.RateLimitedError) which we treat
// as "try again later."
func isDefinitiveNotFound(err error) bool {
	// slack-go historically returned both value and pointer
	// forms of SlackErrorResponse depending on the code path.
	// errors.As handles the direct value match; we also check
	// the pointer form explicitly for older call sites.
	var sErr slack.SlackErrorResponse
	if errors.As(err, &sErr) {
		return isNotFoundCode(sErr.Err)
	}
	var pErr *slack.SlackErrorResponse
	if errors.As(err, &pErr) && pErr != nil {
		return isNotFoundCode(pErr.Err)
	}
	return false
}

func isNotFoundCode(code string) bool {
	switch code {
	case "user_not_found", "channel_not_found", "bot_not_found", "user_disabled":
		return true
	}
	return false
}

// ---- channel resolution ----

// channelRef matches <#Cxxx|name> or <#Cxxx>. Slack includes the
// name when it has one to spare the client a lookup; we use it as
// a cache-miss fallback but still cache the canonical mapping.
var channelRef = regexp.MustCompile(`<#(C[A-Z0-9]+)(?:\|([^<>\n]*))?>`)

func (d *Directory) resolveChannels(ctx context.Context, text string) string {
	return channelRef.ReplaceAllStringFunc(text, func(match string) string {
		m := channelRef.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		channelID := m[1]
		inlineName := ""
		if len(m) >= 3 {
			inlineName = m[2]
		}
		name, err := d.LookupChannel(ctx, channelID)
		if err != nil || name == "" {
			if inlineName != "" {
				return "#" + inlineName
			}
			return "#" + channelID
		}
		return "#" + name
	})
}

// IsBotInChannel returns whether the bot is currently a member of
// channelID. Always hits conversations.info live (not cached) —
// membership changes between calls in ways the cached name doesn't,
// and the staleness window has to be tighter for membership checks
// (e.g. operator-driven backfill validation) than for name
// resolution. Returns ErrChannelNotFound if Slack doesn't know the
// channel.
func (d *Directory) IsBotInChannel(ctx context.Context, channelID string) (bool, error) {
	var info *slack.Channel
	err := d.limiterCall(ctx, methodConvInfo, func() error {
		var fetchErr error
		info, fetchErr = d.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
			ChannelID: channelID,
		})
		return fetchErr
	})
	if err != nil {
		if isDefinitiveNotFound(err) {
			return false, ErrChannelNotFound
		}
		return false, err
	}
	return info.IsMember, nil
}

// LookupChannel returns a channel's name from the cache or, on
// miss, from Slack's conversations.info.
func (d *Directory) LookupChannel(ctx context.Context, channelID string) (string, error) {
	result, err, _ := d.sf.Do("channel:"+channelID, func() (any, error) {
		return d.lookupChannelUncached(ctx, channelID)
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

func (d *Directory) lookupChannelUncached(ctx context.Context, channelID string) (string, error) {
	var name string
	var fetchedAt time.Time
	err := d.pool.QueryRow(ctx,
		`SELECT coalesce(name, ''), fetched_at FROM slack_channel_info WHERE channel_id = $1`,
		channelID,
	).Scan(&name, &fetchedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// fetch below
	case err != nil:
		return "", fmt.Errorf("cache lookup: %w", err)
	default:
		if time.Since(fetchedAt) < directoryTTL {
			return name, nil
		}
	}

	info, fetchErr := d.refreshChannel(ctx, channelID, false)
	if fetchErr != nil {
		if info != nil {
			return info.Name, nil
		}
		if name != "" {
			// Stale positive + definitive not-found: the
			// channel was renamed/deleted. Bump fetched_at so
			// we don't re-hit the API on every mention, but
			// keep returning the stale name rather than
			// nothing. Transient errors leave fetched_at
			// alone so the TTL keeps expiring.
			if isDefinitiveNotFound(fetchErr) {
				_, _ = d.pool.Exec(ctx,
					`UPDATE slack_channel_info SET fetched_at=now() WHERE channel_id=$1`,
					channelID)
			}
			return name, nil
		}
		// First-time miss. Negative cache only for definitive
		// not-found — transient failures must NOT get cached
		// for 7 days.
		if isDefinitiveNotFound(fetchErr) {
			_, _ = d.pool.Exec(ctx,
				`INSERT INTO slack_channel_info (channel_id, name, is_private, is_archived, fetched_at)
				 VALUES ($1, '', false, false, now())
				 ON CONFLICT (channel_id) DO UPDATE SET fetched_at=now()`,
				channelID)
		}
		return "", fetchErr
	}
	return info.Name, nil
}

// ChannelIsPrivate reports whether channelID is a private channel.
// Cache-first via slack_channel_info with the standard directory
// TTL; on miss or stale, LookupChannel fetches conversations.info
// (which upserts is_private) and the row is re-read.
//
// Used by the ingest paths to stamp events.scope: private channels
// scope to "slack-channel:<id>", workspace-public to "internal". An error here means
// "privacy unknown" — callers stamp events.ScopeUnstamped (deny by
// default) rather than guessing. A negative-cache row (empty name:
// Slack returned a definitive not-found for the channel) is
// likewise treated as unknown, NOT as public — is_private=false on
// those rows is the column default, not information.
func (d *Directory) ChannelIsPrivate(ctx context.Context, channelID string) (bool, error) {
	var (
		isPrivate bool
		name      string
		fetchedAt time.Time
	)
	err := d.pool.QueryRow(ctx,
		`SELECT is_private, coalesce(name, ''), fetched_at
		   FROM slack_channel_info WHERE channel_id = $1`,
		channelID,
	).Scan(&isPrivate, &name, &fetchedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to fetch
	case err != nil:
		return false, fmt.Errorf("channel privacy cache lookup: %w", err)
	default:
		if name != "" && time.Since(fetchedAt) < directoryTTL {
			return isPrivate, nil
		}
		// Stale or negative-cache row: refresh below.
	}

	// LookupChannel owns the fetch path (singleflight, limiter,
	// negative caching) and upserts is_private as a side effect;
	// re-read the row afterwards rather than duplicating that
	// machinery here.
	if _, err := d.LookupChannel(ctx, channelID); err != nil {
		return false, fmt.Errorf("channel privacy fetch: %w", err)
	}
	err = d.pool.QueryRow(ctx,
		`SELECT is_private, coalesce(name, '')
		   FROM slack_channel_info WHERE channel_id = $1`,
		channelID,
	).Scan(&isPrivate, &name)
	if err != nil {
		return false, fmt.Errorf("channel privacy re-read: %w", err)
	}
	if name == "" {
		return false, fmt.Errorf("channel %s privacy unknown (negative-cache info row)", channelID)
	}
	return isPrivate, nil
}

// LookupChannelByName resolves a channel name (without the leading
// "#") to its channel_id. Cache-first via the slack_channel_info
// table; on miss or stale cache, walks conversations.list with
// paging until either the channel is found or the workspace is
// exhausted.
//
// Naming caveat: Slack channel names are unique at any given moment
// but not historically — a channel can be renamed and a new channel
// can take the old name. The cache index (idx_slack_channel_info_name,
// non-unique on purpose) preserves rename history. We resolve to the
// freshest non-empty row to break ties; that's a "current channel"
// heuristic that works for the operator's expected usage ("invite me
// to #foo, the channel called #foo right now").
//
// Returns ErrChannelNotFound when no channel matches after a full
// conversations.list scan. Other errors (auth, network) propagate.
func (d *Directory) LookupChannelByName(ctx context.Context, name string) (string, error) {
	name = strings.TrimPrefix(name, "#")
	if name == "" {
		return "", errors.New("LookupChannelByName: empty name")
	}
	result, err, _ := d.sf.Do("channelByName:"+name, func() (any, error) {
		return d.lookupChannelByNameUncached(ctx, name)
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

// ErrChannelNotFound is returned when LookupChannelByName exhausts
// conversations.list without a match. Distinguishable from API
// errors so callers can render a clean "no such channel" message
// to the operator.
var ErrChannelNotFound = errors.New("channel not found")

func (d *Directory) lookupChannelByNameUncached(ctx context.Context, name string) (string, error) {
	var channelID string
	var fetchedAt time.Time
	// Cache-first. ORDER BY fetched_at DESC + LIMIT 1 picks the
	// most-recently-seen row when multiple historical rows share
	// the same name (rename history); LIMIT 1 keeps the path
	// O(log n) on the index.
	err := d.pool.QueryRow(ctx,
		`SELECT channel_id, fetched_at FROM slack_channel_info
		  WHERE name = $1 ORDER BY fetched_at DESC LIMIT 1`,
		name,
	).Scan(&channelID, &fetchedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// fetch below
	case err != nil:
		return "", fmt.Errorf("cache lookup: %w", err)
	default:
		if time.Since(fetchedAt) < directoryTTL {
			return channelID, nil
		}
		// Stale — walk the workspace below to refresh + verify
		// the name still maps to this id. If the workspace walk
		// fails we'll return the stale id rather than nothing.
	}

	// conversations.list, paged. Type filter includes both public
	// and private channels; the user/bot token determines what's
	// visible (the bot token sees public + private channels it's
	// in; a user token sees everything that user can see).
	var observedAt time.Time
	if err := d.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&observedAt); err != nil {
		return "", err
	}
	cursor := ""
	for {
		params := &slack.GetConversationsParameters{
			Cursor:          cursor,
			Limit:           1000, // Slack's max
			Types:           []string{"public_channel", "private_channel"},
			ExcludeArchived: true,
		}
		var channels []slack.Channel
		var nextCursor string
		err := d.limiterCall(ctx, methodConvList, func() error {
			var apiErr error
			channels, nextCursor, apiErr = d.api.GetConversationsContext(ctx, params)
			return apiErr
		})
		if err != nil {
			if channelID != "" {
				// Stale-cache fallback: we had a hit but the
				// refresh walk failed. Return the stale id.
				return channelID, nil
			}
			return "", fmt.Errorf("conversations.list: %w", err)
		}
		for _, ch := range channels {
			// Refresh-on-walk: stamp every channel we see into
			// the cache. Cheap (one row per channel, ~hundreds of
			// rows for a typical workspace), turns this name
			// lookup into a workspace-wide cache warm at the
			// same time.
			_ = saveChannelInfo(ctx, d.pool, &ch, observedAt)
			if ch.Name == name {
				return ch.ID, nil
			}
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}
	if channelID != "" {
		// Stale cache hit but the channel isn't in the current
		// workspace listing. Probably renamed or archived. Return
		// the stale id with a warning-shaped error so the caller
		// can decide.
		return channelID, ErrChannelNotFound
	}
	return "", ErrChannelNotFound
}

// ---- usergroup resolution ----

// Usergroup refs look like <!subteam^S0EXAMPLE01> or
// <!subteam^S0EXAMPLE01|netops>. The inline name (if present) is
// Slack's hint to clients; we prefer it on cache miss.
var usergroupRef = regexp.MustCompile(`<!subteam\^(S[A-Z0-9]+)(?:\|([^<>\n]*))?>`)

func (d *Directory) resolveUsergroups(ctx context.Context, text string) string {
	return usergroupRef.ReplaceAllStringFunc(text, func(match string) string {
		m := usergroupRef.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		groupID := m[1]
		inlineName := ""
		if len(m) >= 3 {
			inlineName = m[2]
		}
		name, err := d.LookupUsergroup(ctx, groupID)
		if err != nil || name == "" {
			if inlineName != "" {
				return "@" + inlineName
			}
			return "@" + groupID
		}
		return "@" + name
	})
}

// LookupUsergroup uses usergroups.list since Slack doesn't expose a
// single-id endpoint. Paginates the list once, caches all groups,
// and returns the one we asked for. On miss we re-pull; on stale
// TTL we re-pull too. The full-list nature means the first lookup
// costs one API call regardless of how many groups we query.
func (d *Directory) LookupUsergroup(ctx context.Context, groupID string) (string, error) {
	result, err, _ := d.sf.Do("group:"+groupID, func() (any, error) {
		return d.lookupUsergroupUncached(ctx, groupID)
	})
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

func (d *Directory) lookupUsergroupUncached(ctx context.Context, groupID string) (string, error) {
	var handle, name string
	var fetchedAt time.Time
	err := d.pool.QueryRow(ctx,
		`SELECT coalesce(handle, ''), coalesce(name, ''), fetched_at
		 FROM slack_usergroups WHERE usergroup_id = $1`, groupID,
	).Scan(&handle, &name, &fetchedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// fetch below
	case err != nil:
		return "", fmt.Errorf("cache lookup: %w", err)
	default:
		if time.Since(fetchedAt) < directoryTTL {
			return pickName(handle, name), nil
		}
	}

	// usergroups.list has no pagination; Slack returns all groups
	// on the workspace. We cache them all to amortize future
	// lookups.
	var groups []slack.UserGroup
	fetchErr := d.limiterCall(ctx, methodUserGroupsList, func() error {
		var err error
		groups, err = d.api.GetUserGroupsContext(ctx,
			slack.GetUserGroupsOptionIncludeDisabled(true))
		return err
	})
	if fetchErr != nil {
		if handle != "" || name != "" {
			return pickName(handle, name), nil
		}
		return "", fetchErr
	}
	found := ""
	idsInResult := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		idsInResult[g.ID] = struct{}{}
		// Resolve-on-the-fly first — cache write failure must
		// not prevent us from returning a correct answer for
		// the group the caller actually asked about.
		if g.ID == groupID {
			found = pickName(g.Handle, g.Name)
		}
		if _, err := d.pool.Exec(ctx,
			`INSERT INTO slack_usergroups (usergroup_id, name, handle, fetched_at)
			 VALUES ($1, $2, $3, now())
			 ON CONFLICT (usergroup_id) DO UPDATE SET
			   name=EXCLUDED.name, handle=EXCLUDED.handle, fetched_at=now()`,
			g.ID, g.Name, g.Handle); err != nil {
			// Cache write failed; next lookup will retry. No
			// point aborting the sweep.
			continue
		}
	}
	// Negative cache: if the requested groupID wasn't in the
	// list-response, Slack doesn't know about it — probably a
	// deleted group. Write a stub entry so we don't re-fetch the
	// entire usergroups list on every future mention.
	if _, ok := idsInResult[groupID]; !ok {
		_, _ = d.pool.Exec(ctx,
			`INSERT INTO slack_usergroups (usergroup_id, name, handle, fetched_at)
			 VALUES ($1, '', '', now())
			 ON CONFLICT (usergroup_id) DO UPDATE SET fetched_at=now()`,
			groupID)
	}
	return found, nil
}

// ---- url resolution ----

// urlRef matches <url|anchor> and bare <url>. Slack wraps every URL
// in angle brackets even when there's no anchor, so naive text
// renderers show them as "<https://...>". Strip the markup: keep
// the anchor when present, fall back to the URL.
var urlRef = regexp.MustCompile(`<(https?://[^|>\s]+)(?:\|([^<>\n]*))?>`)

func resolveURLs(text string) string {
	return urlRef.ReplaceAllStringFunc(text, func(match string) string {
		m := urlRef.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		url := m[1]
		anchor := ""
		if len(m) >= 3 {
			anchor = strings.TrimSpace(m[2])
		}
		if anchor != "" && anchor != url {
			return fmt.Sprintf("%s (%s)", anchor, url)
		}
		return url
	})
}

// ---- single-flight ----
//
// Small hand-rolled version (no go.mod add for x/sync) so repeated
// mentions of the same user/channel inside one message batch don't
// fire N identical API calls. Keys are scoped ("user:Uxxx") to
// avoid collisions across resolver types.

type singleflight struct {
	mu    sync.Mutex
	calls map[string]*sfCall
}

type sfCall struct {
	done chan struct{}
	val  any
	err  error
}

func newSingleflight() singleflight {
	return singleflight{calls: make(map[string]*sfCall)}
}

func (s *singleflight) Do(key string, fn func() (any, error)) (any, error, bool) {
	s.mu.Lock()
	if c, ok := s.calls[key]; ok {
		s.mu.Unlock()
		<-c.done
		return c.val, c.err, true
	}
	c := &sfCall{done: make(chan struct{})}
	s.calls[key] = c
	s.mu.Unlock()

	// Cleanup via defer so a panic in fn() doesn't leave the call
	// record in an unfinished state. Recover the panic, record a
	// synthetic error on the call so waiters don't see (nil, nil)
	// and type-assert-panic themselves, then re-panic in the
	// caller's goroutine once the defers have finished.
	var panicVal any
	defer func() {
		if panicVal != nil {
			c.err = fmt.Errorf("panic in singleflight fn: %v", panicVal)
		}
		close(c.done)
		s.mu.Lock()
		delete(s.calls, key)
		s.mu.Unlock()
		if panicVal != nil {
			panic(panicVal)
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			panicVal = r
		}
	}()

	c.val, c.err = fn()
	return c.val, c.err, false
}
