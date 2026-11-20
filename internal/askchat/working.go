package askchat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/worker"
)

// A turn shows its work in a conversation (Slack's agent status, Discord's
// placeholder), and a surface can leave that showing after its process dies.
// So a turn records the conversation as working first, renews the record
// while it runs, and the leader's sweep takes down what an expired record
// left showing. The record is also how a stop reaches a turn on another
// replica.
const (
	// WorkingTTL is how long a working record lasts unrenewed; a turn whose
	// process dies is cleared about this long after.
	WorkingTTL = 30 * time.Second
	// WorkingSettle is how long a finished turn whose progress could not be
	// confirmed down leaves its record, so that a progress call still in
	// flight has landed before the sweep takes it down.
	WorkingSettle = 30 * time.Second
	// WorkingSweepEvery is how often the leader sweeps expired records.
	WorkingSweepEvery = 15 * time.Second
)

// WorkingRenew is how often a running turn renews its record, and so how
// long a stop pressed while the turn runs on another replica takes to reach
// it. A variable so tests can shorten it.
var WorkingRenew = 5 * time.Second

// InterruptedNotice tells a conversation that the turn working on it died.
const InterruptedNotice = "I was interrupted before I could finish answering this. Please ask again."

// WorkingThread is one expired working record.
type WorkingThread struct {
	ChannelID, ThreadTS string
	Holder              uuid.UUID
}

// WorkingStore is the working records. PGWorkingThreads is the database's;
// tests without one use a fake.
type WorkingStore interface {
	// Begin records that holder's turn is about to show progress. It takes
	// the record over from an earlier turn of the same conversation.
	Begin(ctx context.Context, channelID, threadTS string, holder uuid.UUID) error
	// Renew extends holder's record and reports whether someone asked to
	// stop the turn. held is false once the record is no longer holder's.
	Renew(ctx context.Context, channelID, threadTS string, holder uuid.UUID) (stopRequested, held bool, err error)
	// Finish ends holder's turn: it deletes the record when the progress is
	// confirmed down (cleared), and otherwise leaves it to expire after
	// WorkingSettle, marked finished when the turn said what it had to.
	Finish(ctx context.Context, channelID, threadTS string, holder uuid.UUID, cleared, finished bool) error
	// RequestStop marks the conversation's turn, on whatever replica, to
	// stop.
	RequestStop(ctx context.Context, channelID, threadTS string) error
	// Expired lists the records past their expiry.
	Expired(ctx context.Context) ([]WorkingThread, error)
	// Claim takes an expired record for the sweep: it marks it finished and
	// pushes its expiry out by WorkingSettle, so a sweep that cannot clear
	// it tries again later. claimed is false when the record is no longer
	// expired and holder's: its turn renewed it, or a new turn took it over.
	// notify is whether the turn ended without saying anything: it neither
	// finished nor was stopped by its asker.
	Claim(ctx context.Context, row WorkingThread) (claimed, notify bool, err error)
	// Release deletes a claimed record once its progress is down.
	Release(ctx context.Context, row WorkingThread) error
}

// PGWorkingThreads is one surface's working records in the database. The
// table is slack_working_threads, named for the first surface; its keys are
// any surface's channel and thread IDs, and each record names its surface,
// whose sweep alone clears it.
type PGWorkingThreads struct {
	Pool    *pgxpool.Pool
	Surface ask.AskRunSurface
}

func (s PGWorkingThreads) Begin(ctx context.Context, channelID, threadTS string, holder uuid.UUID) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO slack_working_threads (channel_id, thread_ts, holder, expires_at, surface)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5)
		ON CONFLICT (channel_id, thread_ts) DO UPDATE
		   SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at,
		       finished = false, stop_requested = false, surface = EXCLUDED.surface`,
		channelID, threadTS, holder, WorkingTTL.Seconds(), string(s.Surface))
	return err
}

func (s PGWorkingThreads) Renew(ctx context.Context, channelID, threadTS string, holder uuid.UUID) (bool, bool, error) {
	var stop bool
	err := s.Pool.QueryRow(ctx, `
		UPDATE slack_working_threads SET expires_at = now() + make_interval(secs => $4)
		 WHERE channel_id = $1 AND thread_ts = $2 AND holder = $3
		RETURNING stop_requested`,
		channelID, threadTS, holder, WorkingTTL.Seconds()).Scan(&stop)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return stop, err == nil, err
}

func (s PGWorkingThreads) Finish(ctx context.Context, channelID, threadTS string, holder uuid.UUID, cleared, finished bool) error {
	if cleared {
		_, err := s.Pool.Exec(ctx, `
			DELETE FROM slack_working_threads WHERE channel_id = $1 AND thread_ts = $2 AND holder = $3`,
			channelID, threadTS, holder)
		return err
	}
	// Even an unfinished turn's progress call may still be in flight (a
	// turn cut off by losing leadership doesn't wait for it), so every
	// record settles before the sweep clears it.
	_, err := s.Pool.Exec(ctx, `
		UPDATE slack_working_threads
		   SET expires_at = now() + make_interval(secs => $4), finished = $5
		 WHERE channel_id = $1 AND thread_ts = $2 AND holder = $3`,
		channelID, threadTS, holder, WorkingSettle.Seconds(), finished)
	return err
}

func (s PGWorkingThreads) RequestStop(ctx context.Context, channelID, threadTS string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE slack_working_threads SET stop_requested = true WHERE channel_id = $1 AND thread_ts = $2`,
		channelID, threadTS)
	return err
}

