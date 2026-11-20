package ask

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
)

const (
	ObservabilityRouterVersion = "o11y-router-v3"
	// ObservabilityBundleVersion bumped to v5: delegated Sentry investigations
	// now select a Sentry-specific workflow instead of inheriting Grafana query
	// syntax that may not be available to the child. The persisted bundle text is
	// now the delegated child's steering block (buildDelegatedSteering /
	// delegatedSteeringText in observability_investigate.go), not a root-loop
	// bundle that filtered tools and capped rounds. The root no longer builds
	// or persists steering itself; bump this again if the child's steering
	// text composition changes, so stored provenance hashes stay
	// distinguishable across versions.
	ObservabilityBundleVersion = "o11y-steering-v5"
)

// The workflow material below is a compact, Diderot-owned adaptation of the
// Apache-2.0 gcx skills at commit d3c8e552, principally debug-with-grafana,
// investigate-alert, explore-datasources, and their PromQL/LogQL/TraceQL
// references. It deliberately omits gcx commands and every mutation workflow.
var observabilityWorkflows = map[string]string{
	"alert":       promptcatalog.AskObservabilityAlert,
	"slo":         promptcatalog.AskObservabilitySLO,
	"exploration": promptcatalog.AskObservabilityExplore,
	"incident":    promptcatalog.AskObservabilityIncident,
	"sentry":      promptcatalog.AskObservabilitySentry,
}

var observabilitySyntax = map[string]string{
	"promql":  promptcatalog.AskObservabilityPromQL,
	"logql":   promptcatalog.AskObservabilityLogQL,
	"traceql": promptcatalog.AskObservabilityTraceQL,
}

type ObservabilitySteering struct {
	Enabled         bool
	Active          bool
	PanelRequested  bool
	Intent          string
	CardIDs         []string
	Text            string
	normalizedInput string
	RouterHash      string
	BundleHash      string
}

// BuildObservabilitySteering routes free-text input to an intent and card
// selection. Its Text/RouterHash/BundleHash are provenance-only now: the
// investigate tool's buildDelegatedSteering (observability_investigate.go)
// recomputes its own child-facing Text and hashes over the delegated request,
// since that is the text a model actually sees. This function still composes
// a real Text and valid hashes for both branches, though, because
// ArtifactStore.RecordSteering persists RouterHash/BundleHash under a CHECK
// constraint requiring a well-formed hex sha256 even for inactive rows.
func BuildObservabilitySteering(input string) ObservabilitySteering {
	return buildObservabilitySteering(promptcatalog.Active(), input)
}

func buildObservabilitySteering(bundle promptcatalog.Bundle, input string) ObservabilitySteering {
	normalized := normalizeObservabilityIntent(input)
	active := containsAnyToken(normalized, observabilityIntentTerms)
	b := ObservabilitySteering{Enabled: true, Active: active, normalizedInput: normalized}
	if !active {
		b.Intent = "none"
		b.RouterHash = hashText(ObservabilityRouterVersion + "\nnone")
		b.BundleHash = hashText(ObservabilityBundleVersion + "\nnone")
		return b
	}
	b.PanelRequested = observabilityPanelRequested(normalized)

	switch {
	case containsAnyToken(normalized, []string{"alert", "alerts", "firing", "pending", "rule", "pager", "page"}):
		b.Intent = "alert"
	case containsAnyToken(normalized, []string{"slo", "slis", "error budget", "burn rate", "objective"}):
		b.Intent = "slo"
	case containsAnyToken(normalized, []string{"discover", "available metrics", "metric names", "label names", "label values", "datasource", "data source", "what metrics"}):
		b.Intent = "exploration"
	default:
		b.Intent = "incident"
	}

	cards := []string{"workflow:" + b.Intent}
	if containsAnyToken(normalized, []string{"prometheus", "promql", "metric", "metrics", "histogram", "latency", "error rate", "slo", "burn rate"}) || b.Intent == "incident" {
		cards = append(cards, "syntax:promql")
	}
	if containsAnyToken(normalized, []string{"loki", "logql", "log", "logs"}) || b.Intent == "incident" {
		cards = append(cards, "syntax:logql")
	}
	if containsAnyToken(normalized, []string{"tempo", "traceql", "trace", "traces", "span", "spans"}) || b.Intent == "incident" {
		cards = append(cards, "syntax:traceql")
	}
	b.CardIDs = cards

	parts := []string{bundle.MustText(observabilityWorkflows[b.Intent])}
	for _, id := range cards[1:] {
		parts = append(parts, bundle.MustText(observabilitySyntax[strings.TrimPrefix(id, "syntax:")]))
	}
	b.Text = strings.Join(parts, "\n\n")
	b.RouterHash = hashText(ObservabilityRouterVersion + "\n" + normalized + "\n" + strings.Join(cards, ","))
	b.BundleHash = hashText(ObservabilityBundleVersion + "\n" + b.Text)
	return b
}

func observabilityPanelRequested(normalized string) bool {
	return containsAnyToken(normalized, []string{"panel", "panels", "dashboard", "dashboards"}) &&
		containsAnyToken(normalized, []string{"show", "see", "render", "image", "images", "screenshot", "screenshots", "picture", "visualize"})
}

var observabilityIntentTerms = []string{
	"grafana", "prometheus", "promql", "loki", "logql", "tempo", "traceql",
	"pyroscope", "observability", "o11y", "metric", "metrics", "dashboard",
	"panel", "alert", "alerts", "firing", "pending", "trace", "traces",
	"span", "spans", "profile", "profiles", "latency", "error rate", "slo",
	"error budget", "burn rate", "incident", "service down", "outage",
}

func normalizeObservabilityIntent(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func containsAnyToken(s string, terms []string) bool {
	padded := " " + s + " "
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term != "" && strings.Contains(padded, " "+term+" ") {
			return true
		}
	}
	return false
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sortedCardIDs(ids []string) []string {
	out := make([]string, len(ids))
	copy(out, ids)
	sort.Strings(out)
	return out
}
