package slack

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"
)

func getChannelConfigPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE slack_channel_config, slack_users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// fakeMembersAPI implements slackAPIMembers for tests. Returns a
// fixed slice of user IDs; ignores cursor/limit (tests stay
// single-page).
type fakeMembersAPI struct {
	mu       sync.Mutex
	members  map[string][]string // channelID -> user IDs
	calls    int
	failOnce bool
}

func (f *fakeMembersAPI) GetUsersInConversationContext(_ context.Context, p *membersParams) ([]string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failOnce {
		f.failOnce = false
		return nil, "", errors.New("transient slack error")
	}
	return f.members[p.ChannelID], "", nil
}

// fakeUserAPI lets the directory resolve external flags. Implements
// slackAPI.
type fakeUserAPI struct {
	users map[string]*slack.User
}

func (f *fakeUserAPI) GetUserInfoContext(_ context.Context, id string, _ ...slack.GetUserInfoOption) (*slack.User, error) {
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	return nil, &slack.SlackErrorResponse{Err: "user_not_found"}
}

func (f *fakeUserAPI) GetConversationInfoContext(_ context.Context, _ *slack.GetConversationInfoInput) (*slack.Channel, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeUserAPI) GetUserGroupsContext(_ context.Context, _ ...slack.GetUserGroupsOption) ([]slack.UserGroup, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeUserAPI) GetConversationsContext(_ context.Context, _ *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	return nil, "", errors.New("not implemented")
}

