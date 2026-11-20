package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/dbconn"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/scopesync"
	"github.com/bluesky-social/diderot/internal/settings"
)

// qdrantAuditCmd is an explicit, in-pod cutover operation. It is not wired
// into server startup, so ordinary restarts never scan the collection.
func qdrantAuditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "qdrant-audit",
		Short: "Compare the Qdrant collection with the database and queue repairs",
		Long: `Reads the whole Qdrant collection and compares it with the database. It
queues a delete for each point no chunk owns, a payload fix for each point
whose scope is wrong, and an embedding for each chunk the collection lacks,
such as every chunk after a change of embedding model. The server's workers
apply the queues.

Run it where the server runs, with the server's configuration, once an
embedding model is chosen on the Models page, which creates its collection.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			pool, err := dbconn.NewPool(cmd.Context(), dbconn.Options{URL: cfg.DBURL, IAMAuth: cfg.DBIAMAuth})
			if err != nil {
				return fmt.Errorf("connect Postgres: %w", err)
			}
			defer pool.Close()
			logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
			models, err := settings.NewStore(pool, logger).ReadModels(cmd.Context())
			if err != nil {
				return err
			}
			if models.Embedding == nil {
				return errors.New("no embedding model is chosen yet: choose one on the Models page, which creates its collection, then run this again")
			}
			store, err := newQdrantStore(cmd.Context(), cfg, models.Embedding.Collection, models.Embedding.Dimensions, prometheus.NewRegistry(), logger)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			if err := requireQdrantCollection(cmd.Context(), store); err != nil {
				return err
			}
			service := &scopesync.Service{
				Pool:   pool,
				Store:  store,
				Logger: logger,
			}
			result, err := service.Audit(cmd.Context())
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		},
	}
}

// requireQdrantCollection refuses an audit of a collection that does not
// exist.
func requireQdrantCollection(ctx context.Context, store *qdrantstore.Store) error {
	exists, err := store.CollectionExists(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("the Qdrant collection %s doesn't exist: choose the embedding model again on the Models page to create it",
			store.Collection())
	}
	return nil
}
