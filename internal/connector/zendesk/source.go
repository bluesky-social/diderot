package zendesk

import (
	"context"

	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
	"github.com/bluesky-social/diderot/internal/worker"
)

// Info is Zendesk's facts as a source.
func (Connector) Info() source.Info {
	return source.Info{
		EventSource: "zendesk", Title: "Zendesk", ResultNoun: "Zendesk",
		Summary: "Zendesk tickets and Help Center articles.",
		ToolDescription: "indexed customer support tickets, individual replies/internal notes, and Help Center articles with available attachment content. " +
			"Use zendesk for support reports and guidance, not engineering work items.",
		ActorRole:         identity.RoleEventActor,
		IdentityNamespace: func(string) string { return "zendesk_user" },
	}
}

// Run holds the sync worker (tickets, comments, Help Center articles, the
// group purge, and the reconcile) and, while Index documents is on, a
// document worker for Zendesk attachments. It lends its attachment fetcher
// to the caption worker.
func (c Connector) Run(ctx context.Context, deps source.Deps, resolved settings.Resolved) error {
	s := SettingsFrom(resolved)
	client, err := c.NewClient(resolved)
	if err != nil {
		return err
	}
	attachments := NewFileFetcher(deps.Pool, client)
	release := deps.Files.LendImages(ctx, attachments)
	defer release()

	sup := worker.NewSupervisor(deps.Logger, deps.Metrics)
	syncWorker := &Worker{
		Pool: deps.Pool, Client: client, Sink: deps.Sink, Logger: deps.Logger.With("component", "zendesk-sync"),
		RecentLookback: RecentLookback, IndexedGroups: s.IndexedGroups, State: deps.State, Observer: deps.Metrics,
	}
	sup.AddFunc("zendesk-sync", syncWorker.Run)
	if s.DocIngest {
		closeDocuments := deps.Files.AddDocumentWorker(sup, "zendesk-doc-extract", documentGate{}, attachments)
		defer closeDocuments()
	}
	deps.Logger.Info("zendesk connector running", "subdomain", s.Subdomain,
		"indexed_groups", len(s.IndexedGroups), "doc_ingest", s.DocIngest)
	sup.Run(ctx)
	return nil
}

// documentGate is the per-channel opt-in of Zendesk's document worker.
// Zendesk documents have no channel, so the worker never asks it.
type documentGate struct{}

func (documentGate) DocIngestEnabled(context.Context, string) (bool, error) { return false, nil }

// Expand returns the indexed conversation or article the event belongs to,
// with its processed attachment content, as far as scopes may read it.
func (Connector) Expand(ctx context.Context, deps source.Deps, eventID int64, scopes []string) (source.Expanded, error) {
	content, truncated, err := conversation(ctx, deps.Pool, eventID, scopes)
	return source.Expanded{Content: content, Truncated: truncated}, err
}