func newTestStore(t *testing.T, pool *pgxpool.Pool, members map[string][]string, users map[string]*slack.User) *ChannelConfigStore {
	t.Helper()
	dir := newDirectoryWithAPI(pool, &fakeUserAPI{users: users})
	return NewChannelConfigStore(pool, dir,
		&fakeMembersAPI{members: members},
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
}

func TestChannelConfig_GetMissingReturnsZeroValue(t *testing.T) {
	pool := getChannelConfigPool(t)
	store := newTestStore(t, pool, nil, nil)
	cfg, err := store.Get(context.Background(), "C_UNKNOWN")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ChannelID != "C_UNKNOWN" {
		t.Errorf("ChannelID = %q", cfg.ChannelID)
	}
	if cfg.AllowExternalAsk {
		t.Error("default should be allow_external_ask=false")
	}
	if !cfg.IsExternalCheckStale() {
		t.Error("never-checked config should be stale")
	}
}

func TestChannelConfig_SetAllowExternalAskRoundTrip(t *testing.T) {
	pool := getChannelConfigPool(t)
	store := newTestStore(t, pool, nil, nil)
	ctx := context.Background()
	if err := store.SetAllowExternalAsk(ctx, "C_OPS", true, "avery@example.com"); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.Get(ctx, "C_OPS")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowExternalAsk {
		t.Error("expected allow_external_ask=true after Set")
	}
	if cfg.UpdatedBy != "avery@example.com" {
		t.Errorf("UpdatedBy = %q", cfg.UpdatedBy)
	}
	// Flip back.
	if err := store.SetAllowExternalAsk(ctx, "C_OPS", false, "avery@example.com"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = store.Get(ctx, "C_OPS")
	if cfg.AllowExternalAsk {
		t.Error("expected allow_external_ask=false after second Set")
	}
}

func TestChannelConfig_EnsureExternalMembersFresh_ScansOnce(t *testing.T) {
	pool := getChannelConfigPool(t)
	users := map[string]*slack.User{
		"U_AVERY":  {ID: "U_AVERY", Name: "avery"},
		"U_VENDOR": {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
	}
	members := map[string][]string{
		"C_MIXED": {"U_AVERY", "U_VENDOR"},
	}
	store := newTestStore(t, pool, members, users)
	ctx := context.Background()

	cfg, err := store.EnsureExternalMembersFresh(ctx, "C_MIXED")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasExternalMembers {
		t.Error("expected HasExternalMembers=true (vendor is_stranger)")
	}
	if cfg.IsExternalCheckStale() {
		t.Error("expected fresh after first scan")
	}

	// Second call within TTL should NOT trigger another scan.
	preCalls := store.api.(*fakeMembersAPI).calls
	cfg2, err := store.EnsureExternalMembersFresh(ctx, "C_MIXED")
	if err != nil {
		t.Fatal(err)
	}
	postCalls := store.api.(*fakeMembersAPI).calls
	if postCalls != preCalls {
		t.Errorf("second EnsureExternalMembersFresh hit slack API again: %d->%d calls", preCalls, postCalls)
	}
	if !cfg2.HasExternalMembers {
		t.Error("cached result should still be true")
	}
}

func TestChannelConfig_EnsureExternalMembersFresh_AllInternal(t *testing.T) {
	pool := getChannelConfigPool(t)
	users := map[string]*slack.User{
		"U_AVERY": {ID: "U_AVERY", Name: "avery"},
		"U_BOB":   {ID: "U_BOB", Name: "bob"},
	}
	members := map[string][]string{
		"C_INTERNAL": {"U_AVERY", "U_BOB"},
	}
	store := newTestStore(t, pool, members, users)
	cfg, err := store.EnsureExternalMembersFresh(context.Background(), "C_INTERNAL")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasExternalMembers {
		t.Error("all-internal channel should report HasExternalMembers=false")
	}
}

func TestChannelConfig_EnsureExternalMembersFresh_StaleRefreshes(t *testing.T) {
	pool := getChannelConfigPool(t)
	users := map[string]*slack.User{
		"U_AVERY": {ID: "U_AVERY", Name: "avery"},
	}
	members := map[string][]string{"C": {"U_AVERY"}}
	store := newTestStore(t, pool, members, users)
	ctx := context.Background()

	// Pre-seed a stale row.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, has_external_members, external_members_checked_at, updated_at)
		VALUES ('C', true, $1, now())
	`, time.Now().Add(-externalMembersTTL-time.Hour)); err != nil {
		t.Fatal(err)
	}

	cfg, err := store.EnsureExternalMembersFresh(ctx, "C")
	if err != nil {
		t.Fatal(err)
	}
	// Scan should have run (stale → refresh) and overwritten true→false.
	if cfg.HasExternalMembers {
		t.Error("stale row should have been refreshed; expected false")
	}
}

func TestChannelConfig_EnsureExternalMembersFresh_ScanErrorReturnsStale(t *testing.T) {
	pool := getChannelConfigPool(t)
	store := newTestStore(t, pool, nil, nil)
	store.api.(*fakeMembersAPI).failOnce = true
	ctx := context.Background()
	cfg, err := store.EnsureExternalMembersFresh(ctx, "C_MISSING")
	if err == nil {
		t.Fatal("expected error from scan failure, got nil")
	}
	// Caller gets the (zero-valued) cfg + err. Default-deny posture
	// is the caller's choice — verify we DID hand back the zero
	// cfg (channel id set, everything else default).
	if cfg.ChannelID != "C_MISSING" {
		t.Errorf("ChannelID = %q", cfg.ChannelID)
	}
	if cfg.AllowExternalAsk {
		t.Error("error path should return default-deny cfg")
	}
}

// TestChannelConfig_ScanRecordsExternalMembers: the scan persists
// WHO is external, not just whether anyone is — the gate ephemeral
// names them.
func TestChannelConfig_ScanRecordsExternalMembers(t *testing.T) {
	pool := getChannelConfigPool(t)
	users := map[string]*slack.User{
		"U_AVERY":   {ID: "U_AVERY", Name: "avery"},
		"U_VENDOR":  {ID: "U_VENDOR", Name: "vendor", IsStranger: true},
		"U_VENDOR2": {ID: "U_VENDOR2", Name: "vendor2", IsRestricted: true},
	}
	members := map[string][]string{
		"C_MIXED": {"U_AVERY", "U_VENDOR", "U_VENDOR2"},
	}
	store := newTestStore(t, pool, members, users)
	cfg, err := store.EnsureExternalMembersFresh(context.Background(), "C_MIXED")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasExternalMembers {
		t.Error("expected HasExternalMembers=true")
	}
	// No short-circuit: BOTH externals are recorded.
	if len(cfg.ExternalMembers) != 2 {
		t.Fatalf("ExternalMembers = %v, want both vendors", cfg.ExternalMembers)
	}
}

// TestChannelConfig_ConfirmRescansStalePositive: ConfirmExternalMembers
// ignores the 6h TTL and re-scans when the cached positive is older
// than the confirm bound — the "kick the guest, re-ask" path.
func TestChannelConfig_ConfirmRescansStalePositive(t *testing.T) {
	pool := getChannelConfigPool(t)
	users := map[string]*slack.User{
		"U_AVERY": {ID: "U_AVERY", Name: "avery"},
	}
	// Channel membership is NOW all-internal (the guest was kicked)…
	members := map[string][]string{"C": {"U_AVERY"}}
	store := newTestStore(t, pool, members, users)
	ctx := context.Background()

	// …but the cached row still says external, scanned 5 minutes
	// ago — fresh by the 6h TTL, stale by the confirm bound.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, has_external_members, external_members, external_members_checked_at, updated_at)
		VALUES ('C', true, '{U_GONE}', $1, now())
	`, time.Now().Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// The TTL path trusts the cache (no rescan)…
	cfg, err := store.EnsureExternalMembersFresh(ctx, "C")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasExternalMembers {
		t.Fatal("TTL path should still return the cached positive")
	}
	// …the confirm path re-scans and clears it.
	cfg, err = store.ConfirmExternalMembers(ctx, "C")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasExternalMembers {
		t.Error("confirm rescan should have cleared the stale positive")
	}
	if len(cfg.ExternalMembers) != 0 {
		t.Errorf("ExternalMembers = %v, want empty after clean rescan", cfg.ExternalMembers)
	}
}

// TestChannelConfig_ConfirmTrustsVeryFreshScan: a positive scanned
// within the confirm bound is NOT re-scanned — a burst of mentions
// in a genuinely-blocked channel costs one scan, not N.
func TestChannelConfig_ConfirmTrustsVeryFreshScan(t *testing.T) {
	pool := getChannelConfigPool(t)
	store := newTestStore(t, pool, map[string][]string{"C": {"U_AVERY"}}, map[string]*slack.User{
		"U_AVERY": {ID: "U_AVERY", Name: "avery"},
	})
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, has_external_members, external_members, external_members_checked_at, updated_at)
		VALUES ('C', true, '{U_VENDOR}', now(), now())
	`); err != nil {
		t.Fatal(err)
	}
	preCalls := store.api.(*fakeMembersAPI).calls
	cfg, err := store.ConfirmExternalMembers(ctx, "C")
	if err != nil {
		t.Fatal(err)
	}
	if store.api.(*fakeMembersAPI).calls != preCalls {
		t.Error("confirm within the bound should not re-scan")
	}
	if !cfg.HasExternalMembers {
		t.Error("very-fresh positive should be trusted")
	}
}

// TestChannelConfig_ExpireExternalCheckForcesRescan: the membership-
// event cache bust makes the next TTL-path check re-scan.
func TestChannelConfig_ExpireExternalCheckForcesRescan(t *testing.T) {
	pool := getChannelConfigPool(t)
	store := newTestStore(t, pool, map[string][]string{"C": {"U_AVERY"}}, map[string]*slack.User{
		"U_AVERY": {ID: "U_AVERY", Name: "avery"},
	})
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, has_external_members, external_members, external_members_checked_at, updated_at)
		VALUES ('C', true, '{U_GONE}', now(), now())
	`); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireExternalCheck(ctx, "C"); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.EnsureExternalMembersFresh(ctx, "C")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasExternalMembers {
		t.Error("expired check should have forced a rescan that cleared the positive")
	}
}

// TestChannelConfig_DefaultAskEnabled pins the join
// policy: auto-enable fires on untouched rows (missing or
// scan-created) but never overrides human-set config.
func TestChannelConfig_DefaultAskEnabled(t *testing.T) {
	pool := getChannelConfigPool(t)
	store := newTestStore(t, pool, nil, nil)
	ctx := context.Background()

	// No row at all: insert + enable.
	enabled, err := store.DefaultAskEnabled(ctx, "C_NEW", "auto (joined channel)")
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Error("fresh channel should auto-enable")
	}
	cfg, err := store.Get(ctx, "C_NEW")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AskEnabled {
		t.Error("ask_enabled should be true after auto-enable")
	}

	// Row created by the automatic scan path (updated_by NULL):
	// still auto-enables — the scan isn't human intent.
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_channel_config (channel_id, has_external_members, external_members, external_members_checked_at)
		VALUES ('C_SCANNED', false, '{}', now())
	`); err != nil {
		t.Fatal(err)
	}
	enabled, err = store.DefaultAskEnabled(ctx, "C_SCANNED", "auto (joined channel)")
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		t.Error("scan-created row should auto-enable")
	}

	// Operator explicitly disabled: auto-enable must NOT override.
	if err := store.SetAskEnabled(ctx, "C_OFF", false, "avery"); err != nil {
		t.Fatal(err)
	}
	enabled, err = store.DefaultAskEnabled(ctx, "C_OFF", "auto (joined channel)")
	if err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Error("auto-enable overrode an operator-set row")
	}
	cfg, err = store.Get(ctx, "C_OFF")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AskEnabled {
		t.Error("operator's ask_enabled=false was flipped")
	}
}

func TestUserInfo_IsExternal(t *testing.T) {
	cases := []struct {
		name string
		ui   UserInfo
		want bool
	}{
		{"internal", UserInfo{}, false},
		{"stranger", UserInfo{IsStranger: true}, true},
		{"restricted", UserInfo{IsRestricted: true}, true},
		{"ultra", UserInfo{IsUltraRestricted: true}, true},
		{"all set", UserInfo{IsStranger: true, IsRestricted: true, IsUltraRestricted: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ui.IsExternal(); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestDirectory_ProfilelessBotCacheRead pins the IsNegativeCache
// derivation on the cache-read path: a real bot with an empty
// profile (is_bot=true, all names empty) must NOT be flagged as a
// negative-cache tombstone when re-read from cache — tombstones are
// always written with is_bot=false, and the !IsBot guard is what
// keeps the bot's slack:<id> canonical key alive.
func TestDirectory_ProfilelessBotCacheRead(t *testing.T) {
	pool := getChannelConfigPool(t)
	users := map[string]*slack.User{
		"U0BOT": {ID: "U0BOT", IsBot: true}, // no name, no profile
	}
	dir := newDirectoryWithAPI(pool, &fakeUserAPI{users: users})
	ctx := context.Background()

	// First lookup: fresh API fetch, persists the bot row.
	ui, err := dir.LookupUserInfo(ctx, "U0BOT")
	if err != nil {
		t.Fatal(err)
	}
	if ui.IsNegativeCache {
		t.Error("fresh profile-less bot flagged as negative-cache")
	}
	// Second lookup: cache hit — the read-path derivation runs.
	ui2, err := dir.LookupUserInfo(ctx, "U0BOT")
	if err != nil {
		t.Fatal(err)
	}
	if ui2.IsNegativeCache {
		t.Error("CACHED profile-less bot flagged as negative-cache (is_bot guard missing)")
	}
	if got := SlackActorCanonical(ui2); got != "slack:U0BOT" {
		t.Errorf("cached bot canonical = %q, want slack:U0BOT", got)
	}
}

func TestDirectory_LookupUserInfo_PersistsExternalFlags(t *testing.T) {
	pool := getChannelConfigPool(t)
	users := map[string]*slack.User{
		"U_VENDOR": {
			ID:         "U_VENDOR",
			Name:       "vendor",
			TeamID:     "T_OTHER",
			IsStranger: true,
		},
	}
	dir := newDirectoryWithAPI(pool, &fakeUserAPI{users: users})
	ctx := context.Background()
	ui, err := dir.LookupUserInfo(ctx, "U_VENDOR")
	if err != nil {
		t.Fatal(err)
	}
	if !ui.IsStranger {
		t.Error("IsStranger should be set from API")
	}
	if !ui.IsExternal() {
		t.Error("IsExternal should be true")
	}
	// Round-trip through the cache.
	ui2, err := dir.LookupUserInfo(ctx, "U_VENDOR")
	if err != nil {
		t.Fatal(err)
	}
	if !ui2.IsStranger {
		t.Error("cached read should preserve IsStranger")
	}
}
