package files

import (
	"context"
	"testing"
)

// An administrator's block on a Slack channel stops Diderot reading it.
// An image of a message indexed before the block is still read from Slack
// when it is captioned, so the worker leaves it unclaimed, and pending,
// until the channel is unblocked.
func TestClaimLeavesABlockedSlackChannelsImagesPending(t *testing.T) {
	pool := getWorkerPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE slack_ingest_blacklist`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `TRUNCATE slack_ingest_blacklist`) })

	eventID := seedEventForWorker(t, pool, "C_BLOCKED:1.0")
	if _, err := pool.Exec(ctx, `
		INSERT INTO slack_message_details (event_id, channel_id, user_id, has_files, occurred_at)
		VALUES ($1, 'C_BLOCKED', 'U1', true, now())
	`, eventID); err != nil {
		t.Fatal(err)
	}
	imageID := seedImage(t, pool, eventID, "F1")
	if _, err := pool.Exec(ctx, `INSERT INTO slack_ingest_blacklist (channel_id) VALUES ('C_BLOCKED')`); err != nil {
		t.Fatal(err)
	}

	w := &Worker{Pool: pool, Fetchers: map[string]SourceFetcher{"slack": &fakeFetcher{}}}
	row, err := w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Fatalf("claimed image %d of a blocked channel", row.imageID)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM slack_ingest_blacklist WHERE channel_id = 'C_BLOCKED'`); err != nil {
		t.Fatal(err)
	}
	row, err = w.claimUncaptioned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.imageID != imageID {
		t.Fatalf("after the unblock: claimed %+v, want image %d", row, imageID)
	}
}
