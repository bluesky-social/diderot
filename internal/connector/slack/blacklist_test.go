package slack

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func getBlacklistPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE slack_ingest_blacklist RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestBlacklistAddIsIDOnlyAndIdempotent(t *testing.T) {
	pool := getBlacklistPool(t)
	bl := NewIngestBlacklist(pool, nil)
	ctx := context.Background()
	first, err := bl.Add(ctx, "C123", "friendly-name", "test reason", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if first.ChannelID != "C123" || first.ChannelName != "friendly-name" {
		t.Fatalf("entry = %+v", first)
	}
	second, err := bl.Add(ctx, "C123", "renamed", "different", "someone-else")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Reason != "test reason" || second.ChannelName != "friendly-name" {
		t.Errorf("idempotent add changed original entry: %+v", second)
	}
}

func TestBlacklistMatchesOnlyChannelID(t *testing.T) {
	pool := getBlacklistPool(t)
	bl := NewIngestBlacklist(pool, nil)
	ctx := context.Background()
	if _, err := bl.Add(ctx, "C_BLOCKED", "some-channel", "reason", "op"); err != nil {
		t.Fatal(err)
	}
	if entry, ok := bl.Match("C_BLOCKED"); !ok || entry.Reason != "reason" {
		t.Errorf("id match = %+v, %v", entry, ok)
	}
	if _, ok := bl.Match("C_OTHER"); ok {
		t.Error("reused name must not transfer a blacklist rule")
	}
}

func TestBlacklistRemoveRefreshesSnapshot(t *testing.T) {
	pool := getBlacklistPool(t)
	bl := NewIngestBlacklist(pool, nil)
	ctx := context.Background()
	if _, err := bl.Add(ctx, "C_REMOVE", "", "r", "op"); err != nil {
		t.Fatal(err)
	}
	removed, err := bl.RemoveByID(ctx, "C_REMOVE")
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if _, ok := bl.Match("C_REMOVE"); ok {
		t.Error("removed entry remains in snapshot")
	}
	removed, err = bl.RemoveByID(ctx, "C_REMOVE")
	if err != nil || removed {
		t.Fatalf("second remove = %v, %v", removed, err)
	}
}

func TestBlacklistEmptySnapshotFailsOpen(t *testing.T) {
	bl := NewIngestBlacklist(nil, nil)
	if _, ok := bl.Match("C123"); ok {
		t.Error("unloaded blacklist should fail open")
	}
}

func TestBlacklistConcurrentAddReturnsOneRow(t *testing.T) {
	pool := getBlacklistPool(t)
	bl := NewIngestBlacklist(pool, nil)
	ctx := context.Background()
	const n = 12
	results := make(chan BlacklistEntry, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			e, err := bl.Add(ctx, "C_RACE", "", fmt.Sprintf("r%d", i), "op")
			if err != nil {
				errs <- err
				return
			}
			results <- e
		}(i)
	}
	var id int64
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			t.Fatal(err)
		case e := <-results:
			if id == 0 {
				id = e.ID
			} else if e.ID != id {
				t.Fatalf("different row ids: %d and %d", id, e.ID)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out")
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM slack_ingest_blacklist`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d, %v", rows, err)
	}
}

func TestBlacklistRequiresChannelID(t *testing.T) {
	pool := getBlacklistPool(t)
	bl := NewIngestBlacklist(pool, nil)
	if _, err := bl.Add(context.Background(), "", "name", "r", "op"); err == nil {
		t.Error("expected missing channel id error")
	}
}

// TestBlacklistFollowsChangesMadeThroughAnotherStore: the ingest gates'
// snapshot (the leader's) picks up an entry added or removed through a
// different store (an admin request on any replica) when it is announced,
// not at the next minute's refresh.
func TestBlacklistFollowsChangesMadeThroughAnotherStore(t *testing.T) {
	pool := getBlacklistPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gates, admin := NewIngestBlacklist(pool, nil), NewIngestBlacklist(pool, nil)
	gates.StartRefresher(ctx)

	matches := func(want bool) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			if _, blocked := gates.Match("C123"); blocked == want {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("the gates' snapshot never showed blocked=%v", want)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	if _, err := admin.Add(ctx, "C123", "", "privileged", "operator"); err != nil {
		t.Fatal(err)
	}
	matches(true)
	if _, err := admin.RemoveByID(ctx, "C123"); err != nil {
		t.Fatal(err)
	}
	matches(false)
}
