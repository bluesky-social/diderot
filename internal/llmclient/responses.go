package llmclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ResponsesToolRequest is the Responses request session titles and session
// summaries send: one function tool, which the model is made to call.
type ResponsesToolRequest struct {
	Model             string              `json:"model"`
	Instructions      string              `json:"instructions"`
	Input             string              `json:"input"`
	MaxOutputTokens   int                 `json:"max_output_tokens"`
	Tools             []ResponsesTool     `json:"tools"`
	ToolChoice        ResponsesToolChoice `json:"tool_choice"`
	ParallelToolCalls bool                `json:"parallel_tool_calls"`
	Store             bool                `json:"store"`
	Reasoning         *ResponsesReasoning `json:"reasoning,omitempty"`
}

type ResponsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ResponsesToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type ResponsesReasoning struct {
	Effort string `json:"effort"`
}

// NewResponsesToolRequest is the request that makes model call the function
// tool name. An empty effort sends no reasoning field.
func NewResponsesToolRequest(model, instructions, input string, maxOutputTokens int, effort, name, description string, parameters map[string]any) ResponsesToolRequest {
	request := ResponsesToolRequest{
		Model: model, Instructions: instructions, Input: input, MaxOutputTokens: maxOutputTokens,
		Tools:      []ResponsesTool{{Type: "function", Name: name, Description: description, Parameters: parameters}},
		ToolChoice: ResponsesToolChoice{Type: "function", Name: name},
	}
	if effort != "" {
		request.Reasoning = &ResponsesReasoning{Effort: effort}
	}
	return request
}

// ResponsesToolAnswer is what a ResponsesToolRequest's answer is read from.
type ResponsesToolAnswer struct {
	Status            string `json:"status"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"output"`
	// Usage counts cached input in InputTokens and reasoning in
	// OutputTokens.
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// ResponsesTextRequest is a one-shot text generation request. Input is
// either plain text or a list of ResponsesMessage values.
type ResponsesTextRequest struct {
	Model           string `json:"model"`
	Instructions    string `json:"instructions,omitempty"`
	Input           any    `json:"input"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Store           bool   `json:"store"`
}

type ResponsesMessage struct {
	Role    string          `json:"role"`
	Content []ResponsesPart `json:"content"`
}

type ResponsesPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	FileData string `json:"file_data,omitempty"`
	Filename string `json:"filename,omitempty"`
}

type ResponsesTextResult struct {
	Text         string
	InputTokens  int
	OutputTokens int
	Truncated    bool
}

// TextResponse sends native Responses input and reads only the model's
// message text. Reasoning and tool items are never treated as output text.
func (c *Client) TextResponse(ctx context.Context, body ResponsesTextRequest, limit int64) (ResponsesTextResult, error) {
	raw, err := c.PostJSON(ctx, "/responses", body, limit)
	if err != nil {
		return ResponsesTextResult{}, err
	}
	var out struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ResponsesTextResult{}, fmt.Errorf("decode Responses text: %w", err)
	}
	if out.Status != "completed" && out.Status != "incomplete" {
		return ResponsesTextResult{}, errors.New("Responses did not complete")
	}
	var text []string
	for _, item := range out.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" && strings.TrimSpace(part.Text) != "" {
				text = append(text, part.Text)
			}
		}
	}
	if len(text) == 0 {
		return ResponsesTextResult{}, errors.New("Responses returned no text")
	}
	return ResponsesTextResult{
		Text: strings.Join(text, "\n"), InputTokens: out.Usage.InputTokens,
		OutputTokens: out.Usage.OutputTokens, Truncated: out.Status == "incomplete",
	}, nil
}
