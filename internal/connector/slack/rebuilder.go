package slack

import (
	"context"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/reembed"
)

// Rebuilder implements reembed.SourceRebuilder for slack events.
// Re-runs ParseMessage on the stored `raw` payload against the
// current Directory (so mention-resolution, channel-name, actor
// display-name all pick up fresh cache state) and rebuilds the
// contextual prefix. The events.DBSink with ForceUpdate=true
// then rewrites chunks + queues re-embedding.
type Rebuilder struct {
	dir           *Directory
	prefix        *PrefixBuilder
	workspaceHost string
}

// NewRebuilder builds a slack rebuilder. dir/prefix must be the
// same shared instances the live consumer and backfill use, so
// the Directory cache warms once and everyone benefits.
func NewRebuilder(dir *Directory, prefix *PrefixBuilder, workspaceHost string) *Rebuilder {
	return &Rebuilder{dir: dir, prefix: prefix, workspaceHost: workspaceHost}
}

// Source returns "slack".
func (r *Rebuilder) Source() string { return "slack" }

// Rebuild reconstructs the Event. On malformed `raw` (shouldn't
// happen — we stored it ourselves) or ParseMessage returning a
// non-ingest shape (subtype we'd now skip, delete tuple for some
// reason), it returns (nil, nil) to signal skip.
func (r *Rebuilder) Rebuild(ctx context.Context, row reembed.StoredRow) (*events.Event, error) {
	res, err := ParseMessage(ctx, row.Raw, r.workspaceHost, r.dir)
	if err != nil {
		return nil, err
	}
	if res.Event == nil {
		// Subtype now mapped to delete or deliberately skipped.
		// No harm to the existing row; we just don't re-ingest it.
		return nil, nil
	}

	// Resolve display name + canonical key via Directory.
	if r.dir != nil && res.Event.ActorID != "" {
		switch {
		case res.Event.ActorCanonical == "":
			if ui, err := r.dir.LookupUserInfo(ctx, res.Event.ActorID); err == nil {
				res.Event.Actor = pickName(ui.DisplayName, ui.Name)
				res.Event.ActorCanonical = SlackActorCanonical(ui)
			}
		case res.Event.Actor == "" && res.Event.ActorCanonical == SlackBotActorCanonical(res.Event.ActorID):
			if bi, err := r.dir.LookupBot(ctx, res.Event.ActorID); err == nil {
				res.Event.Actor = bi.Name
			}
		}
	}

	// Rebuild contextual prefix. Need the ts (derived from
	// external_id = channel:ts); parse the same way the live
	// consumer and backfill do.
	details, _ := DetailsFrom(row.Raw)
	// external_id = "channel:ts" — strip the channel prefix.
	ts := ""
	if details.ChannelID != "" && len(row.ExternalID) > len(details.ChannelID)+1 {
		ts = row.ExternalID[len(details.ChannelID)+1:]
	}
	res.Event.ContextPrefix = r.prefix.Build(ctx, details.ChannelID, details.ThreadTS, ts, res.Event.Actor)

	// Visibility scope from channel privacy (deny-by-default on
	// lookup failure — see stampScope). Re-stamping during rebuild
	// is also what heals scope drift on force-reembeds.
	stampScope(ctx, r.dir, nil, details.ChannelID, res.Event)
	return res.Event, nil
}
