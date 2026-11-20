package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"
)

func getPrefixPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE events, slack_users, slack_bots, slack_channel_info, slack_usergroups
		 RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

// newTestPrefixBuilder wires a PrefixBuilder with a fakeAPI-backed
// Directory so tests don't hit real Slack.
func newTestPrefixBuilder(t *testing.T, pool *pgxpool.Pool, channels map[string]string) *PrefixBuilder {
	t.Helper()
	fakeChannels := map[string]*slack.Channel{}
	for id, name := range channels {
		// slack.Channel's Name field lives on the embedded GroupConversation.
		c := &slack.Channel{}
		c.ID = id
		c.Name = name
		fakeChannels[id] = c
	}
	api := &fakeAPI{channels: fakeChannels}
	dir := newDirectoryWithAPI(pool, api)
	return NewPrefixBuilder(dir, pool)
}

func TestPrefixTopLevelMessage(t *testing.T) {
	pool := getPrefixPool(t)
	b := newTestPrefixBuilder(t, pool, map[string]string{
		"C123": "ops-incidents",
	})
	got := b.Build(context.Background(), "C123", "", "1700000000.000100", "")
	want := "Slack message in #ops-incidents:"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixThreadRootTreatedAsTopLevel(t *testing.T) {
	// Slack's thread-root message carries thread_ts == ts. That's a
	// root, not a reply — prefix should be the message form, not the
	// reply form (otherwise every root would reference its own body
	// as the "thread subject", which is noisy).
	pool := getPrefixPool(t)
	b := newTestPrefixBuilder(t, pool, map[string]string{
		"C123": "ops-incidents",
	})
	got := b.Build(context.Background(), "C123", "1700000000.000100", "1700000000.000100", "")
	if got != "Slack message in #ops-incidents:" {
		t.Errorf("root message got treated as reply: %q", got)
	}
}

func TestPrefixThreadReplyWithRootInDB(t *testing.T) {
	pool := getPrefixPool(t)
	ctx := context.Background()
	// Seed the thread root event.
	_, err := pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw)
		VALUES ('slack', 'C123:1700000000.000100', to_timestamp(1700000000),
		        'the cluster is angry and alerts are firing', '{}')
	`)
	if err != nil {
		t.Fatal(err)
	}
	b := newTestPrefixBuilder(t, pool, map[string]string{
		"C123": "ops-incidents",
	})
	got := b.Build(ctx, "C123", "1700000000.000100", "1700000005.000200", "")
	want := `Slack reply in #ops-incidents to thread "the cluster is angry and alerts are firing":`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestPrefixThreadReplyWithoutRootInDB(t *testing.T) {
	pool := getPrefixPool(t)
	b := newTestPrefixBuilder(t, pool, map[string]string{
		"C123": "ops-incidents",
	})
	got := b.Build(context.Background(), "C123", "9999999999.999999", "9999999999.999998", "")
	want := "Slack reply in #ops-incidents:"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixDMHasNoChannelName(t *testing.T) {
	pool := getPrefixPool(t)
	b := newTestPrefixBuilder(t, pool, nil)
	got := b.Build(context.Background(), "D0EXAMPLE01", "", "1700000000.000100", "")
	want := "Slack message:"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixUnresolvableChannelFallsBackToID(t *testing.T) {
	pool := getPrefixPool(t)
	// No channel configured; fakeAPI returns channel_not_found via the
	// Directory negative-cache path.
	b := newTestPrefixBuilder(t, pool, nil)
	got := b.Build(context.Background(), "C999", "", "1", "")
	want := "Slack message in #C999:"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixLongThreadRootTruncates(t *testing.T) {
	pool := getPrefixPool(t)
	ctx := context.Background()
	longBody := strings.Repeat("word ", 100) // 500 bytes, 500 runes
	_, err := pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw)
		VALUES ('slack', 'C123:1.1', to_timestamp(1), $1, '{}')
	`, longBody)
	if err != nil {
		t.Fatal(err)
	}
	b := newTestPrefixBuilder(t, pool, map[string]string{
		"C123": "c",
	})
	got := b.Build(ctx, "C123", "1.1", "2.2", "")
	if !strings.Contains(got, "…") {
		t.Errorf("expected ellipsis in truncated prefix: %q", got)
	}
	// Absolute cap: thread-excerpt (120 runes + ellipsis) + frame
	// overhead. prefixMaxLen is 250 runes — well above what the
	// builder will actually emit in practice.
	if len([]rune(got)) > prefixMaxLen {
		t.Errorf("prefix too long: %d runes, cap %d", len([]rune(got)), prefixMaxLen)
	}
}

func TestPrefixIncludesActorWhenProvided(t *testing.T) {
	pool := getPrefixPool(t)
	b := newTestPrefixBuilder(t, pool, map[string]string{
		"C123": "ops-incidents",
	})
	// Top-level message with actor.
	got := b.Build(context.Background(), "C123", "", "1700000000.000100", "avery")
	want := "Slack message in #ops-incidents by @avery:"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrefixIncludesActorInReply(t *testing.T) {
	pool := getPrefixPool(t)
	ctx := context.Background()
	// Seed the thread root.
	_, err := pool.Exec(ctx, `
		INSERT INTO events (source, external_id, occurred_at, body, raw)
		VALUES ('slack', 'C123:1.1', to_timestamp(1), 'the cluster is angry', '{}')
	`)
	if err != nil {
		t.Fatal(err)
	}
	b := newTestPrefixBuilder(t, pool, map[string]string{
		"C123": "ops-incidents",
	})
	got := b.Build(ctx, "C123", "1.1", "2.2", "avery")
	want := `Slack reply in #ops-incidents by @avery to thread "the cluster is angry":`
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestPrefixActorInDM(t *testing.T) {
	pool := getPrefixPool(t)
	b := newTestPrefixBuilder(t, pool, nil)
	got := b.Build(context.Background(), "D0EXAMPLE01", "", "1.1", "alice")
	want := "Slack message by @alice:"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTruncateRunesUTF8Safe(t *testing.T) {
	rocket := strings.Repeat("\U0001F680", 50) // 50 runes, 200 bytes
	out := truncateRunes(rocket, 10)
	if strings.Count(out, "\U0001F680") != 10 {
		t.Errorf("expected 10 rockets, got %d", strings.Count(out, "\U0001F680"))
	}
}
