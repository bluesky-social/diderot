package docs

import (
	"context"
	"testing"
)

// An administrator's block on a Slack channel stops Diderot reading it.
// A document of a message indexed before the block is still read from
// Slack when it is extracted, so the worker leaves it unclaimed, and
// pending, until the channel is unblocked.
func TestClaimLeavesABlockedSlackChannelsDocumentsPending(t *testing.T) {
	pool := workerTestPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `TRUNCATE slack_ingest_blacklist`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `TRUNCATE slack_ingest_blacklist`) })

	_, docID := seedDoc(t, pool, "C_BLOCKED", "F1", true)
	if _, err := pool.Exec(ctx, `INSERT INTO slack_ingest_blacklist (channel_id) VALUES ('C_BLOCKED')`); err != nil {
		t.Fatal(err)
	}

	w := newTestWorker(pool, nil)
	row, err := w.claimPending(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if row != nil {
		t.Fatalf("claimed document %d of a blocked channel", row.documentID)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM slack_ingest_blacklist WHERE channel_id = 'C_BLOCKED'`); err != nil {
		t.Fatal(err)
	}
	row, err = w.claimPending(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil || row.documentID != docID {
		t.Fatalf("after the unblock: claimed %+v, want document %d", row, docID)
	}
}
