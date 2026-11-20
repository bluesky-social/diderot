package main

import (
	"context"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/httpserver"
	"github.com/bluesky-social/diderot/internal/pagerduty"
	"github.com/bluesky-social/diderot/internal/settings"
)

// startReadiness is what /readyz reports. The database and Qdrant are
// ready once serve builds it. With Ask on, the pod is also not ready until
// one cheap real inference through Ask's model succeeds, and a later
// failure makes it not ready again: the gate checks on an interval, the
// first at once, until ctx ends, so a probe only reads the last verdict.
// With askBuilds, it is not ready either until the replica's Ask has been
// built once (startReplicaAsk marks "ask"), so a load balancer sends it no
// Ask while it connects Ask's MCP servers. /healthz ignores both, so a
// broken model provider never restarts the pod.
//
// It is not ready either while the models as saved can't be run (the
// "models" check, modelCatalog.faults), or while the embedding model's
// collection can't be opened (the "search" check, followEmbedding), so a
// rollout whose new pods come up unable to serve what the settings say
// stops, as one whose pods could not start did. A server nobody has set up
// yet has nothing saved and is ready, for its administrator to set up.
func startReadiness(ctx context.Context, cfg *config.Config, catalog *modelCatalog, askBuilds bool, logger *slog.Logger) *httpserver.Readiness {
	liveModels := catalog.live
	readiness := httpserver.NewReadiness()
	readiness.SetReady("db", true)
	// Not ready until followEmbedding has opened the collection, or found
	// there is none to open.
	readiness.SetNotReady(readySearch, "The embedding model's collection is not open yet.")
	gateModelsReadiness(catalog, readiness, logger)
	go func() {
		ticker := time.NewTicker(modelsWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				gateModelsReadiness(catalog, readiness, logger)
			}
		}
	}()
	if askBuilds {
		readiness.SetReady("ask", false)
	}
	if cfg.AskEnabled {
		// Not ready, rather than silently ready, until the first check.
		readiness.SetReady("gateway", false)
		go runPerModels(ctx, logger, "ask readiness", liveModels, sameAskModel, func(termCtx context.Context, models *inferenceModels) {
			gateAskReadiness(termCtx, cfg, models, readiness, logger)
		})
	}
	return readiness
}

// The readiness checks of the saved models.
const (
	readyModels = "models"
	readySearch = "search"
)

// gateModelsReadiness sets the "models" check from the saved models as this
// replica's settings cache holds them, and logs when what is wrong changes.
func gateModelsReadiness(catalog *modelCatalog, readiness *httpserver.Readiness, logger *slog.Logger) {
	var faults []string
	if catalog.saved != nil {
		faults = catalog.faults(catalog.saved())
	}
	reason := strings.Join(faults, " ")
	if reason == readiness.NotReady()[readyModels] {
		if reason == "" {
			readiness.SetReady(readyModels, true)
		}
		return
	}
	if reason == "" {
		readiness.SetReady(readyModels, true)
		logger.Info("the saved models can be run again; /readyz no longer fails on them")
		return
	}
	readiness.SetNotReady(readyModels, reason)
	logger.Error("the saved models can't be run as saved; /readyz answers 503 until this is fixed", "faults", faults)
}

// gateAskReadiness checks models' Ask model until ctx ends, which a newly
// saved Ask model does: the last verdict stands until the new model's first
// check. A check that cannot be built keeps readiness at 503 until the
// configuration is fixed. With no Ask model there is nothing to check: a
// fresh install is ready before anyone has chosen one, and Ask answers that
// it is not set up.
func gateAskReadiness(ctx context.Context, cfg *config.Config, models *inferenceModels, readiness *httpserver.Readiness, logger *slog.Logger) {
	if models.ask.ID == "" {
		readiness.SetReady("gateway", true)
		logger.Info("ask readiness: no Ask model is chosen, so there is nothing to check")
		<-ctx.Done()
		return
	}
	gatewayCheck, err := gatewayReadinessCheck(ctx, cfg, models, logger)
	if err != nil {
		readiness.SetReady("gateway", false)
		logger.Error("ask gateway readiness gate failed to initialize; /readyz will stay 503",
			"model", models.ask.ID, "err", err)
		<-ctx.Done()
		return
	}
	gatewayCheck.SetUpdateHook(func(ready bool, _ error) {
		// A check cut short by a model change says nothing.
		if ctx.Err() == nil {
			readiness.SetReady("gateway", ready)
		}
	})
	logger.Info("ask gateway readiness gate enabled", "model", models.ask.ID,
		"interval", cfg.AskReadinessInterval,
		"timeout", cfg.AskReadinessTimeout)
	gatewayCheck.Run(ctx)
}

