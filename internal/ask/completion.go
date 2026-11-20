package ask

import (
	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/openai"
)

// IncompleteReason identifies why a persisted answer is not complete.
// The empty value means the answer completed normally. Values are stable wire
// and database strings: callers render them directly in diagnostics and the
// final assistant message persists them in ask_messages.incomplete_reason.
type IncompleteReason string

const (
	IncompleteMaxTokens            IncompleteReason = "max_tokens"
	IncompleteStopSequence         IncompleteReason = "stop_sequence"
	IncompleteMalformedToolUse     IncompleteReason = "malformed_tool_use"
	IncompleteGuardrail            IncompleteReason = "guardrail_intervened"
	IncompleteContentFiltered      IncompleteReason = "content_filtered"
	IncompleteMalformedOutput      IncompleteReason = "malformed_model_output"
	IncompleteContextExceeded      IncompleteReason = "model_context_window_exceeded"
	IncompleteUnknownStop          IncompleteReason = "unknown_stop_reason"
	IncompleteToolBudget           IncompleteReason = "tool_budget_exhausted"
	IncompleteSynthesisError       IncompleteReason = "synthesis_error"
	IncompleteReferenceUnavailable IncompleteReason = "reference_unavailable"
	IncompleteStopWithoutRespond   IncompleteReason = "stop_without_respond"
)

func incompleteReasonForResponse(msg *schema.AgenticMessage) IncompleteReason {
	if msg == nil || msg.ResponseMeta == nil {
		return ""
	}
	if ext := msg.ResponseMeta.OpenAIExtension; ext != nil {
		if ext.Status != openai.ResponseStatusIncomplete {
			return ""
		}
		if ext.IncompleteDetails == nil {
			return IncompleteUnknownStop
		}
		switch ext.IncompleteDetails.Reason {
		case "max_output_tokens":
			return IncompleteMaxTokens
		case "content_filter":
			return IncompleteContentFiltered
		default:
			return IncompleteUnknownStop
		}
	}
	if ext := msg.ResponseMeta.ClaudeExtension; ext != nil {
		switch ext.StopReason {
		case "", "end_turn", "tool_use":
			return ""
		case "max_tokens":
			return IncompleteMaxTokens
		case "stop_sequence":
			return IncompleteStopSequence
		case "refusal":
			return IncompleteGuardrail
		case "model_context_window_exceeded":
			return IncompleteContextExceeded
		default:
			return IncompleteUnknownStop
		}
	}
	return ""
}

// IncompleteNotice is the shared user-facing explanation used by Slack and
// scheduled reports. Keeping this mapping next to the enum prevents each
// delivery surface from turning every non-happy terminal state into the
// misleading generic "response was cut off" message.
func IncompleteNotice(reason IncompleteReason) string {
	switch reason {
	case IncompleteMaxTokens:
		return "answer stopped at the model's output limit"
	case IncompleteStopSequence:
		return "answer stopped after the model emitted a stop sequence"
	case IncompleteMalformedToolUse:
		return "answer stopped after the model emitted an invalid tool call"
	case IncompleteGuardrail:
		return "answer stopped because a model safety guardrail intervened"
	case IncompleteContentFiltered:
		return "answer stopped because content filtering intervened"
	case IncompleteMalformedOutput:
		return "answer stopped because the model returned malformed output"
	case IncompleteContextExceeded:
		return "answer stopped because the model context window was exhausted"
	case IncompleteToolBudget:
		return "answer is incomplete because the research tool budget was exhausted"
	case IncompleteSynthesisError:
		return "answer is incomplete because final synthesis failed"
	case IncompleteReferenceUnavailable:
		return "answer unavailable because a referenced corpus item could not be accessed"
	case IncompleteStopWithoutRespond:
		return "answer stopped before the assistant submitted a response"
	case IncompleteUnknownStop:
		return "answer stopped for an unrecognized model reason"
	case "":
		return ""
	default:
		return "answer stopped before completion"
	}
}