func (s PGWorkingThreads) Expired(ctx context.Context) ([]WorkingThread, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT channel_id, thread_ts, holder FROM slack_working_threads
		 WHERE expires_at <= now() AND surface = $1 ORDER BY expires_at LIMIT 100`, string(s.Surface))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkingThread, error) {
		var w WorkingThread
		err := row.Scan(&w.ChannelID, &w.ThreadTS, &w.Holder)
		return w, err
	})
}

func (s PGWorkingThreads) Claim(ctx context.Context, row WorkingThread) (bool, bool, error) {
	// The FROM subquery reads the row as it was before this update.
	var notify bool
	err := s.Pool.QueryRow(ctx, `
		UPDATE slack_working_threads t
		   SET finished = true, expires_at = now() + make_interval(secs => $4)
		  FROM (SELECT finished, stop_requested FROM slack_working_threads
		         WHERE channel_id = $1 AND thread_ts = $2 AND holder = $3
		           AND expires_at <= now() FOR UPDATE) before
		 WHERE t.channel_id = $1 AND t.thread_ts = $2 AND t.holder = $3
		RETURNING NOT before.finished AND NOT before.stop_requested`,
		row.ChannelID, row.ThreadTS, row.Holder, WorkingSettle.Seconds()).Scan(&notify)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, notify, err
}

func (s PGWorkingThreads) Release(ctx context.Context, row WorkingThread) error {
	_, err := s.Pool.Exec(ctx, `
		DELETE FROM slack_working_threads WHERE channel_id = $1 AND thread_ts = $2 AND holder = $3`,
		row.ChannelID, row.ThreadTS, row.Holder)
	return err
}

// Turns are the turns running in this process, by conversation, so that a
// stop reaches them at once. A turn on another replica sees the stop through
// its working record instead.
type Turns struct {
	mu     sync.Mutex
	byConv map[Conversation]map[uuid.UUID]context.CancelCauseFunc
}

func (t *Turns) add(channelID, threadTS string, holder uuid.UUID, stop context.CancelCauseFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byConv == nil {
		t.byConv = map[Conversation]map[uuid.UUID]context.CancelCauseFunc{}
	}
	key := Conversation{channelID, threadTS}
	if t.byConv[key] == nil {
		t.byConv[key] = map[uuid.UUID]context.CancelCauseFunc{}
	}
	t.byConv[key][holder] = stop
}

func (t *Turns) remove(channelID, threadTS string, holder uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := Conversation{channelID, threadTS}
	delete(t.byConv[key], holder)
	if len(t.byConv[key]) == 0 {
		delete(t.byConv, key)
	}
}

// stop stops every turn running here in the conversation and returns how
// many.
func (t *Turns) stop(channelID, threadTS string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	turns := t.byConv[Conversation{channelID, threadTS}]
	for _, stop := range turns {
		stop(ask.ErrTurnStopped)
	}
	return len(turns)
}

// workingTurn is one turn's hold on its conversation's working record.
type workingTurn struct {
	store               WorkingStore
	logger              *slog.Logger
	channelID, threadTS string
	holder              uuid.UUID
	stopRenew           context.CancelFunc
	renewing            chan struct{}
}

// BeginWorking records the conversation before its progress shows, and
// renews the record until the returned turn's finish, stopping the turn
// (stop) when someone asks to.
func beginWorking(ctx context.Context, store WorkingStore, logger *slog.Logger, channelID, threadTS string, holder uuid.UUID, stop context.CancelCauseFunc) (*workingTurn, error) {
	if store == nil {
		return nil, errors.New("no working-thread store")
	}
	if err := store.Begin(ctx, channelID, threadTS, holder); err != nil {
		return nil, err
	}
	renewCtx, stopRenew := context.WithCancel(context.WithoutCancel(ctx))
	w := &workingTurn{store: store, logger: logger, channelID: channelID, threadTS: threadTS,
		holder: holder, stopRenew: stopRenew, renewing: make(chan struct{})}
	go w.renew(renewCtx, stop)
	return w, nil
}

func (w *workingTurn) renew(ctx context.Context, stop context.CancelCauseFunc) {
	defer close(w.renewing)
	// A panic here (e.g. a transient DB-driver bug) must not crash the
	// whole process over a renewal loop. Recovering alone would leave the
	// turn running with no renewal: its working record would expire after
	// WorkingTTL and the sweep would treat it as a dead turn (post
	// InterruptedNotice, release the record) while the turn -- whose
	// context this did not cancel -- could still be mid-answer, and a
	// cross-replica Stop could no longer reach it either, since that too
	// is delivered by this same loop polling stop_requested. Stopping the
	// turn here instead routes the panic into RunTurn's ordinary
	// turn-ended path (ask.ErrTurnStopped-shaped: the turn stops cleanly
	// and its own cleanup runs), matching what every other recovered
	// panic in this package does: end the unit of work as a failure, not
	// leave it in an ambiguous half-alive state.
	defer worker.Recover(w.logger, "ask-working-renew", func() {
		stop(errors.New("ask: working-record renewal panicked"))
	})
	ticker := time.NewTicker(WorkingRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		renewCtx, cancel := context.WithTimeout(ctx, WorkingRenew)
		stopRequested, held, err := w.store.Renew(renewCtx, w.channelID, w.threadTS, w.holder)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			w.logger.Warn("ask: working thread renewal failed", "channel", w.channelID, "thread_ts", w.threadTS, "err", err)
		case !held:
			return
		case stopRequested:
			stop(ask.ErrTurnStopped)
		}
	}
}

// finish stops renewing and records how the turn ended (see
// WorkingStore.Finish). It writes even after ctx has ended: a turn cut off
// by losing leadership must leave its record for the next leader.
func (w *workingTurn) finish(ctx context.Context, cleared, finished bool) {
	w.stopRenew()
	<-w.renewing
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := w.store.Finish(writeCtx, w.channelID, w.threadTS, w.holder, cleared, finished); err != nil {
		w.logger.Warn("ask: working thread finish failed; it expires on its own",
			"channel", w.channelID, "thread_ts", w.threadTS, "err", err)
	}
}

// Sweep takes down the progress expired working records left showing. A
// conversation whose turn never finished (its process died, or lost
// leadership mid-turn) and was not stopped by its asker is told first that
// the answer was interrupted. A record whose progress can't be taken down is
// tried again once its claim expires. It returns how many it took down.
func (e *Engine) Sweep(ctx context.Context) (int, error) {
	if e.Working == nil {
		return 0, nil
	}
	expired, err := e.Working.Expired(ctx)
	if err != nil {
		return 0, fmt.Errorf("list expired working threads: %w", err)
	}
	swept := 0
	for _, row := range expired {
		claimed, notify, err := e.Working.Claim(ctx, row)
		if err != nil {
			return swept, fmt.Errorf("claim working thread: %w", err)
		}
		if !claimed {
			continue
		}
		conv := Conversation{row.ChannelID, row.ThreadTS}
		if notify {
			e.PostNotice(ctx, conv, InterruptedNotice)
		}
		if ok, err := e.Surface.ClearProgress(ctx, conv); err != nil || !ok {
			e.logger().Warn("ask: clearing an expired working thread failed; retrying next sweep",
				"channel", row.ChannelID, "thread_ts", row.ThreadTS, "err", err)
			continue
		}
		if err := e.Working.Release(ctx, row); err != nil {
			return swept, fmt.Errorf("release working thread: %w", err)
		}
		swept++
	}
	return swept, nil
}

// RunSweep sweeps at once, then every WorkingSweepEvery, until ctx ends.
func (e *Engine) RunSweep(ctx context.Context, swept func(int)) {
	ticker := time.NewTicker(WorkingSweepEvery)
	defer ticker.Stop()
	for {
		n, err := e.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			e.logger().Warn("ask: working thread sweep failed", "err", err)
		}
		if swept != nil && n > 0 {
			swept(n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
