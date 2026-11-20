package ask

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/cloudwego/eino/schema/claude"
	"github.com/cloudwego/eino/schema/openai"
	"github.com/stretchr/testify/require"
)

func TestIncompleteReasonForResponse(t *testing.T) {
	tests := []struct {
		name    string
		status  openai.ResponseStatus
		details *openai.IncompleteDetails
		want    IncompleteReason
	}{
		{name: "completed", status: openai.ResponseStatusCompleted},
		{name: "incomplete without details", status: openai.ResponseStatusIncomplete, want: IncompleteUnknownStop},
		{name: "max output tokens", status: openai.ResponseStatusIncomplete, details: &openai.IncompleteDetails{Reason: "max_output_tokens"}, want: IncompleteMaxTokens},
		{name: "content filter", status: openai.ResponseStatusIncomplete, details: &openai.IncompleteDetails{Reason: "content_filter"}, want: IncompleteContentFiltered},
		{name: "future reason", status: openai.ResponseStatusIncomplete, details: &openai.IncompleteDetails{Reason: "future_reason"}, want: IncompleteUnknownStop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := NewAgenticAssistantText("answer")
			msg.ResponseMeta = &schema.AgenticResponseMeta{OpenAIExtension: &openai.ResponseMetaExtension{
				Status: tt.status, IncompleteDetails: tt.details,
			}}
			if got := incompleteReasonForResponse(msg); got != tt.want {
				t.Errorf("incompleteReasonForResponse() = %q, want %q", got, tt.want)
			}
		})
	}
	if got := incompleteReasonForResponse(nil); got != "" {
		t.Fatalf("nil response reason = %q, want empty", got)
	}
}

func TestIncompleteReasonForClaudeResponse(t *testing.T) {
	tests := []struct {
		stopReason string
		want       IncompleteReason
	}{
		{stopReason: ""},
		{stopReason: "end_turn"},
		{stopReason: "tool_use"},
		{stopReason: "max_tokens", want: IncompleteMaxTokens},
		{stopReason: "stop_sequence", want: IncompleteStopSequence},
		{stopReason: "refusal", want: IncompleteGuardrail},
		{stopReason: "model_context_window_exceeded", want: IncompleteContextExceeded},
		{stopReason: "pause_turn", want: IncompleteUnknownStop},
		{stopReason: "future_reason", want: IncompleteUnknownStop},
	}
	for _, tt := range tests {
		t.Run(tt.stopReason, func(t *testing.T) {
			msg := NewAgenticAssistantText("answer")
			msg.ResponseMeta = &schema.AgenticResponseMeta{ClaudeExtension: &claude.ResponseMetaExtension{
				StopReason: tt.stopReason,
			}}
			require.Equal(t, tt.want, incompleteReasonForResponse(msg))
		})
	}
}

func TestIncompleteNoticeCompleteIsEmpty(t *testing.T) {
	if got := IncompleteNotice(""); got != "" {
		t.Fatalf("IncompleteNotice(empty) = %q, want empty", got)
	}
}

func TestEveryIncompleteReasonPersistsAgentic(t *testing.T) {
	reasons := []IncompleteReason{
		IncompleteMaxTokens,
		IncompleteStopSequence,
		IncompleteMalformedToolUse,
		IncompleteGuardrail,
		IncompleteContentFiltered,
		IncompleteMalformedOutput,
		IncompleteContextExceeded,
		IncompleteUnknownStop,
		IncompleteToolBudget,
		IncompleteSynthesisError,
		IncompleteStopWithoutRespond,
	}
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	for _, reason := range reasons {
		t.Run(string(reason), func(t *testing.T) {
			session, err := mgr.Create(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = mgr.AppendAgenticMessages(context.Background(), session.ID, []StoredMessage{{
				Turn: 0, Message: NewAgenticAssistantText("partial"), IncompleteReason: reason,
			}})
			if err != nil {
				t.Fatalf("persist reason %q: %v", reason, err)
			}
			_, messages, err := mgr.LoadAgenticSession(context.Background(), session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := messages[0].IncompleteReason; got != reason {
				t.Errorf("round-trip reason = %q, want %q", got, reason)
			}
			if notice := IncompleteNotice(reason); notice == "" {
				t.Errorf("IncompleteNotice(%q) is empty", reason)
			}
		})
	}
}

func TestAppendAgenticMessagesRejectsIncompleteReasonOnUser(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	session, err := mgr.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.AppendAgenticMessages(context.Background(), session.ID, []StoredMessage{{
		Turn: 0, Message: NewAgenticUserText("question"), IncompleteReason: IncompleteMaxTokens,
	}})
	if err == nil {
		t.Fatal("expected assistant-role validation error")
	}
}
