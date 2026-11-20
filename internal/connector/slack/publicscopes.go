package slack

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/events"
)

type PublicScopeResolver struct {
	pool      *pgxpool.Pool
	logger    *slog.Logger
	ttl       time.Duration
	now       func() time.Time
	mu        sync.Mutex
	scopes    []string
	expiresAt time.Time
}

func NewPublicScopeResolver(pool *pgxpool.Pool, logger *slog.Logger) *PublicScopeResolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &PublicScopeResolver{pool: pool, logger: logger, ttl: time.Minute, now: time.Now}
}

func (resolver *PublicScopeResolver) Scopes(ctx context.Context) []string {
	if resolver == nil || resolver.pool == nil || ctx.Err() != nil {
		return nil
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.now().Before(resolver.expiresAt) {
		return slices.Clone(resolver.scopes)
	}
	rows, err := resolver.pool.Query(ctx, `
		SELECT channel_id FROM slack_channel_info
		 WHERE NOT is_private AND COALESCE(name, '') <> ''
		   AND channel_id ~ '^[CG][A-Z0-9]+$'
		 ORDER BY channel_id`)
	if err != nil {
		resolver.logger.WarnContext(ctx, "public channel scopes unavailable", "err", err)
		return nil
	}
	defer rows.Close()
	var scopes []string
	for rows.Next() {
		var channel string
		if err := rows.Scan(&channel); err != nil {
			resolver.logger.WarnContext(ctx, "public channel scope unreadable", "err", err)
			return nil
		}
		scopes = append(scopes, events.ScopeChannel(channel))
	}
	if err := rows.Err(); err != nil {
		resolver.logger.WarnContext(ctx, "public channel scopes incomplete", "err", err)
		return nil
	}
	resolver.scopes = scopes
	resolver.expiresAt = resolver.now().Add(resolver.ttl)
	return slices.Clone(scopes)
}
