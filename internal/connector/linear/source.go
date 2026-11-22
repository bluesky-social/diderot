package linear

import (
	"context"

	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// Info is Linear's facts as a source.
func (Connector) Info() source.Info {
	return source.Info{
		EventSource: "linear", Title: "Linear", ResultNoun: "Linear",
		Summary:           "Linear issues and their comments.",
		ToolDescription:   "engineering issue tracker: tasks, bugs, projects, and their discussion threads.",
		ActorRole:         identity.RoleIssueCreator,
		IdentityNamespace: func(string) string { return "linear_user" },
	}
}

// Run holds the sync worker: incremental sync, the visibility sweep, the
// daily reconcile, and backfills. Credentials for another workspace than the
// last ones' start the sync over.
func (c Connector) Run(ctx context.Context, deps source.Deps, resolved settings.Resolved) error {
	client, err := c.NewClient(resolved)
	if err != nil {
		return err
	}
	reset, err := startWorkspace(ctx, deps.Pool, deps.State, client)
	if err != nil {
		return err
	}
	if reset {
		deps.Logger.Info("linear credentials belong to another workspace; sync starts over")
	}
	syncWorker := NewSyncWorker(deps.Pool, client, deps.Sink, deps.Logger.With("component", "linear-sync"), deps.Metrics)
	syncWorker.ExcludeTeams = resolved.List(FieldExcludeTeams)
	syncWorker.Observer = deps.Metrics

	sup := worker.NewSupervisor(deps.Logger, deps.Metrics)
	sup.AddFunc("linear-sync", syncWorker.Run)
	deps.Logger.Info("linear connector running", "exclude_teams", syncWorker.ExcludeTeams)
	sup.Run(ctx)
	return nil
}

// Expand returns the issue's full rendered markdown, which ingest stored as
// its body, headed by its title. The scope gate ran before it.
func (Connector) Expand(ctx context.Context, deps source.Deps, eventID int64, _ []string) (source.Expanded, error) {
	stored, err := deps.Sink.StoredBody(ctx, eventID)
	if err != nil {
		return source.Expanded{}, err
	}
	if stored.Source != "linear" {
		// The event was re-ingested as another source since the caller
		// looked it up.
		return source.Expanded{}, source.ErrNotExpandable
	}
	content := stored.Body
	if stored.Title != "" {
		content = "# " + stored.Title + "\n\n" + stored.Body
	}
	return source.Expanded{Content: content}, nil
}
