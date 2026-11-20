package ask

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
)

// GatewayCheck verifies that the configured model gateway target can
// complete a real, deliberately tiny inference call, and caches the verdict
// for the readiness probe. When Ask is enabled the pod must not report ready
// (and must stop reporting ready after a regression) until the gateway
// succeeds, so a broken diderot + gateway pairing cannot take traffic.
//
// The probe reuses the same model-profile factory that Ask turns run through,
// so protocol drift at the request boundary fails the probe with the same
// adapter a real turn uses. It uses the smallest output budget accepted by
// the Responses API, no tools, a short
// explicit timeout, and the lowest effort supported by the model. A failed check keeps
// readiness at 503 with a safe diagnostic in logs.
//
// Checks run on an interval, not on every kubelet probe, so per-probe cost is
// zero and gateway load stays bounded. The first check runs immediately at
// startup, so the pod can never report ready before at least one success.
type GatewayCheck struct {
	logger   *slog.Logger
	model    *ConfiguredModel
	timeout  time.Duration
	interval time.Duration
	probeSID string
	metrics  *gatewayCheckMetrics

	checkMu sync.Mutex
	mu      sync.RWMutex
	checked bool
	ready   bool
	lastErr error
	lastOK  time.Time

	hookMu sync.Mutex
	hook   func(ready bool, checkErr error)
}

// GatewayCheckOptions configures the probe. Timeout defaults to 5s and
// Interval to 30s when zero; both must otherwise be positive.
type GatewayCheckOptions struct {
	BaseURL string
	APIKey  string
	// HTTPClient is the provider's client: it carries the provider's extra
	// headers. Nil is the default.
	HTTPClient  *http.Client
	Auth        llmclient.AuthStyle
	Model       modelregistry.Model
	ServiceTier string
	Timeout     time.Duration
	Interval    time.Duration
	Logger      *slog.Logger
	Registerer  prometheus.Registerer
}

const (
	gatewayCheckDefaultTimeout  = 5 * time.Second
	gatewayCheckDefaultInterval = 30 * time.Second
	// OpenAI Responses rejects max_output_tokens below 16. Keep the probe at
	// that protocol minimum; models may still complete after emitting one token.
	gatewayCheckMaxTokens = 16
)

// NewGatewayCheck constructs a checker with the same model-selected adapter
// and protocol the Ask path uses. The probe uses the registry's first (lowest)
// effort, or the provider default when the entry lists none, to keep its
// minimal output limit legal and cheap. Construction performs no network
// activity.
func NewGatewayCheck(ctx context.Context, opts GatewayCheckOptions) (*GatewayCheck, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = gatewayCheckDefaultTimeout
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = gatewayCheckDefaultInterval
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	effort := ""
	if len(opts.Model.ReasoningEfforts) > 0 {
		effort = opts.Model.ReasoningEfforts[0]
	}
	model, err := NewModel(ctx, ModelConfig{
		BaseURL:         opts.BaseURL,
		APIKey:          opts.APIKey,
		HTTPClient:      opts.HTTPClient,
		Auth:            opts.Auth,
		Model:           opts.Model,
		Timeout:         timeout,
		MaxTokens:       gatewayCheckMaxTokens,
		ReasoningEffort: effort,
		ServiceTier:     opts.ServiceTier,
	})
	if err != nil {
		return nil, fmt.Errorf("ask gateway readiness: build probe model: %w", err)
	}
	return &GatewayCheck{
		logger:   logger,
		model:    model,
		timeout:  timeout,
		interval: interval,
		probeSID: uuid.NewString(),
		metrics:  newGatewayCheckMetrics(opts.Registerer),
	}, nil
}

// SetUpdateHook registers fn to be called after every completed check with
// the new cached verdict. Only the last registered hook is used.
func (g *GatewayCheck) SetUpdateHook(fn func(ready bool, checkErr error)) {
	g.hookMu.Lock()
	defer g.hookMu.Unlock()
	g.hook = fn
}

// Ready reports whether the most recent completed check succeeded. It is
// false until at least one check has completed.
func (g *GatewayCheck) Ready() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.checked && g.ready
}

// LastError returns the error from the most recent failed check, or nil when
// the last check succeeded or none has run. Errors never include the API key
// or the probe prompt, so they are safe to log and to expose in diagnostics.
func (g *GatewayCheck) LastError() error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.lastErr
}

// LastSuccess returns when the last successful check completed.
func (g *GatewayCheck) LastSuccess() time.Time {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.lastOK
}

