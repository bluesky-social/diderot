// Package automation defines an automation, a connector that runs Ask in
// response to something and delivers the answer, and the registry of the
// automations a build knows.
package automation

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bluesky-social/diderot/internal/ask"
	slackcx "github.com/bluesky-social/diderot/internal/connector/slack"
	"github.com/bluesky-social/diderot/internal/settings"
)

// Automation runs Ask in response to something and delivers the answer.
type Automation interface {
	settings.Definition

	// Info is fixed facts about the automation.
	Info() Info

	// Run holds the automation's workers for one leader term with these
	// settings, until ctx ends. The settings supervisor restarts it when
	// they change.
	Run(ctx context.Context, deps Deps, s settings.Resolved) error
}

// StatusReporter is an automation whose page shows how it is doing.
type StatusReporter interface {
	// Status reads it. Every replica's pages call it, so it reads only what
	// every replica knows.
	Status(ctx context.Context) (settings.Status, error)
}

// Info is fixed facts about an automation.
type Info struct {
	// Title is what pages call it ("PagerDuty").
	Title string
	// Step is the ID of its step on the setup checklist ("pagerduty"), and
	// Summary that step's sentence on what it does.
	Step, Summary string
}

// Deps is what every automation is handed for one leader term.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	// Metrics registers the automation's own metrics and counts its base
	// ones: a worker supervisor built with it counts its workers' runs.
	Metrics settings.Metrics
	// Ask answers questions. It is nil while Ask is off in this deployment.
	Ask Ask
	// Observability reports whether Ask can delegate to its observability
	// investigator with a Grafana connection now. It changes as the tool
	// connections do.
	Observability func() bool
	// AskAllowed reports whether Ask may answer an audience no
	// administrator chose, which it may not while the install is in setup.
	AskAllowed func() bool
	// Slack is what the running Slack source lends to post with, empty
	// while Slack is off. Post through it inside its With, which keeps
	// that Slack run from ending mid-post, and never keep what it lent.
	Slack *settings.Live[slackcx.Delivery]
}

// Ask is what automations ask with: the Ask service.
type Ask interface {
	CreateSession(ctx context.Context) (uuid.UUID, error)
	ExecuteTurn(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error)
	LoadResultArtifacts(ctx context.Context, sessionID uuid.UUID, assistantTurn int) ([]ask.ArtifactDescriptor, error)
}

// Registry is the automations a build knows, in the order pages list them.
// The zero value has none.
type Registry struct {
	automations []Automation
}

// NewRegistry checks and registers automations. A reused name or an empty
// Info field panics: these are programming errors.
func NewRegistry(automations ...Automation) Registry {
	names := map[string]bool{}
	for _, automation := range automations {
		name := automation.Name()
		if names[name] {
			panic(fmt.Sprintf("automation: %q is registered twice", name))
		}
		names[name] = true
		if info := automation.Info(); info.Title == "" || info.Step == "" || info.Summary == "" {
			panic(fmt.Sprintf("automation: %s: Info needs a title, a setup step, and a summary", name))
		}
	}
	return Registry{automations: automations}
}

// Automations returns every registered automation.
func (r Registry) Automations() []Automation { return r.automations }

// Names returns every registered automation's connector name.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r.automations))
	for _, automation := range r.automations {
		names = append(names, automation.Name())
	}
	return names
}

// Lookup returns the automation named name.
func (r Registry) Lookup(name string) (Automation, bool) {
	for _, automation := range r.automations {
		if automation.Name() == name {
			return automation, true
		}
	}
	return nil, false
}
