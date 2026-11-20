package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/httpserver"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/worker"
)

// vectorStores opens the vector collection of each embedding choice once
// and keeps it open: a replica follows the saved choice, and a change opens
// the new collection beside the old one.
type vectorStores struct {
	cfg    *config.Config
	logger *slog.Logger

	mu     sync.Mutex
	stores map[string]*qdrantstore.Store
}

func newVectorStores(cfg *config.Config, logger *slog.Logger) *vectorStores {
	return &vectorStores{cfg: cfg, logger: logger, stores: map[string]*qdrantstore.Store{}}
}

// open returns the store of embedding's collection, creating the collection
// or checking the one that exists holds vectors of its dimension.
func (v *vectorStores) open(ctx context.Context, embedding settings.Embedding) (*qdrantstore.Store, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if store, ok := v.stores[embedding.Collection]; ok && store.Dimensions() == embedding.Dimensions {
		return store, nil
	}
	store, err := newQdrantStore(ctx, v.cfg, embedding.Collection, embedding.Dimensions, prometheus.DefaultRegisterer, v.logger)
	if err != nil {
		return nil, err
	}
	if err := store.EnsureCollection(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}
	v.logger.Info("qdrant collection ready", "collection", store.Collection(), "dimensions", embedding.Dimensions, "tls", v.cfg.QdrantTLS)
	if old, ok := v.stores[embedding.Collection]; ok {
		_ = old.Close()
	}
	v.stores[embedding.Collection] = store
	return store, nil
}

// exists reports whether the collection is already in Qdrant, for adopting
// an existing one.
func (v *vectorStores) exists(ctx context.Context, collection string, dimensions int) (bool, error) {
	store, err := newQdrantStore(ctx, v.cfg, collection, dimensions, prometheus.NewRegistry(), v.logger)
	if err != nil {
		return false, err
	}
	defer func() { _ = store.Close() }()
	return store.CollectionExists(ctx)
}

func (v *vectorStores) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for name, store := range v.stores {
		_ = store.Close()
		delete(v.stores, name)
	}
}

// vectorRetry is how long a replica waits before opening its collection
// again after Qdrant failed to.
const vectorRetry = 15 * time.Second

// followEmbedding gives retriever the saved embedding model and its
// collection, and again whenever the choice changes, until ctx ends. Until
// a model is chosen search answers that it is not set up. A chosen model
// whose collection can't be opened leaves search off and the replica not
// ready, with the reason, until it opens.
func followEmbedding(ctx context.Context, logger *slog.Logger, catalog *modelCatalog, liveModels func() *inferenceModels,
	vectors *vectorStores, retriever *retrieve.Retriever, readiness *httpserver.Readiness) {
	opened := func() {
		catalog.searchFault.Store(nil)
		readiness.SetReady(readySearch, true)
	}
	runPerModels(ctx, logger, "search", liveModels, sameEmbedding, func(termCtx context.Context, models *inferenceModels) {
		if models.embed.ID == "" {
			retriever.SetVectors(nil, nil)
			opened()
			<-termCtx.Done()
			return
		}
		for termCtx.Err() == nil {
			store, err := vectors.open(termCtx, models.embedding)
			if err == nil {
				emb, embErr := newEmbedder(models.embed, models.embedding.Dimensions)
				if err = embErr; err == nil {
					retriever.SetVectors(emb, store)
					opened()
					logger.Info("search uses the embedding model", "model", models.embed.ID, "collection", store.Collection())
					<-termCtx.Done()
					return
				}
			}
			retriever.SetVectors(nil, nil)
			if termCtx.Err() != nil {
				return
			}
			reason := "Search is off: the collection " + models.embedding.Collection + " of the embedding model can't be opened (" + vectorFault(err) + ")."
			catalog.searchFault.Store(&reason)
			readiness.SetNotReady(readySearch, reason)
			logger.Error("search is off: its vector collection could not be opened; retrying", "collection", models.embedding.Collection, "err", err)
			select {
			case <-termCtx.Done():
			case <-time.After(vectorRetry):
			}
		}
	})
}

// vectorFault is err in words for a page and a probe: Qdrant's own text for
// a collection of the wrong shape, and no address or credential.
func vectorFault(err error) string {
	var shape *qdrantstore.ShapeError
	if errors.As(err, &shape) {
		return "it exists with another shape than this deployment needs: " + strings.Join(shape.Mismatches, "; ")
	}
	return "the vector store did not answer, or refused"
}

// fenceBatch is how many chunks one statement marks for the collection
// being filled.
const fenceBatch = 5000

// fenceFilling marks, for the embedder, every chunk whose vector is not in
// collection: one embedded into another collection, or before chunks
// recorded theirs. It runs at the start of each leader term that fills
// collection, after the previous term's embedder has stopped, so a chunk an
// earlier leader embedded elsewhere meanwhile is caught by the next; a term
// that finds none changes nothing. It works in batches, so no one statement
// holds the whole table.
func fenceFilling(ctx context.Context, pool *pgxpool.Pool, collection string, logger *slog.Logger) error {
	var marked int64
	for {
		tag, err := pool.Exec(ctx, `
			UPDATE event_chunks SET embedded_at = NULL, embedder_id = NULL, queued_at = now()
			 WHERE id IN (SELECT id FROM event_chunks
			               WHERE embedded_at IS NOT NULL AND embedded_collection IS DISTINCT FROM $1
			               LIMIT $2)`, collection, fenceBatch)
		if err != nil {
			return fmt.Errorf("fill %s: mark chunks: %w", collection, err)
		}
		marked += tag.RowsAffected()
		if tag.RowsAffected() < fenceBatch {
			break
		}
	}
	if marked > 0 {
		logger.Info("a new embedding collection is being filled: chunks not yet in it are embedded into it",
			"collection", collection, "chunks", marked)
	}
	return nil
}

// fillCheckInterval is how often the leader checks whether the collection
// being filled holds every chunk.
const fillCheckInterval = 30 * time.Second

// finishFilling makes the embedding model being filled the one search uses,
// once its collection holds every chunk the embedder can embed: none waits,
// and none is in another collection. Chunks the embedder gave up on do not
// hold it back. The save restarts the leader's work and moves every
// replica's search to the collection.
func finishFilling(store *settings.Store, pool *pgxpool.Pool, filling settings.Embedding, logger *slog.Logger) worker.Runner {
	return worker.Runner{
		Name: "embedding-fill", Interval: fillCheckInterval, Logger: logger.With("component", "embedding-fill"),
		Run: func(ctx context.Context) error {
			var waiting bool
			if err := pool.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM event_chunks c JOIN events e ON e.id = c.event_id
				                WHERE NOT e.deleted AND c.failure_count < 5
				                  AND (c.embedded_at IS NULL OR c.embedded_collection IS DISTINCT FROM $1))`,
				filling.Collection).Scan(&waiting); err != nil {
				return fmt.Errorf("fill %s: count waiting chunks: %w", filling.Collection, err)
			}
			if waiting {
				return nil
			}
			row, err := store.ReadModels(ctx)
			if err != nil {
				return err
			}
			if row.Filling == nil || *row.Filling != filling {
				return nil
			}
			_, err = store.UpdateModels(ctx, row.Version, settings.Actor{Email: checkActor}, "setup.models.embedding.filled", func(models *settings.Models) error {
				models.Embedding, models.Filling = models.Filling, nil
				return nil
			})
			if errors.Is(err, settings.ErrConflict) {
				return nil
			}
			if err == nil {
				logger.Info("the new embedding collection holds every chunk: search uses it from now on",
					"model", filling.Model, "collection", filling.Collection)
			}
			return err
		},
	}
}