// CheckOnce performs a single logical probe, caches the verdict, and invokes
// the update hook. It is safe for concurrent use; calls are serialized so an
// older completion cannot overwrite a newer verdict or hook update.
func (g *GatewayCheck) CheckOnce(ctx context.Context) error {
	g.checkMu.Lock()
	defer g.checkMu.Unlock()

	g.mu.Lock()
	wasReady := g.ready
	everChecked := g.checked
	g.mu.Unlock()

	err := g.probe(ctx)
	g.mu.Lock()
	g.checked = true
	g.ready = err == nil
	if err == nil {
		g.lastOK = time.Now()
	}
	g.lastErr = err
	g.mu.Unlock()

	if err == nil {
		g.metrics.recordSuccess()
	} else {
		g.metrics.recordError()
	}

	g.hookMu.Lock()
	hook := g.hook
	g.hookMu.Unlock()
	if hook != nil {
		hook(err == nil, err)
	}

	// A failing probe used to be visible only as a 503 and a counter, which
	// makes "the pod never went ready" a guessing game — the reason belongs in
	// the log. Warn on the transition rather than every interval so a long
	// outage does not flood, and log recovery so the pair brackets the window.
	switch {
	case err != nil && wasReady:
		g.logger.Warn("ask gateway readiness check failing; /readyz will report 503", "err", err)
	case err != nil && !everChecked:
		g.logger.Warn("ask gateway readiness check failed on first attempt; /readyz will report 503", "err", err)
	case err == nil && everChecked && !wasReady:
		g.logger.Info("ask gateway readiness check recovered")
	}
	g.logger.Debug("ask gateway readiness check", "ok", err == nil, "err", err)
	return err
}

// Run checks immediately, then re-checks on the configured interval until ctx
// is cancelled. The immediate check guarantees startup cannot report ready
// before at least one successful inference.
func (g *GatewayCheck) Run(ctx context.Context) {
	_ = g.CheckOnce(ctx)
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = g.CheckOnce(ctx)
		}
	}
}

// probe runs one real inference through the selected native protocol with a
// minimal one-token request. A parsed response proves the gateway accepted the
// request shape and the configured target completed it.
func (g *GatewayCheck) probe(ctx context.Context) error {
	// Identity rides the same bounded x-agent-parent-session-id header the ask
	// path uses (eino_metadata.go); the gateway rejects a non-empty Responses
	// metadata body, and there is no provider-invocation attribution header.
	in := &schema.AgenticMessage{
		Role: schema.AgenticRoleTypeUser,
		ContentBlocks: []*schema.ContentBlock{{
			Type:          schema.ContentBlockTypeUserInputText,
			UserInputText: &schema.UserInputText{Text: "ping"},
		}},
	}
	// The SDK timeout applies to each retry attempt. Bound the whole logical
	// probe as well so retries and backoff cannot exceed the readiness budget.
	probeCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	out, err := g.model.Generate(probeCtx, []*schema.AgenticMessage{in},
		g.model.WithCustomHeaders(map[string]string{
			"x-agent-parent-session-id": g.probeSID,
		}))
	if err != nil {
		return fmt.Errorf("ask gateway readiness: inference failed: %w", err)
	}
	if out == nil {
		return errors.New("ask gateway readiness: gateway returned no response")
	}
	return nil
}

// gatewayCheckMetrics mirrors the worker-package liveness pattern: a
// success/error counter plus a last-success timestamp gauge, so an alert
// like `time() - diderot_ask_gateway_last_success_timestamp_seconds > 120`
// fires when the gateway stops passing readiness.
type gatewayCheckMetrics struct {
	checksTotal *prometheus.CounterVec
	lastSuccess prometheus.Gauge
}

func newGatewayCheckMetrics(reg prometheus.Registerer) *gatewayCheckMetrics {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	return &gatewayCheckMetrics{
		checksTotal: regCounterVec(reg, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "diderot_ask_gateway_check_total",
			Help: "Model gateway readiness probe count by outcome (success/error).",
		}, []string{"outcome"})),
		lastSuccess: regGauge(reg, prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "diderot_ask_gateway_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last successful model gateway readiness probe. Alert on staleness to catch a broken gateway before it takes ask traffic.",
		})),
	}
}

func (m *gatewayCheckMetrics) recordSuccess() {
	m.checksTotal.WithLabelValues("success").Inc()
	m.lastSuccess.Set(float64(time.Now().Unix()))
}

func (m *gatewayCheckMetrics) recordError() {
	m.checksTotal.WithLabelValues("error").Inc()
}

func regCounterVec(reg prometheus.Registerer, v *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(*prometheus.CounterVec)
		}
		panic(err)
	}
	return v
}

func regGauge(reg prometheus.Registerer, v prometheus.Gauge) prometheus.Gauge {
	if err := reg.Register(v); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			return already.ExistingCollector.(prometheus.Gauge)
		}
		panic(err)
	}
	return v
}
