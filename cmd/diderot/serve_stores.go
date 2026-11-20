package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/attachments/files"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/dbconn"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/migrations"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// openDatabase applies pending schema migrations, then opens the pool every
// request and worker on this replica shares.
func openDatabase(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	opts := dbconn.Options{URL: cfg.DBURL, IAMAuth: cfg.DBIAMAuth}
	if err := applyMigrations(ctx, opts, logger); err != nil {
		return nil, err
	}
	return openPool(ctx, opts, logger)
}

// applyMigrations runs Atlas in-process. Replicas starting together
// serialize on Atlas's Postgres advisory lock, so every replica runs it.
// The 2-minute ceiling bounds a stuck lock; a DDL change that needs longer
// gets its own window through a separate Atlas run.
func applyMigrations(ctx context.Context, opts dbconn.Options, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	url, err := dbconn.MigrationURL(ctx, opts)
	if err != nil {
		logger.Error("DB credentials for migrations", "err", err)
		return err
	}
	if _, err := migrations.Apply(ctx, url, logger); err != nil {
		logger.Error("migrations failed", "err", err)
		return err
	}
	return nil
}

func openPool(ctx context.Context, opts dbconn.Options, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := dbconn.PoolConfig(ctx, opts)
	if err != nil {
		logger.Error("parse DB url", "err", err)
		return nil, err
	}
	// Sized for the embedder's writebacks, the Slack consumer, API handlers,
	// and up to 16 caption workers (DIDEROT_CAPTION_WORKERS) at once.
	poolCfg.MaxConns = 32
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		logger.Error("connect DB", "err", err)
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		logger.Error("ping DB", "err", err)
		pool.Close()
		return nil, err
	}
	logger.Info("db connected")
	return pool, nil
}

// preparePrompts seeds the prompt catalog on a database that has no
// release, applies the database's active release synchronously (seeding
// already reads it; EnsureSeed's result is always the release active at
// that moment), starts the watcher for its ticking loop only -- it reads
// the database no earlier than its first tick -- and installs the
// variables prompts fill from the settings cache, the sources, and the
// Grafana datasources the investigator's tools are pinned to.
//
// The active release must be applied before this returns: every other
// required store this replica depends on (the database pool itself,
// connectors, server auth, the embedding model, admin, the browser UI, the
// corpus MCP server) fails startup on its first read's error rather than
// start serving on a stand-in, and prompts follow that same rule instead
// of being the odd one out. A failure here is at least as consequential:
// unlike those, silently falling back here does not even surface as an
// error, it serves the generic example catalog to session titles, document
// extraction, image captions, and scheduled-report templates.
func preparePrompts(ctx context.Context, pool *pgxpool.Pool, store *settings.Store, sources source.Registry, grafana func() string, logger *slog.Logger) (*promptcatalog.Store, error) {
	embedded, err := promptcatalog.Embedded()
	if err != nil {
		return nil, fmt.Errorf("load embedded prompt catalog: %w", err)
	}
	releases := promptcatalog.NewStore(pool)
	active, err := releases.EnsureSeed(ctx, embedded, "embedded:"+embedded.SHA256)
	if err != nil {
		return nil, fmt.Errorf("seed prompt releases: %w", err)
	}
	if err := promptcatalog.SetActive(active.Bundle); err != nil {
		return nil, fmt.Errorf("activate prompt release %d: %w", active.ID, err)
	}
	logger.Info("prompt release store ready", "embedded_bundle_sha256", embedded.SHA256,
		"active_release_id", active.ID, "active_bundle_sha256", active.Bundle.SHA256)
	go promptcatalog.RunWatcher(ctx, releases, active.ID, 10*time.Second, logger.With("component", "prompts"))
	sourcesPrompt := sources.Infos().PromptList()
	promptcatalog.SetVariables(func() map[string]string {
		values := store.PromptValues()
		org := store.Org().Org
		values[promptcatalog.VarOrgName], values[promptcatalog.VarOrgDescription] = org.Name, org.Description
		values[promptcatalog.VarSources] = sourcesPrompt
		values[promptcatalog.VarGrafanaDatasources] = grafana()
		return values
	})
	return releases, nil
}

