package slack

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/slack-go/slack"
)

// warmAPI is the subset of slack-go methods used to populate the
// Directory caches in one shot. Narrower than the full *slack.Client
// so tests can inject a fake without implementing the whole surface.
type warmAPI interface {
	GetUsersContext(ctx context.Context, options ...slack.GetUsersOption) ([]slack.User, error)
	GetConversationsContext(ctx context.Context, params *slack.GetConversationsParameters) ([]slack.Channel, string, error)
}

// WarmUsers bulk-loads the entire workspace's users into the
// slack_users cache table. Single users.list call (paginated
// internally by slack-go). Returns the number of users written.
// limiter may be nil — production wires it; tests usually leave nil.
//
// Rationale: during backfill, each unique <@Uxxx> mention that
// misses the cache burns one users.info call (tier-3, ~50/min
// workspace-wide). For a channel with thousands of mentions and
// hundreds of distinct users, that's minutes of API budget just
// warming. This replaces that with one tier-2 users.list call up
// front. Negative hits (deactivated accounts) are NOT pre-populated
// here — they come through as part of the GetUsers response, so
// they land in the cache naturally.
func WarmUsers(ctx context.Context, api warmAPI, limiter *Limiter, pool *pgxpool.Pool) (int, error) {
	var users []slack.User
	err := WithSlackRetry(ctx, limiter, methodUsersList, func() error {
		var apiErr error
		users, apiErr = api.GetUsersContext(ctx)
		return apiErr
	})
	if err != nil {
		return 0, fmt.Errorf("warm users: %w", err)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("warm users: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, u := range users {
		if _, err := tx.Exec(ctx, `
			INSERT INTO slack_users
				(user_id, name, display_name, real_name, email, is_bot, fetched_at)
			VALUES ($1, $2, $3, $4, $5, $6, now())
			ON CONFLICT (user_id) DO UPDATE SET
				name = EXCLUDED.name,
				display_name = EXCLUDED.display_name,
				real_name = EXCLUDED.real_name,
				email = EXCLUDED.email,
				is_bot = EXCLUDED.is_bot,
				fetched_at = now()
		`, u.ID, u.Name, u.Profile.DisplayName, u.RealName,
			u.Profile.Email, u.IsBot); err != nil {
			return 0, fmt.Errorf("warm users: upsert %s: %w", u.ID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("warm users: commit: %w", err)
	}
	return len(users), nil
}

// WarmChannels bulk-loads every non-archived public + private channel
// the bot can see into slack_channel_info. Paginates through
// conversations.list until exhausted. Returns the number written.
//
// Excludes archived channels on the assumption that retrieval-
// relevant chatter happens in active channels; a backfill that
// hits an archived channel's id in a mention will do the normal
// lazy fetch + cache miss path.
//
// types: we request "public_channel,private_channel". DMs and
// group DMs are left off — they're unbounded (one channel per
// counterparty), rarely referenced by id in messages, and fetching
// them would surface metadata for people the bot has DM'd that's
// not relevant to retrieval.
func WarmChannels(ctx context.Context, api warmAPI, limiter *Limiter, pool *pgxpool.Pool) (int, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("warm channels: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var observedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&observedAt); err != nil {
		return 0, fmt.Errorf("warm channels: observation time: %w", err)
	}

	var cursor string
	written := 0
	for {
		params := &slack.GetConversationsParameters{
			ExcludeArchived: true,
			Types:           []string{"public_channel", "private_channel"},
			Limit:           1000, // Slack caps the response regardless; be explicit
			Cursor:          cursor,
		}
		var (
			channels []slack.Channel
			next     string
		)
		err := WithSlackRetry(ctx, limiter, methodConvList, func() error {
			var apiErr error
			channels, next, apiErr = api.GetConversationsContext(ctx, params)
			return apiErr
		})
		if err != nil {
			return written, fmt.Errorf("warm channels: list: %w", err)
		}
		for _, c := range channels {
			if err := saveChannelInfo(ctx, tx, &c, observedAt); err != nil {
				return written, fmt.Errorf("warm channels: upsert %s: %w", c.ID, err)
			}
			written++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if err := tx.Commit(ctx); err != nil {
		return written, fmt.Errorf("warm channels: commit: %w", err)
	}
	return written, nil
}

// sleepCtx is a ctx-aware sleep; returns nil on timer fire, ctx.Err()
// if the context is cancelled first. Duplicated from the embedder
// worker to keep this file standalone.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
