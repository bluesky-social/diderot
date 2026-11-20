package slack

import (
	"context"
	"testing"
)

// An administrator's block on a channel stops Diderot reading it. A
// thread queued before the block is left unclaimed, and pending, until the
// channel is unblocked.
func TestThreadWorker_ClaimNext_LeavesABlockedChannelsThreadsPending(t *testing.T) {
	pool := getSyncPool(t)
	ctx := context.Background()
	w := newTestThreadWorker(t, pool, nil)
	if _, err := pool.Exec(ctx, `TRUNCATE slack_ingest_blacklist`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `TRUNCATE slack_ingest_blacklist`) })

	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_thread_sync (channel_id, thread_ts, enqueued_at)
		VALUES ('C_BLOCKED', '1700000002.000100', now()),
		       ('C_OPEN', '1700000001.000100', now() - '1 hour'::interval)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO slack_ingest_blacklist (channel_id) VALUES ('C_BLOCKED')`); err != nil {
		t.Fatal(err)
	}

	claim, err := w.ClaimNext(ctx)
	if err != nil || claim == nil {
		t.Fatalf("ClaimNext: %v, %v", claim, err)
	}
	if claim.ChannelID != "C_OPEN" {
		t.Fatalf("claimed a thread of %s, want the open channel's", claim.ChannelID)
	}
	if claim, err = w.ClaimNext(ctx); err != nil || claim != nil {
		t.Fatalf("claimed %+v (err %v) with only the blocked channel's thread left", claim, err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM slack_ingest_blacklist WHERE channel_id = 'C_BLOCKED'`); err != nil {
		t.Fatal(err)
	}
	claim, err = w.ClaimNext(ctx)
	if err != nil || claim == nil || claim.ChannelID != "C_BLOCKED" {
		t.Fatalf("after the unblock: claimed %+v (err %v), want the unblocked channel's thread", claim, err)
	}
}