// newQdrantStore builds the Qdrant client of one collection from the
// server's configuration, for every command that reaches Qdrant through it
// (serve, qdrant-audit), and checks that Qdrant answers.
func newQdrantStore(ctx context.Context, cfg *config.Config, collection string, dimensions int, reg prometheus.Registerer, logger *slog.Logger) (*qdrantstore.Store, error) {
	if qdrantKeyInPlaintext(cfg) {
		logger.Warn("DIDEROT_QDRANT_API_KEY crosses the network in plaintext; set DIDEROT_QDRANT_TLS=true once Qdrant serves TLS",
			"addr", cfg.QdrantAddr)
	}
	store, err := qdrantstore.New(qdrantstore.Config{
		Addr:              cfg.QdrantAddr,
		APIKey:            cfg.QdrantAPIKey,
		UseTLS:            cfg.QdrantTLS,
		RootCAs:           cfg.QdrantRootCAs,
		Collection:        collection,
		Dimensions:        uint64(dimensions),
		ShardNumber:       cfg.QdrantShardNumber,
		ReplicationFactor: cfg.QdrantReplicationFactor,
	}, reg)
	if err != nil {
		return nil, err
	}
	// A Qdrant that accepts the connection and never answers would otherwise
	// hold startup forever.
	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := store.Ping(pingCtx); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// qdrantKeyInPlaintext reports whether the Qdrant API key goes without TLS
// to a host other than localhost or a loopback IP.
func qdrantKeyInPlaintext(cfg *config.Config) bool {
	if cfg.QdrantAPIKey == "" || cfg.QdrantTLS {
		return false
	}
	host, _, err := net.SplitHostPort(cfg.QdrantAddr)
	if err != nil {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return host != "localhost" && (err != nil || !ip.IsLoopback())
}

// attachFileStorage opens the object storage that mirrors attachments and
// gives it to admin, which serves the admin pages' images from it on every
// replica; the leader's caption and document workers write to it.
// It is nil while DIDEROT_IMAGES_BUCKET is unset; a bucket that is set but
// cannot be opened fails startup.
func attachFileStorage(ctx context.Context, cfg *config.Config, admin *adminapi.Server, logger *slog.Logger) (files.S3Mirror, error) {
	if cfg.ImagesBucket == "" {
		return nil, nil
	}
	mirror, err := files.NewS3Mirror(ctx, files.S3MirrorConfig{
		Bucket:    cfg.ImagesBucket,
		Region:    cfg.ImagesRegion,
		Endpoint:  cfg.ImagesEndpoint,
		PathStyle: cfg.ImagesPathStyle,
	})
	if err != nil {
		logger.Error("files: s3 mirror init failed", "err", err)
		return nil, fmt.Errorf("object storage: %w", err)
	}
	admin.SetFileBytesGetter(mirror)
	logger.Info("attachment file storage attached", "bucket", cfg.ImagesBucket)
	return mirror, nil
}

// corpusReaders is what every replica's corpus surfaces (the API's
// retrieve, list and expand, and the corpus MCP server) and the leader's
// Ask tools read the corpus through.
type corpusReaders struct {
	// sink is the event store. It also answers each event's scope, which
	// expansion checks before any source reads the event.
	sink *events.DBSink
	// retriever searches once an embedding model is chosen
	// (followEmbedding); until then it answers that search is not set up.
	retriever *retrieve.Retriever
	// expander hands each event to its source's Expand.
	expander *sourceExpander
	// references resolves the explicit handles in a question.
	references *sourceReferenceResolver
	// linearTeams makes a bare issue key (ABC-123) an exact reference only
	// for the Linear teams the corpus holds. There is one, so Ask and MCP
	// retrieve parse references alike.
	linearTeams *ask.LinearTeamCache
}

func newCorpusReaders(pool *pgxpool.Pool, sink *events.DBSink, sources source.Registry, logger *slog.Logger) (corpusReaders, error) {
	linearTeams, err := ask.NewLinearTeamCache(sink, logger.With("component", "linear-teams"))
	if err != nil {
		return corpusReaders{}, fmt.Errorf("initialize linear team keys: %w", err)
	}
	expander := newSourceExpander(sources, source.Deps{Pool: pool, Sink: sink, Logger: logger.With("component", "expand")})
	retriever := retrieve.NewRetriever(pool, nil, nil, logger.With("component", "retrieve"))
	retriever.SetChannelFinder(sourceChannels{sources: sources, deps: source.Deps{Pool: pool, Sink: sink, Logger: logger.With("component", "channels")}})
	return corpusReaders{
		sink:        sink,
		retriever:   retriever,
		expander:    expander,
		references:  newSourceReferenceResolver(sink, expander),
		linearTeams: linearTeams,
	}, nil
}
