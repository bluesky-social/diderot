package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/adminapi"
	"github.com/bluesky-social/diderot/internal/adminui"
	"github.com/bluesky-social/diderot/internal/httpserver"
	"github.com/bluesky-social/diderot/internal/migrations"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/retrieve"
	"github.com/bluesky-social/diderot/internal/sessiondev"
	"github.com/bluesky-social/diderot/internal/sessionfixture"
	"github.com/bluesky-social/diderot/internal/sessioningest"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
)

const defaultDiderotSessionDevDB = "postgres://diderot:diderot@127.0.0.1:15432/diderot_session_dev?sslmode=disable"

const sessionDevTokenRefreshInterval = 5 * time.Minute

func sessionDevCmd() *cobra.Command {
	var bundlePath, databaseURL, listen, viewerURL, persona, runtimeDirectory, qdrantAddress, qdrantCollection string
	cmd := &cobra.Command{
		Use:   "session-dev",
		Short: "run the focused local agent-session server",
		// A tool for developing Diderot, not for running it.
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSessionDev(cmd.Context(), sessionDevOptions{
				BundlePath: bundlePath, DatabaseURL: databaseURL, Listen: listen,
				ViewerURL: viewerURL, Persona: persona, RuntimeDirectory: runtimeDirectory,
				QdrantAddress: qdrantAddress, QdrantCollection: qdrantCollection,
			})
		},
	}
	cmd.Flags().StringVar(&bundlePath, "bundle", "internal/sessionfixture/testdata/synthetic-v1", "validated session fixture bundle")
	cmd.Flags().StringVar(&databaseURL, "db-url", defaultDiderotSessionDevDB, "dedicated Postgres URL ending in _session_dev")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8082", "literal loopback listen address")
	cmd.Flags().StringVar(&viewerURL, "session-viewer-url", "http://127.0.0.1:8083", "local session viewer origin")
	cmd.Flags().StringVar(&persona, "as", "", "manifest persona to inject (defaults to the first)")
	cmd.Flags().StringVar(&runtimeDirectory, "runtime-dir", "", "directory for the short-lived local bearer token")
	cmd.Flags().StringVar(&qdrantAddress, "qdrant-addr", "127.0.0.1:16334", "dedicated loopback Qdrant address")
	cmd.Flags().StringVar(&qdrantCollection, "qdrant-collection", "diderot_session_dev", "resettable collection ending in _session_dev")
	return cmd
}

type sessionDevOptions struct {
	BundlePath       string
	DatabaseURL      string
	Listen           string
	ViewerURL        string
	Persona          string
	RuntimeDirectory string
	QdrantAddress    string
	QdrantCollection string
}

