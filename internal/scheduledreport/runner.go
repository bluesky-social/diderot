package scheduledreport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/authorization"
	"github.com/bluesky-social/diderot/internal/events"
)

// Agent is the scheduled-report subset of ask.Runner.
type Agent interface {
	CreateSession(ctx context.Context) (uuid.UUID, error)
	ExecuteTurn(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error)
}

// Delivery posts a generated report and returns the first Slack message ts.
type Delivery interface {
	DeliverScheduledReport(ctx context.Context, kind, target string, runID uuid.UUID, body string) (string, error)
}

type artifactAgent interface {
	LoadResultArtifacts(ctx context.Context, sessionID uuid.UUID, assistantTurn int) ([]ask.ArtifactDescriptor, error)
}

type artifactDelivery interface {
	DeliverScheduledArtifacts(ctx context.Context, kind, target, deliveryTS string, sessionID uuid.UUID, assistantTurn int, artifacts []ask.ArtifactDescriptor) error
}

// ScopeResolver resolves current private-channel membership for DM reports.
type ScopeResolver interface {
	ScopesForEmail(ctx context.Context, email string) []string
	UserIDForEmail(ctx context.Context, email string) (string, bool)
}

// Runner materializes due occurrences and drains the durable queue. One Tick is
// bounded so a backlog cannot monopolize the generic worker supervisor.
type Runner struct {
	Store             *Store
	Agent             Agent
	Delivery          Delivery
	ScopeResolver     ScopeResolver
	ChannelAuthorizer interface {
		ResolveChannel(context.Context, string) authorization.ChannelAccess
	}
	// Ready reports whether reports can be delivered now. While it is
	// false, due reports wait in the queue: none is enqueued or taken. Nil
	// is always ready.
	Ready      func() bool
	Logger     *slog.Logger
	MaxPerTick int
}

