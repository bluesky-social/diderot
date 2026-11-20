package slack

import (
	"context"

	"github.com/slack-go/slack"
)

// Delivery is what a running Slack connector lends to work outside it: its
// API client and the limiter every Slack call shares, its consumer, which
// delivers answers, reports and operator notifications, the resolvers that
// say who a DM or a channel reaches, and the rebuilder that re-derives its
// stored events. It belongs to one run: use it inside the With of the
// settings.Live the run sets, which the run's end waits for, and never keep
// it.
type Delivery struct {
	// Run is the run's context. It ends as the run starts to stop, before
	// the run waits for the uses still holding its delivery.
	Run      context.Context
	API      *slack.Client
	Limiter  *Limiter
	Consumer *Consumer
	// DMScopes resolves a person's private-channel scopes; live DMs share
	// it, and its membership cache, with DM reports.
	DMScopes *UserScopeResolver
	// Channels resolves what a channel's audience may read.
	Channels  *ChannelAuthorizer
	Rebuilder *Rebuilder
}

// Running reports whether the run that lent d is still up, rather than
// stopping: work it would take on now could not finish through it.
func (d Delivery) Running() bool {
	return d.Run == nil || d.Run.Err() == nil
}

// Within returns ctx, also ended when the run that lent d starts to stop,
// so a use waiting on Slack, such as a retry after a 429, never holds that
// run's end. Call the returned func when the use is done.
func (d Delivery) Within(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if d.Run == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(d.Run, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}
