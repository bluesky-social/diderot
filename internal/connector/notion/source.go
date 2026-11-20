package notion

import (
	"context"
	"strings"

	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// Info is Notion's facts as a source.
func (Connector) Info() source.Info {
	return source.Info{
		EventSource: "notion", Title: "Notion", ResultNoun: "Notion",
		Summary: "The Notion pages and databases you choose.",
		ToolDescription: "internal Notion pages: docs, runbooks, specs, design/product docs, and structured database rows. " +
			"Use notion for documented decisions, processes, specs, and reference material.",
		ActorRole: identity.RoleLastEditor,
		IdentityNamespace: func(canonical string) string {
			if strings.HasPrefix(canonical, "notion-bot:") {
				return "notion_bot"
			}
			return "notion_user"
		},
	}
}

// Run holds the sync worker (discovery, managed-root reconcile, backfill)
// and lends the same API client, with its one rate limiter, to the caption
// worker for Notion images.
func (c Connector) Run(ctx context.Context, deps source.Deps, resolved settings.Resolved) error {
	client, err := c.NewClient(resolved.Secret(FieldToken))
	if err != nil {
		return err
	}
	release := deps.Files.LendImages(ctx, NewFetcher(client, nil, deps.Logger.With("component", "notion-fetcher")))
	defer release()

	ingester := NewEventSinkAdapter(deps.Sink, deps.Pool)
	roots := NewRootsStore(deps.Pool)
	roots.SetHider(ingester)
	walker := NewWalker(client, NewDirCache(client), ingester, deps.Logger.With("component", "notion-walker"))
	syncWorker := NewSyncWorker(deps.Pool, walker, roots, deps.Logger.With("component", "notion-sync"))
	syncWorker.Observer = deps.Metrics

	sup := worker.NewSupervisor(deps.Logger, deps.Metrics)
	// The sync worker drives its own search and reconcile timers.
	sup.AddFunc("notion-sync", syncWorker.Run)
	deps.Logger.Info("notion connector running")
	sup.Run(ctx)
	return nil
}

// Section is the managed roots on Notion's page.
func (Connector) Section() string { return "notion-roots" }

// Expand returns the page's full rendered markdown, which ingest stored as
// its body, headed by its title so a sparse database row still says what it
// is. The scope gate ran before it.
func (Connector) Expand(ctx context.Context, deps source.Deps, eventID int64, _ []string) (source.Expanded, error) {
	stored, err := deps.Sink.StoredBody(ctx, eventID)
	if err != nil {
		return source.Expanded{}, err
	}
	if stored.Source != "notion" {
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
