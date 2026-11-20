package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// blacklistChanged is the Postgres notification channel a change to
// slack_ingest_blacklist is announced on.
const blacklistChanged = "diderot_slack_ingest_blacklist"

// IngestBlacklist is an in-memory snapshot of the operator-managed
// set of stable Slack channel IDs that must never be ingested. The
// store an operator's change is written through and the one the
// ingest gates match against are different stores, often in
// different processes: each change is announced to the others
// (StartRefresher), and List, Add and RemoveByID read and write the
// table itself.
type IngestBlacklist struct {
	pool   *pgxpool.Pool
	logger *slog.Logger

	refreshInterval time.Duration
	refreshMu       sync.Mutex
	snapshot        atomic.Pointer[blacklistSnapshot]
}

// BlacklistEntry is keyed only by ChannelID. ChannelName is
// informational and may be stale after a rename; identity never is.
type BlacklistEntry struct {
	ID          int64
	ChannelID   string
	ChannelName string
	Reason      string
	AddedBy     string
	AddedAt     time.Time
}

type blacklistSnapshot struct {
	byID map[string]BlacklistEntry
}

// NewIngestBlacklist constructs the ID-only store.
func NewIngestBlacklist(pool *pgxpool.Pool, logger *slog.Logger) *IngestBlacklist {
	if logger == nil {
		logger = slog.Default()
	}
	b := &IngestBlacklist{
		pool:            pool,
		logger:          logger,
		refreshInterval: 60 * time.Second,
	}
	b.snapshot.Store(&blacklistSnapshot{byID: map[string]BlacklistEntry{}})
	return b
}

// Refresh serializes loads and atomically publishes one immutable ID map.
func (b *IngestBlacklist) Refresh(ctx context.Context) (int, error) {
	b.refreshMu.Lock()
	defer b.refreshMu.Unlock()

	rows, err := b.pool.Query(ctx, `
		SELECT id, channel_id, coalesce(channel_name, ''),
		       coalesce(reason, ''), coalesce(added_by, ''), added_at
		  FROM slack_ingest_blacklist
		 ORDER BY added_at`)
	if err != nil {
		return 0, fmt.Errorf("blacklist: query: %w", err)
	}
	defer rows.Close()
	snap := &blacklistSnapshot{byID: make(map[string]BlacklistEntry, 16)}
	for rows.Next() {
		var e BlacklistEntry
		if err := rows.Scan(&e.ID, &e.ChannelID, &e.ChannelName,
			&e.Reason, &e.AddedBy, &e.AddedAt); err != nil {
			return 0, fmt.Errorf("blacklist: scan: %w", err)
		}
		snap.byID[e.ChannelID] = e
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("blacklist: rows: %w", err)
	}
	b.snapshot.Store(snap)
	return len(snap.byID), nil
}

// StartRefresher keeps the snapshot current until ctx ends: it reloads
// when any store announces a change, so a channel blacklisted through
// another replica stops being ingested at once, and every minute in
// case an announcement was missed.
func (b *IngestBlacklist) StartRefresher(ctx context.Context) {
	go b.followChanges(ctx)
	go func() {
		t := time.NewTicker(b.refreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := b.Refresh(ctx); err != nil {
					b.logger.Warn("blacklist: refresh failed; retaining previous snapshot", "err", err)
				}
			}
		}
	}()
}

// followChanges reloads the snapshot on every announced change, reconnecting
// while ctx lasts.
func (b *IngestBlacklist) followChanges(ctx context.Context) {
	for ctx.Err() == nil {
		err := b.listenChanges(ctx)
		if ctx.Err() != nil {
			return
		}
		b.logger.Warn("blacklist: change listener disconnected", "err", err)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (b *IngestBlacklist) listenChanges(ctx context.Context) error {
	conn, err := listenOn(ctx, b.pool, blacklistChanged)
	if err != nil {
		return err
	}
	defer closeListener(conn)
	for {
		// Also on connecting, for changes announced while not listening.
		if _, err := b.Refresh(ctx); err != nil {
			return err
		}
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
	}
}

// announce refreshes this store's snapshot after a change and tells the
// other stores to refresh theirs.
func (b *IngestBlacklist) announce(ctx context.Context) {
	if _, err := b.Refresh(ctx); err != nil {
		b.logger.Warn("blacklist: refresh after a change failed", "err", err)
	}
	if _, err := b.pool.Exec(ctx, `SELECT pg_notify($1, '')`, blacklistChanged); err != nil {
		b.logger.Warn("blacklist: announcing a change failed; ingest follows it within a minute", "err", err)
	}
}

// Match checks only stable channel identity.
func (b *IngestBlacklist) Match(channelID string) (BlacklistEntry, bool) {
	if channelID == "" {
		return BlacklistEntry{}, false
	}
	snap := b.snapshot.Load()
	if snap == nil {
		return BlacklistEntry{}, false
	}
	e, ok := snap.byID[channelID]
	return e, ok
}

// Add inserts an ID-keyed entry. channelName is display-only.
func (b *IngestBlacklist) Add(ctx context.Context, channelID, channelName, reason, addedBy string) (BlacklistEntry, error) {
	if channelID == "" {
		return BlacklistEntry{}, errors.New("blacklist: channelID required")
	}
	var e BlacklistEntry
	err := b.pool.QueryRow(ctx, `
		INSERT INTO slack_ingest_blacklist
		       (channel_id, channel_name, reason, added_by, added_at)
		VALUES ($1, nullif($2, ''), $3, nullif($4, ''), now())
		ON CONFLICT (channel_id) DO NOTHING
		RETURNING id, channel_id, coalesce(channel_name, ''),
		          reason, coalesce(added_by, ''), added_at
	`, channelID, channelName, reason, addedBy).Scan(
		&e.ID, &e.ChannelID, &e.ChannelName, &e.Reason, &e.AddedBy, &e.AddedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = b.pool.QueryRow(ctx, `
			SELECT id, channel_id, coalesce(channel_name, ''), reason,
			       coalesce(added_by, ''), added_at
			  FROM slack_ingest_blacklist WHERE channel_id = $1
		`, channelID).Scan(&e.ID, &e.ChannelID, &e.ChannelName,
			&e.Reason, &e.AddedBy, &e.AddedAt)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return BlacklistEntry{}, fmt.Errorf("blacklist: insert: %s", pgErr.Message)
		}
		return BlacklistEntry{}, fmt.Errorf("blacklist: insert: %w", err)
	}
	b.announce(ctx)
	return e, nil
}

func (b *IngestBlacklist) RemoveByID(ctx context.Context, channelID string) (bool, error) {
	tag, err := b.pool.Exec(ctx, `DELETE FROM slack_ingest_blacklist WHERE channel_id = $1`, channelID)
	if err != nil {
		return false, fmt.Errorf("blacklist: remove: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	b.announce(ctx)
	return true, nil
}
