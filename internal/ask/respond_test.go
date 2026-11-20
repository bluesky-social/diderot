package ask

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	schemaopenai "github.com/cloudwego/eino/schema/openai"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDecodeFinalResponse(t *testing.T) {
	id := uuid.New()
	got, err := decodeFinalResponse(`{"answer":"the answer","artifacts":[{"artifact_id":"` + id.String() + `","title":"Panel","alt_text":"A chart"}]}`)
	require.NoError(t, err)
	require.Equal(t, "the answer", got.Answer)
	require.Equal(t, id, got.Artifacts[0].ArtifactID)
}

func TestDecodeFinalResponseStripsEmbeddedNulBytes(t *testing.T) {
	// A model can copy a literal "\u0000" escape it saw in a tool result (a
	// Loki/Sentry/Grafana MCP result, a web_fetch page, etc.) into its
	// structured answer. encoding/json decodes that escape to a real NUL
	// byte (U+0000). Every surface that persists payload.Answer -- via
	// TurnResult.Reply (scheduled reports, PagerDuty investigations) and via
	// the transcript (ask_messages) -- writes to a plain Postgres text
	// column that rejects a NUL outright (SQLSTATE 22021), so the answer
	// must come out of decodeFinalResponse clean.
	got, err := decodeFinalResponse(`{"answer":"log line: \u0000 end","artifacts":[]}`)
	require.NoError(t, err)
	require.Equal(t, "log line:  end", got.Answer)
	require.NotContains(t, got.Answer, "\x00")
}

func TestDecodeFinalResponseRejectsAllNulAnswer(t *testing.T) {
	// An answer that is nothing but NUL bytes strips down to empty, which
	// must fail the same "answer is required" check a blank answer does,
	// not silently succeed with an empty Reply.
	_, err := decodeFinalResponse(`{"answer":"\u0000\u0000","artifacts":[]}`)
	require.ErrorContains(t, err, "answer is required")
}

func TestDecodeFinalResponseRejectsInvalidTerminalValues(t *testing.T) {
	for name, raw := range map[string]string{
		"not json":          `answer`,
		"unknown field":     `{"answer":"x","artifacts":[],"extra":true}`,
		"blank answer":      `{"answer":"  ","artifacts":[]}`,
		"missing artifacts": `{"answer":"x"}`,
		"trailing value":    `{"answer":"x","artifacts":[]} {}`,
		"bad artifact":      `{"answer":"x","artifacts":[{"artifact_id":"00000000-0000-0000-0000-000000000000","title":"t","alt_text":"a"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeFinalResponse(raw)
			require.Error(t, err)
		})
	}
}

func TestDecodeFinalResponseMessageRejectsRefusal(t *testing.T) {
	msg := NewAgenticAssistantText("")
	msg.ContentBlocks[0].AssistantGenText.OpenAIExtension = &schemaopenai.AssistantGenTextExtension{
		Refusal: &schemaopenai.OutputRefusal{Reason: "policy"},
	}
	_, err := decodeFinalResponseMessage(msg)
	require.ErrorContains(t, err, "refused: policy")

	msg.ContentBlocks = append(msg.ContentBlocks, schema.NewContentBlock(&schema.AssistantGenText{Text: `{}`}))
	_, err = decodeFinalResponseMessage(msg)
	require.Error(t, err)
}
