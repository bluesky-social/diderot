package ask

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

const finalResponseSchemaName = "diderot_answer"

// FinalResponse is the provider-constrained terminal value for a root Ask
// turn. Research remains ordinary tool calling; the final generation is
// emitted as strict JSON and projected to the user-visible answer at the
// persistence boundary.
type FinalResponse struct {
	Answer    string              `json:"answer"`
	Artifacts []ArtifactSelection `json:"artifacts"`
}

func finalResponseModelOption() model.Option {
	format := responses.ResponseFormatTextConfigParamOfJSONSchema(finalResponseSchemaName, map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"answer": map[string]any{
				"type":        "string",
				"description": "The complete answer shown to the user.",
			},
			"artifacts": map[string]any{
				"type":        "array",
				"description": "Panel images to deliver with the answer; use an empty array when none belong.",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"properties": map[string]any{
						"artifact_id": map[string]any{"type": "string"},
						"title":       map[string]any{"type": "string"},
						"alt_text":    map[string]any{"type": "string"},
					},
					"required": []string{"artifact_id", "title", "alt_text"},
				},
			},
		},
		"required": []string{"answer", "artifacts"},
	})
	format.OfJSONSchema.Strict = param.NewOpt(true)
	return agenticopenai.WithResponsesText(&responses.ResponseTextConfigParam{Format: format})
}

func decodeFinalResponse(raw string) (FinalResponse, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var response FinalResponse
	if err := decoder.Decode(&response); err != nil {
		return FinalResponse{}, fmt.Errorf("decode terminal response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return FinalResponse{}, fmt.Errorf("decode terminal response trailer: %w", err)
		}
		return FinalResponse{}, errors.New("decode terminal response: trailing JSON value")
	}
	// response.Answer is decoded by encoding/json, which turns a literal
	// "\u0000" escape the model wrote (e.g. copied from a tool result) into
	// a real NUL byte (U+0000). FinalResponse.Answer becomes TurnResult.Reply
	// verbatim, and every surface that persists it writes to a plain
	// Postgres text column -- ask_messages via the recorder, but also
	// scheduled_report_runs.output (internal/scheduledreport/runner.go) and
	// pagerduty_investigations.output (internal/pagerduty/runner.go) -- all
	// of which reject a NUL byte outright (SQLSTATE 22021). Stripping it here,
	// at the one place every surface decodes the model's structured answer,
	// keeps Reply and the persisted transcript the same text and keeps the
	// answer from being discarded after a full, otherwise-successful run.
	response.Answer = stripNUL(response.Answer)
	if strings.TrimSpace(response.Answer) == "" {
		return FinalResponse{}, errors.New("terminal response answer is required")
	}
	if response.Artifacts == nil {
		return FinalResponse{}, errors.New("terminal response artifacts is required")
	}
	if err := validateArtifactSelectionShape(response.Artifacts); err != nil {
		return FinalResponse{}, err
	}
	return response, nil
}

func decodeFinalResponseMessage(msg *schema.AgenticMessage) (FinalResponse, error) {
	if msg == nil {
		return FinalResponse{}, errors.New("terminal assistant message is required")
	}
	var text string
	textBlocks := 0
	for _, block := range msg.ContentBlocks {
		if block == nil || block.AssistantGenText == nil {
			continue
		}
		textBlocks++
		generated := block.AssistantGenText
		if generated.OpenAIExtension != nil && generated.OpenAIExtension.Refusal != nil {
			reason := strings.TrimSpace(generated.OpenAIExtension.Refusal.Reason)
			if reason == "" {
				reason = "unspecified refusal"
			}
			return FinalResponse{}, fmt.Errorf("terminal response refused: %s", reason)
		}
		text = generated.Text
	}
	if textBlocks != 1 {
		return FinalResponse{}, fmt.Errorf("terminal assistant has %d text blocks; want 1", textBlocks)
	}
	return decodeFinalResponse(text)
}
