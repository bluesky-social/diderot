package discord

import (
	"context"
	"fmt"
	"time"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// Status is the live connection, the last read of the servers, and the
// chosen channels' counts. While live updates stopped for good, or the
// servers can't be read, it says why.
func (Connector) Status(ctx context.Context, deps source.Deps) (settings.Status, error) {
	o, err := ChannelsFor(deps).Overview(ctx)
	if err != nil {
		return settings.Status{}, fmt.Errorf("discord status: %w", err)
	}
	var status settings.Status
	live := "not connected"
	switch {
	case !o.HasGateway:
		live = "not started"
	case o.Gateway.Connected:
		live = "connected"
	case o.Gateway.Fatal:
		live = "stopped"
		status.Waiting = "Live updates stopped: " + o.Gateway.Error + "."
	}
	var since *time.Time
	if o.HasGateway {
		since = &o.Gateway.Since
	}
	var read *time.Time
	if !o.Directory.ReadAt.IsZero() {
		read = &o.Directory.ReadAt
	}
	if o.Directory.Error != "" && status.Waiting == "" {
		status.Waiting = "The servers can't be read: " + o.Directory.Error + "."
	}
	var messages int64
	var failing int64
	for _, id := range selectedIDs(o.Selections) {
		p := o.Progress[id]
		messages += p.Messages
		if p.Problem() != "" {
			failing++
		}
	}
	status.Rows = []settings.StatusRow{
		{Label: "Live updates", Value: since, Detail: live},
		{Label: "Servers last read", Value: read},
		{Label: "Servers", Value: int64(len(o.Directory.Guilds))},
		{Label: "Channels chosen", Value: int64(len(selectedIDs(o.Selections)))},
		{Label: "Messages indexed", Value: messages},
	}
	if failing > 0 {
		status.Rows = append(status.Rows, settings.StatusRow{Label: "Channels that can't be read", Value: failing, Detail: "see Channels below"})
	}
	return status, nil
}
