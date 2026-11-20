package adminapi

import (
	"context"
	"testing"

	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocumentReplayNeverEnablesDisabledChannel(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "adminapi_browser_sources"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE slack_channel_config`)
	require.NoError(t, err)

	configs := slackcx.NewChannelConfigStore(pool, nil, nil, nil)
	server := NewServer(nil, nil)
	server.SetSlackControls(func() (SlackControls, bool) { return SlackControls{ChannelConfigs: configs}, true })

	err = server.RequestSlackDocumentBackfill(ctx, "CPLATFORM", "operator@example.com")
	assert.EqualError(t, err, "document ingest is disabled for this channel; enable it before requesting a replay")
	config, err := configs.Get(ctx, "CPLATFORM")
	require.NoError(t, err)
	assert.False(t, config.DocIngestEnabled)

	require.NoError(t, configs.SetDocIngestEnabled(ctx, "CPLATFORM", true, "operator@example.com"))
	_, err = pool.Exec(ctx, `UPDATE slack_channel_config SET doc_backfill_cursor=42 WHERE channel_id='CPLATFORM'`)
	require.NoError(t, err)
	require.NoError(t, server.RequestSlackDocumentBackfill(ctx, "CPLATFORM", "operator@example.com"))
	config, err = configs.Get(ctx, "CPLATFORM")
	require.NoError(t, err)
	assert.True(t, config.DocIngestEnabled)
	require.NotNil(t, config.DocBackfillCursor)
	assert.Zero(t, *config.DocBackfillCursor)
}