// newListeners builds serve's HTTP servers, in listenerNames order: the
// public listener over api, the internal listener over api without the
// browser routes, the PagerDuty webhook, and the metrics listener.
func newListeners(addrs []string, api, webhook http.Handler) []*http.Server {
	return []*http.Server{
		{Addr: addrs[0], Handler: api, ReadHeaderTimeout: 5 * time.Second},
		{Addr: addrs[1], Handler: internalServiceHandler(api), ReadHeaderTimeout: 5 * time.Second},
		{
			Addr: addrs[2], Handler: webhook, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute,
		},
		{Addr: addrs[3], Handler: metricsHandler(), ReadHeaderTimeout: 5 * time.Second},
	}
}

// pagerDutyWebhook answers 404 while the PagerDuty connector is disabled,
// and checks each signature with the secrets cached when the request comes.
func pagerDutyWebhook(store *settings.Store, pool *pgxpool.Pool, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(pagerduty.WebhookPath, &pagerduty.Webhook{
		Secrets: func() ([]string, bool) {
			resolved, ok := store.Connector(pagerduty.ConnectorName)
			if !ok {
				return nil, false
			}
			return pagerduty.SettingsFrom(resolved).WebhookSecrets, true
		},
		Inbox: pagerduty.NewStore(pool), Logger: logger.With("component", "pagerduty-webhook"),
	})
	return mux
}

// startListeners serves each of servers in the background. The returned
// channel receives each one's result: the error that stopped it, or nil
// once it is shut down.
func startListeners(servers []*http.Server, logger *slog.Logger) <-chan error {
	result := make(chan error, len(servers))
	for i, server := range servers {
		go serveHTTP(server, listenerNames[i], logger, result)
	}
	return result
}

// shutDownListeners stops every listener at once and waits up to grace for
// the requests in progress. Then it closes the connections still open,
// which cancels their requests.
func shutDownListeners(servers []*http.Server, grace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	var shutting sync.WaitGroup
	for _, server := range servers {
		shutting.Go(func() {
			if server.Shutdown(ctx) != nil {
				_ = server.Close()
			}
		})
	}
	shutting.Wait()
}

func serveHTTP(server *http.Server, name string, logger *slog.Logger, result chan<- error) {
	logger.Info("http server listening", "name", name, "addr", server.Addr)
	err := server.ListenAndServe()
	if err == http.ErrServerClosed {
		err = nil
	}
	result <- err
}

// probeMux is where the API listeners' routes start: the probes. Metrics
// and pprof are never on it, only on the metrics listener, so nothing that
// reaches the API can read them.
func probeMux(readiness http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/healthz", httpserver.HealthzHandler())
	mux.Handle("/readyz", readiness)
	return mux
}

// metricsHandler serves the metrics listener: Prometheus metrics and pprof,
// which net/http/pprof registers on http.DefaultServeMux.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/debug/pprof/", http.DefaultServeMux)
	return mux
}

// internalServiceHandler is the machine listener's routes: the
// token-authenticated machine APIs, MCP, and the probes. It answers 404 for
// the browser routes and the built-in sign-in before reading any identity
// header, so machine callers sent to this listener, on a network the
// public listener's proxy does not guard, cannot forge a browser identity.
func internalServiceHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleanPath := path.Clean(r.URL.Path)
		if cleanPath == "/" || cleanPath == "/admin" || strings.HasPrefix(cleanPath, "/admin/") ||
			cleanPath == "/sessions" || strings.HasPrefix(cleanPath, "/sessions/") ||
			cleanPath == "/auth" || strings.HasPrefix(cleanPath, "/auth/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// gatewayReadinessCheck constructs the Ask endpoint readiness probe, or
// (nil, nil) when Ask is disabled (readiness then gates on the same
// dependencies as before). Only the model and endpoint resolved when
// DIDEROT_ASK_ENABLED=true are used, so this never probes an unconfigured
// endpoint. The returned error means the probe could not be built; callers
// treat that as fail-closed (readiness stuck at 503).
func gatewayReadinessCheck(ctx context.Context, cfg *config.Config, models *inferenceModels, logger *slog.Logger) (*ask.GatewayCheck, error) {
	if !cfg.AskEnabled {
		return nil, nil
	}
	check, err := ask.NewGatewayCheck(ctx, ask.GatewayCheckOptions{
		BaseURL:     models.ask.endpoint.BaseURL,
		APIKey:      models.ask.endpoint.LibraryKey(),
		HTTPClient:  models.ask.endpoint.HTTPClient(nil),
		Auth:        models.ask.endpoint.Auth,
		Model:       models.ask.Model,
		ServiceTier: models.askLimits.ServiceTier,
		Timeout:     cfg.AskReadinessTimeout,
		Interval:    cfg.AskReadinessInterval,
		Logger:      logger.With("component", "ask-readiness"),
		Registerer:  prometheus.DefaultRegisterer,
	})
	if err != nil {
		return nil, err
	}
	return check, nil
}