func (r *Runner) Tick(ctx context.Context) error {
	if r.Store == nil || r.Agent == nil || r.Delivery == nil {
		return errors.New("scheduled reports: store, agent, and delivery are required")
	}
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if !r.ready() {
		return nil
	}
	if err := r.Store.MaterializeDue(ctx); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	limit := r.MaxPerTick
	if limit <= 0 {
		limit = 10
	}
	for i := 0; i < limit && r.ready(); i++ {
		claimed, err := r.Store.ClaimNext(ctx)
		if errors.Is(err, ErrNoRun) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		if err := r.execute(ctx, claimed); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) ready() bool {
	return r.Ready == nil || r.Ready()
}

func (r *Runner) execute(ctx context.Context, claimed *ClaimedRun) error {
	report, run := claimed.Report, claimed.Run
	var err error
	output := ""
	var sessionID uuid.UUID
	assistantTurn := 0
	var artifacts []ask.ArtifactDescriptor
	generated := run.Output != nil
	if generated {
		if run.DeliveryKind == nil || run.SlackTarget == nil {
			err = errors.New("generated report has no saved delivery audience; enqueue a new run")
			r.recordFailure(ctx, report.ID, run.ID, true, err)
			return err
		}
		report.DeliveryKind, report.SlackTarget = *run.DeliveryKind, *run.SlackTarget
		output = *run.Output
		if run.AskSessionID != nil {
			sessionID = *run.AskSessionID
		}
		if run.AskAssistantTurn != nil {
			assistantTurn = *run.AskAssistantTurn
		}
		if sessionID != uuid.Nil && run.AskAssistantTurn != nil {
			if loader, ok := r.Agent.(artifactAgent); ok {
				artifacts, err = loader.LoadResultArtifacts(ctx, sessionID, assistantTurn)
				if err != nil {
					r.recordFailure(ctx, report.ID, run.ID, true, err)
					return fmt.Errorf("scheduled report %s reload artifacts: %w", report.ID, err)
				}
			}
		}
	} else {
		scopes, scopeErr := r.scopes(ctx, report)
		if scopeErr != nil {
			r.recordFailure(ctx, report.ID, run.ID, false, scopeErr)
			return fmt.Errorf("scheduled report %s resolve audience: %w", report.ID, scopeErr)
		}
		corpusOnly := false
		if report.DeliveryKind == DeliverySlackChannel {
			if r.ChannelAuthorizer == nil {
				err = errors.New("channel audience resolver is not configured")
				r.recordFailure(ctx, report.ID, run.ID, false, err)
				return err
			}
			access := r.ChannelAuthorizer.ResolveChannel(ctx, report.SlackTarget)
			scopes, corpusOnly = access.AllowedScopes, access.CorpusOnly
		}
		if len(scopes) == 0 {
			err = errors.New("channel has no authorized corpus access")
			r.recordFailure(ctx, report.ID, run.ID, false, err)
			return err
		}
		sessionID, err = r.Agent.CreateSession(ctx)
		if err != nil {
			r.recordFailure(ctx, report.ID, run.ID, false, err)
			return fmt.Errorf("scheduled report %s create session: %w", report.ID, err)
		}
		turn, err := r.Agent.ExecuteTurn(ctx, ask.TurnRequest{
			SessionID:     sessionID,
			UserInput:     buildPrompt(report, run),
			Surface:       ask.AskRunSurfaceScheduled,
			AllowedScopes: scopes,
			CorpusOnly:    corpusOnly,
		})
		if err != nil {
			r.recordFailure(ctx, report.ID, run.ID, false, err)
			return fmt.Errorf("scheduled report %s generate: %w", report.ID, err)
		}
		output = strings.TrimSpace(turn.Reply)
		assistantTurn = turn.AssistantTurn
		artifacts = turn.Artifacts
		if output == "" {
			err = errors.New("agent returned an empty report")
			r.recordFailure(ctx, report.ID, run.ID, false, err)
			return fmt.Errorf("scheduled report %s: %w", report.ID, err)
		}
		if turn.IncompleteReason != "" {
			output += "\n_(" + ask.IncompleteNotice(turn.IncompleteReason) + ")_"
		}
		if err := r.Store.SaveGenerated(ctx, run.ID, sessionID, assistantTurn, output, report.DeliveryKind, report.SlackTarget); err != nil {
			r.recordFailure(ctx, report.ID, run.ID, false, err)
			return fmt.Errorf("scheduled report %s save output: %w", report.ID, err)
		}
		generated = true
	}

	deliveryTS := ""
	if run.DeliveryTS != nil {
		deliveryTS = *run.DeliveryTS
	}
	if deliveryTS == "" {
		deliveryTS, err = r.Delivery.DeliverScheduledReport(ctx, report.DeliveryKind,
			report.SlackTarget, run.ID, output)
		if err != nil {
			r.recordFailure(ctx, report.ID, run.ID, generated, err)
			return fmt.Errorf("scheduled report %s deliver: %w", report.ID, err)
		}
		if err := r.Store.SaveDeliveryTS(ctx, run.ID, deliveryTS); err != nil {
			r.recordFailure(ctx, report.ID, run.ID, true, err)
			return fmt.Errorf("scheduled report %s checkpoint delivery: %w", report.ID, err)
		}
	}
	if len(artifacts) > 0 {
		deliverer, ok := r.Delivery.(artifactDelivery)
		if !ok {
			err = errors.New("scheduled report artifact delivery is unavailable")
		} else {
			err = deliverer.DeliverScheduledArtifacts(ctx, report.DeliveryKind, report.SlackTarget, deliveryTS, sessionID, assistantTurn, artifacts)
		}
		if err != nil {
			r.recordFailure(ctx, report.ID, run.ID, true, err)
			return fmt.Errorf("scheduled report %s deliver artifacts: %w", report.ID, err)
		}
	}
	if err := r.Store.Succeed(ctx, report.ID, run.ID, deliveryTS); err != nil {
		return fmt.Errorf("scheduled report %s mark success: %w", report.ID, err)
	}
	r.Logger.Info("scheduled report delivered", "report_id", report.ID,
		"run_id", run.ID, "target", report.SlackTarget)
	return nil
}

func (r *Runner) recordFailure(ctx context.Context, reportID, runID uuid.UUID, generated bool, err error) {
	if saveErr := r.Store.Fail(ctx, reportID, runID, generated, err); saveErr != nil {
		r.Logger.Error("scheduled report: persist failure failed", "run_id", runID,
			"original_err", err, "persist_err", saveErr)
	}
}

func (r *Runner) scopes(ctx context.Context, report Report) ([]string, error) {
	baseline := []string{events.ScopePublic, events.ScopeInternal}
	if report.DeliveryKind == DeliverySlackChannel {
		return append(baseline, events.ScopeChannel(report.SlackTarget)), nil
	}
	if r.ScopeResolver == nil {
		return nil, errors.New("DM audience resolver is not configured")
	}
	userID, ok := r.ScopeResolver.UserIDForEmail(ctx, report.OwnerEmail)
	if !ok || userID != report.SlackTarget {
		return nil, fmt.Errorf("DM target %q does not match owner %q", report.SlackTarget, report.OwnerEmail)
	}
	scopes := r.ScopeResolver.ScopesForEmail(ctx, report.OwnerEmail)
	if len(scopes) == 0 {
		return baseline, nil
	}
	return scopes, nil
}

func buildPrompt(report Report, run Run) string {
	return fmt.Sprintf(`<scheduled_report>
name: %s
owner_email: %s
delivery: %s:%s
timezone: %s
window_start_inclusive: %s
window_end_exclusive: %s
</scheduled_report>

Write this recurring report now, from the corpus. It covers the reporting window above and nothing outside it: pass the window's start and end as the time bounds of every retrieve and list call, and take dates from them, not from words such as "today" in the instructions. Use list, filtered to the people, channels or sources the instructions concern, to see everything in the window, and retrieve to look into a particular subject.

The instructions below decide what the report contains, its shape and its length. Write it in standard Markdown, whatever markup they name. Where they are silent: cite each material claim inline, keep finished work apart from work in progress and from plans, and report a piece of work once even when several sources mention it. Whatever they say about length, do not split one piece of work in two or promote chatter to reach a count. When the window holds nothing that belongs in the report, say so plainly instead of filling it.

<report_instructions>
%s
</report_instructions>`,
		xmlText(report.Name), xmlText(report.OwnerEmail), xmlText(report.DeliveryKind), xmlText(report.SlackTarget),
		report.Timezone, run.WindowStart.Format(time.RFC3339),
		run.WindowEnd.Format(time.RFC3339), run.PromptSnapshot)
}

func xmlText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
