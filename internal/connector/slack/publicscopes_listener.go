package slack

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// listenOn opens a connection of its own, outside pool, that LISTENs on
// channel. The caller closes it with closeListener.
func listenOn(ctx context.Context, pool *pgxpool.Pool, channel string) (*pgx.Conn, error) {
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// A dedicated connection outside the pool still needs the pool's
	// BeforeConnect: with RDS IAM auth (internal/dbconn) that is what sets
	// the login token.
	poolConfig := pool.Config()
	connConfig := poolConfig.ConnConfig.Copy()
	if poolConfig.BeforeConnect != nil {
		if err := poolConfig.BeforeConnect(connectCtx, connConfig); err != nil {
			return nil, err
		}
	}
	conn, err := pgx.ConnectConfig(connectCtx, connConfig)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `LISTEN `+pgx.Identifier{channel}.Sanitize()); err != nil {
		closeListener(conn)
		return nil, err
	}
	return conn, nil
}

func closeListener(conn *pgx.Conn) {
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Close(closeCtx)
}

func (resolver *PublicScopeResolver) Invalidate() {
	if resolver == nil {
		return
	}
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.scopes = nil
	resolver.expiresAt = time.Time{}
}

func (resolver *PublicScopeResolver) RunInvalidationListener(ctx context.Context) {
	for ctx.Err() == nil {
		err := resolver.listenInvalidations(ctx)
		resolver.Invalidate()
		if ctx.Err() != nil {
			return
		}
		resolver.logger.WarnContext(ctx, "public channel invalidation listener disconnected", "err", err)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (resolver *PublicScopeResolver) listenInvalidations(ctx context.Context) error {
	conn, err := listenOn(ctx, resolver.pool, "diderot_slack_public_scopes")
	if err != nil {
		return err
	}
	defer closeListener(conn)
	resolver.Invalidate()
	resolver.logger.InfoContext(ctx, "public channel invalidation listener connected")
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		resolver.Invalidate()
		resolver.logger.DebugContext(ctx, "public channel scope cache invalidated")
	}
}