func runSessionDev(parent context.Context, options sessionDevOptions) error {
	if err := sessiondev.RequireDevDatabase(options.DatabaseURL); err != nil {
		return err
	}
	if err := sessiondev.RequireLoopbackAddress(options.QdrantAddress); err != nil {
		return err
	}
	if !strings.HasSuffix(options.QdrantCollection, "_session_dev") {
		return fmt.Errorf("session-dev Qdrant collection %q must end in _session_dev", options.QdrantCollection)
	}
	bundle, err := sessionfixture.Load(options.BundlePath)
	if err != nil {
		return err
	}
	persona, err := selectSessionDevPersona(bundle.Manifest.AllowedPersonas, options.Persona)
	if err != nil {
		return err
	}
	viewerURL, err := localHTTPOrigin(options.ViewerURL)
	if err != nil {
		return fmt.Errorf("session-dev session viewer URL: %w", err)
	}
	listener, publicOrigin, err := sessiondev.ListenLoopback(options.Listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	migrationContext, cancelMigrations := context.WithTimeout(parent, 2*time.Minute)
	_, err = migrations.Apply(migrationContext, options.DatabaseURL, logger)
	cancelMigrations()
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(parent, options.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(parent); err != nil {
		return err
	}
	if err := sessiondev.ImportDiderot(parent, pool, bundle, viewerURL); err != nil {
		return err
	}
	registry := prometheus.NewRegistry()
	vectorStore, err := qdrantstore.New(qdrantstore.Config{
		Addr: options.QdrantAddress, Collection: options.QdrantCollection,
		Dimensions: sessiondev.VectorDim,
	}, registry)
	if err != nil {
		return err
	}
	defer func() { _ = vectorStore.Close() }()
	// The suffix and literal-loopback guards above make this destructive reset
	// local and explicit. Recreate the collection so every run is deterministic.
	if err := vectorStore.EnsureCollection(parent); err != nil {
		return err
	}
	if err := vectorStore.Drop(parent); err != nil {
		return err
	}
	if err := vectorStore.EnsureCollection(parent); err != nil {
		return err
	}
	localEmbedder := sessiondev.DeterministicEmbedder{}
	if err := sessiondev.SeedQdrant(parent, pool, vectorStore, localEmbedder); err != nil {
		return err
	}
	identity, token, err := sessiondev.NewIdentity(persona, []string{"everyone"})
	if err != nil {
		return err
	}
	runtimeDirectory, removeRuntime, err := prepareSessionDevRuntime(options.RuntimeDirectory)
	if err != nil {
		return err
	}
	if removeRuntime {
		defer func() { _ = os.RemoveAll(runtimeDirectory) }()
	}
	tokenPath := filepath.Join(runtimeDirectory, "diderot-token")
	if err := sessiondev.WriteToken(tokenPath, token); err != nil {
		return fmt.Errorf("write session-dev token: %w", err)
	}
	defer func() { _ = os.Remove(tokenPath) }()

	privacy, err := sessioningest.NewPrivacyService(sessioningest.NewStore(pool), sessioningest.DefaultPolicy())
	if err != nil {
		return err
	}
	ui, err := adminui.New(adminui.Config{
		Pool: pool, Logger: logger.With("component", "session-ui"), Version: Version,
		AdminGroups: []string{"session-dev-admin"}, SelfServiceGroups: []string{"everyone"},
		PublicURL: publicOrigin, SessionViewerBaseURL: func() string { return viewerURL },
		SessionPrivacy: privacy, Registry: registry, AllowInsecureLoopbackCookies: true,
		Authenticator: identity.BrowserAuthenticator(),
	})
	if err != nil {
		return err
	}
	api := adminapi.NewServer(identity, logger.With("component", "session-api"))
	api.SetSelfServiceGroups([]string{"everyone"})
	api.SetSessionPrivacyService(privacy)
	api.SetScopeResolver(sessiondev.ScopeResolver{})
	api.SetRetriever(retrieve.NewRetriever(pool, localEmbedder, vectorStore, logger.With("component", "session-retrieve")))
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpserver.HealthzHandler())
	readiness := httpserver.NewReadiness()
	readiness.SetReady("db", true)
	mux.Handle("GET /readyz", readiness)
	if err := ui.MountSessions(mux); err != nil {
		return err
	}
	if err := api.MountSelfService(mux); err != nil {
		return err
	}
	if err := api.MountRetrieve(mux); err != nil {
		return err
	}

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errChannel := make(chan error, 1)
	go func() { errChannel <- server.Serve(listener) }()
	tokenTicker := time.NewTicker(sessionDevTokenRefreshInterval)
	defer tokenTicker.Stop()
	logger.Info("diderot session-dev ready", "origin", publicOrigin, "persona", persona,
		"bundle", options.BundlePath, "token_file", tokenPath, "viewer_origin", viewerURL)
	for {
		select {
		case <-parent.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return server.Shutdown(shutdownContext)
		case serveErr := <-errChannel:
			if errors.Is(serveErr, http.ErrServerClosed) {
				return nil
			}
			return serveErr
		case <-tokenTicker.C:
			if err := identity.RotateToken(tokenPath); err != nil {
				logger.Warn("rotate session-dev token failed; will retry", "err", err)
			}
		}
	}
}

func selectSessionDevPersona(allowed []string, requested string) (string, error) {
	if requested == "" && len(allowed) > 0 {
		return allowed[0], nil
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	for _, persona := range allowed {
		if requested == persona {
			return persona, nil
		}
	}
	return "", fmt.Errorf("session-dev persona %q is not declared by the bundle", requested)
}

func localHTTPOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("must be an HTTP origin")
	}
	hostIP := net.ParseIP(parsed.Hostname())
	if hostIP == nil || !hostIP.IsLoopback() {
		return "", errors.New("must use a literal loopback IP")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func prepareSessionDevRuntime(configured string) (string, bool, error) {
	if configured != "" {
		return configured, false, nil
	}
	directory, err := os.MkdirTemp("", "diderot-session-dev-")
	return directory, true, err
}
