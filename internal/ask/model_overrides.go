package ask

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/components/model"
	openairesponses "github.com/openai/openai-go/v3/responses"

	"github.com/bluesky-social/diderot/internal/modelregistry"
)

type modelOverridesContextKey struct{}

func validateModelOverrides(entry modelregistry.Model, overrides ModelOverrides) error {
	protocol, ok := entry.AgenticProtocol()
	if !ok {
		return ValidateModelSelection(entry, "", "")
	}
	if overrides.ReasoningEffort != nil {
		if *overrides.ReasoningEffort == "" {
			return errors.New("reasoning effort override must not be empty")
		}
		if err := entry.ValidateReasoningEffort(*overrides.ReasoningEffort); err != nil {
			return err
		}
	}
	if overrides.ServiceTier != nil {
		if *overrides.ServiceTier == "" {
			return errors.New("service tier override must not be empty")
		}
		if err := entry.ValidateServiceTier(*overrides.ServiceTier); err != nil {
			return err
		}
	}
	if schedule := overrides.ReasoningEffortSchedule; schedule != nil {
		if schedule.AfterToolRounds < 1 {
			return fmt.Errorf("reasoning effort schedule after_tool_rounds must be at least 1")
		}
		if schedule.Effort == "" {
			return errors.New("reasoning effort schedule effort must not be empty")
		}
		if err := entry.ValidateReasoningEffort(schedule.Effort); err != nil {
			return fmt.Errorf("reasoning effort schedule: %w", err)
		}
	}
	if (overrides.ReasoningEffort != nil || overrides.ServiceTier != nil || overrides.ReasoningEffortSchedule != nil) && protocol != modelregistry.ProtocolResponses {
		return errors.New("request-scoped model overrides currently require a Responses model")
	}
	return nil
}

func reasoningEffortForCall(ctx context.Context, base string) string {
	schedule := modelOverridesFromContext(ctx).ReasoningEffortSchedule
	budget := agenticToolBudgetFromContext(ctx)
	if schedule == nil || budget == nil {
		return base
	}
	completed, _, _ := budget.state()
	if completed >= schedule.AfterToolRounds {
		return schedule.Effort
	}
	return base
}

// ValidateModelOverrides validates public request-level controls against the
// Ask model's registry entry.
func ValidateModelOverrides(entry modelregistry.Model, overrides ModelOverrides) error {
	return validateModelOverrides(entry, overrides)
}

func resolveModelSettings(defaults EffectiveModelSettings, overrides ModelOverrides) EffectiveModelSettings {
	settings := defaults
	if overrides.ReasoningEffort != nil {
		settings.ReasoningEffort = *overrides.ReasoningEffort
	}
	if overrides.ServiceTier != nil {
		settings.ServiceTier = *overrides.ServiceTier
	}
	return settings
}

func withModelOverrides(ctx context.Context, overrides ModelOverrides) context.Context {
	return context.WithValue(ctx, modelOverridesContextKey{}, overrides)
}

func modelOverridesFromContext(ctx context.Context) ModelOverrides {
	overrides, _ := ctx.Value(modelOverridesContextKey{}).(ModelOverrides)
	return overrides
}

func modelOptionsForOverrides(overrides ModelOverrides, extraFields map[string]any) []model.Option {
	options := make([]model.Option, 0, 2)
	if overrides.ReasoningEffort != nil {
		options = append(options, agenticopenai.WithResponsesReasoning(&openairesponses.ReasoningParam{
			Effort: openairesponses.ReasoningEffort(*overrides.ReasoningEffort),
		}))
	}
	fields := make(map[string]any, len(extraFields)+1)
	for key, value := range extraFields {
		fields[key] = value
	}
	if overrides.ServiceTier != nil {
		fields["service_tier"] = *overrides.ServiceTier
	}
	if len(fields) > 0 {
		options = append(options, agenticopenai.WithExtraFields(fields))
	}
	return options
}
