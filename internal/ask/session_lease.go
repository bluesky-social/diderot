package ask

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bluesky-social/diderot/internal/worker"
)

// sessionLeaseTiming times a session lease: how long one lasts unrenewed,
// how often its holder renews it, and how often a turn waiting for another
// process's lease tries again.
type sessionLeaseTiming struct {
	ttl, renew, retry time.Duration
}

// defaultSessionLeaseTiming renews three times a lease's life, so one slow
// renewal does not lose it, and a process that dies holding one blocks the
// session's next turn for at most 30 seconds.
var defaultSessionLeaseTiming = sessionLeaseTiming{ttl: 30 * time.Second, renew: 10 * time.Second, retry: 500 * time.Millisecond}

// errSessionLeaseLost ends a turn whose session lease another process took
// over, or that could not be renewed for as long as it lasts.
var errSessionLeaseLost = errors.New("ask: lost this session's turn lease")

// sessionLease is the lease a turn holds. fn's context carries it, so that
// each write of the session's history can check that the lease is still
// the turn's (checkSessionLease).
type sessionLease struct{ sessionID, holder uuid.UUID }

type sessionLeaseKey struct{}

// withSessionLease runs fn while this process holds sessionID's row in
// ask_session_leases. It waits, until ctx ends, while another process
// holds an unexpired one; renews the lease while fn runs, cancelling fn's
// context when it is lost; and deletes it when fn returns, so the next
// turn, here or on another replica, starts at once. A holder that stops
// without deleting its lease (a crash) holds the session until the lease
// expires. A holder that freezes past its lease and wakes may run until its
// next renewal, but it writes no history once another process has taken
// the lease over: every write checks the lease in its own transaction.
func (m *Manager) withSessionLease(ctx context.Context, sessionID uuid.UUID, fn func(context.Context) error) error {
	holder := uuid.New()
	if err := m.takeSessionLease(ctx, sessionID, holder); err != nil {
		return err
	}
	leaseCtx, cancel := context.WithCancelCause(ctx)
	renewing := make(chan struct{})
	go func() {
		defer close(renewing)
		m.renewSessionLease(leaseCtx, sessionID, holder, cancel)
	}()
	defer func() {
		cancel(nil)
		<-renewing
		// The turn's own context may have ended; the release must still
		// reach the database for the next turn to start without waiting
		// out the lease.
		releaseCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		if _, err := m.pool.Exec(releaseCtx,
			`DELETE FROM ask_session_leases WHERE session_id = $1 AND holder = $2`, sessionID, holder); err != nil {
			m.logger.Warn("ask: session lease release failed; it expires on its own",
				"session_id", sessionID, "err", err)
		}
	}()
	err := fn(context.WithValue(leaseCtx, sessionLeaseKey{}, sessionLease{sessionID: sessionID, holder: holder}))
	if err != nil && !errors.Is(err, errSessionLeaseLost) && errors.Is(context.Cause(leaseCtx), errSessionLeaseLost) {
		return fmt.Errorf("%w: %w", errSessionLeaseLost, err)
	}
	return err
}

// checkSessionLease is a step of tx, which writes sessionID's history. When
// ctx carries the lease of a turn of that session, it locks the lease's row
// until tx ends, so no other process takes the lease over meanwhile, and
// fails with errSessionLeaseLost unless the lease is still the turn's.
// Writes outside a turn's lease check nothing.
func checkSessionLease(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) error {
	lease, ok := ctx.Value(sessionLeaseKey{}).(sessionLease)
	if !ok || lease.sessionID != sessionID {
		return nil
	}
	var holder uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT holder FROM ask_session_leases WHERE session_id = $1 FOR SHARE`, sessionID).Scan(&holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && holder != lease.holder):
		return errSessionLeaseLost
	case err != nil:
		return fmt.Errorf("check session lease: %w", err)
	}
	return nil
}

// takeSessionLease inserts holder's lease on sessionID, or takes over one
// that has expired, trying again every retry interval until it does or ctx
// ends. Expiry is judged by the database's clock alone.
func (m *Manager) takeSessionLease(ctx context.Context, sessionID, holder uuid.UUID) error {
	for {
		tag, err := m.pool.Exec(ctx, `
			INSERT INTO ask_session_leases (session_id, holder, expires_at)
			VALUES ($1, $2, now() + make_interval(secs => $3))
			ON CONFLICT (session_id) DO UPDATE
			   SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at
			 WHERE ask_session_leases.expires_at <= now()`,
			sessionID, holder, m.lease.ttl.Seconds())
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				return ErrSessionNotFound
			}
			return fmt.Errorf("ask: take session lease: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		timer := time.NewTimer(m.lease.retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// renewSessionLease extends holder's lease every renew interval until ctx
// ends. It calls lost when the lease is no longer holder's (another process
// took it over, or the session was deleted), or when no renewal has
// succeeded for as long as a lease lasts, after which the database may
// already have let another process take it.
func (m *Manager) renewSessionLease(ctx context.Context, sessionID, holder uuid.UUID, lost context.CancelCauseFunc) {
	// Runs on its own goroutine (withSessionLease) for every turn, on
	// every surface (HTTP, Slack, Discord, the MCP ask task): recover
	// alone would leave the turn running with no renewal, so the lease
	// could be taken over mid-turn with nothing to end it. Calling lost
	// routes the panic into withSessionLease's existing
	// errSessionLeaseLost path instead, ending the turn the same
	// ordinary way a lost-to-another-process lease already does.
	defer worker.Recover(m.logger, "ask-session-lease-renew", func() {
		lost(fmt.Errorf("%w: renewal panicked", errSessionLeaseLost))
	})
	ticker := time.NewTicker(m.lease.renew)
	defer ticker.Stop()
	renewed := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		renewCtx, cancel := context.WithTimeout(ctx, m.lease.renew)
		tag, err := m.pool.Exec(renewCtx, `
			UPDATE ask_session_leases SET expires_at = now() + make_interval(secs => $3)
			 WHERE session_id = $1 AND holder = $2`,
			sessionID, holder, m.lease.ttl.Seconds())
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err == nil && tag.RowsAffected() == 1:
			renewed = time.Now()
		case err == nil:
			m.logger.Warn("ask: session lease taken over; ending the turn", "session_id", sessionID)
			lost(errSessionLeaseLost)
			return
		case time.Since(renewed) >= m.lease.ttl:
			m.logger.Warn("ask: session lease could not be renewed before it expired; ending the turn",
				"session_id", sessionID, "err", err)
			lost(errSessionLeaseLost)
			return
		default:
			m.logger.Warn("ask: session lease renewal failed; retrying", "session_id", sessionID, "err", err)
		}
	}
}
