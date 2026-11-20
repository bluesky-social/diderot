package slack

import (
	"context"

	"github.com/slack-go/slack/slackevents"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askchat"
)

// An Ask turn shows its work in Slack as the thread's agent session being
// "processing" (Slack's "Working…"). Slack keeps a session processing until
// the app sets it back to active, or for an hour, so the engine records the
// thread as working first (askchat's working records, in
// slack_working_threads): if its process dies, the Slack leader's sweep
// clears the thread instead of leaving it working for the hour.

// interruptedNotice tells a thread that the turn working on it died.
const interruptedNotice = askchat.InterruptedNotice

// workingThread is one expired working record.
type workingThread = askchat.WorkingThread

// workingThreadStore is the working records; tests without a database use
// a fake.
type workingThreadStore = askchat.WorkingStore

// workingThreads is the consumer's store: the injected one, or the
// database's when it has a pool. nil means status is never set.
func (c *Consumer) workingThreads() workingThreadStore {
	if c.working != nil {
		return c.working
	}
	if c.pool != nil {
		return askchat.PGWorkingThreads{Pool: c.pool, Surface: ask.AskRunSurfaceSlack}
	}
	return nil
}

// handleAgentSessionStopped is someone pressing Slack's stop button on a
// thread Diderot is working on. Slack leaves the session's status to the
// app: the turn, stopped, clears it, and so does this, at once.
func (c *Consumer) handleAgentSessionStopped(ctx context.Context, ev *slackevents.AgentSessionStoppedEvent) {
	if ev.Channel == "" || ev.ThreadTimestamp == "" {
		return
	}
	conv := askchat.Conversation{Place: ev.Channel, Thread: ev.ThreadTimestamp}
	stopped := c.engine().Stop(ctx, conv)
	c.logger.Info("ask: stop pressed", "channel", ev.Channel, "thread_ts", ev.ThreadTimestamp, "turns_here", stopped)
	if c.progress == nil {
		return
	}
	if _, err := (slackSurface{c}).ClearProgress(ctx, conv); err != nil {
		c.logger.Warn("ask: clearing a stopped session failed", "channel", ev.Channel, "thread_ts", ev.ThreadTimestamp, "err", err)
	}
}

// runWorkingThreadSweep clears expired working threads while the leader
// runs, at once and then every askchat.WorkingSweepEvery.
func (c *Consumer) runWorkingThreadSweep(ctx context.Context) {
	if c.workingThreads() == nil || c.progress == nil {
		return
	}
	c.engine().RunSweep(ctx, func(n int) { c.metrics.askStatus.WithLabelValues("swept").Add(float64(n)) })
}

// sweepWorkingThreads sets each expired working thread back to active,
// telling a thread whose turn never finished that the answer was
// interrupted (askchat.Engine.Sweep).
func (c *Consumer) sweepWorkingThreads(ctx context.Context) error {
	if c.workingThreads() == nil || c.progress == nil {
		return nil
	}
	n, err := c.engine().Sweep(ctx)
	if n > 0 {
		c.metrics.askStatus.WithLabelValues("swept").Add(float64(n))
	}
	return err
}
