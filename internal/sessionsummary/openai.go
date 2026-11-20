package sessionsummary

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

// ResponsesReasoningEffort is the effort Responses requests send to models
// whose registry entry lists it, as session titles do: a summary is written
// from what the request holds.
const ResponsesReasoningEffort = "low"

// ResponsesClientConfig configures the OpenAI Responses transport.
type ResponsesClientConfig struct {
	// BaseURL is the Responses endpoint, including any /v1 prefix.
	BaseURL    string
	APIKey     string
	Auth       llmclient.AuthStyle
	HTTPClient *http.Client
	// Headers are extra headers every request carries: the provider's.
	Headers http.Header
	// ReasoningEffort is sent as reasoning.effort when non-empty. Callers
	// pass ResponsesReasoningEffort when the model's registry entry lists
	// it.
	ReasoningEffort string
}

// ResponsesClient is the ToolClient that speaks OpenAI Responses, forcing
// the tool with a function tool_choice.
type ResponsesClient struct {
	client *llmclient.Client
	effort string
}

// NewResponsesClient validates cfg without making a network request.
func NewResponsesClient(cfg ResponsesClientConfig) (*ResponsesClient, error) {
	client, err := llmclient.New(llmclient.Config{
		BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Auth: cfg.Auth, HTTPClient: cfg.HTTPClient, Headers: cfg.Headers,
	})
	if err != nil {
		return nil, fmt.Errorf("sessionsummary responses: %w", err)
	}
	return &ResponsesClient{client: client, effort: cfg.ReasoningEffort}, nil
}

func (r *ResponsesClient) CallTool(ctx context.Context, request ToolRequest) (json.RawMessage, Usage, error) {
	body := llmclient.NewResponsesToolRequest(request.ModelID, request.System, request.User, int(request.MaxTokens),
		r.effort, request.ToolName, request.ToolDescription, request.ToolSchema)
	raw, err := r.client.PostJSON(ctx, "/responses", body, gatewaySummaryMaxResponseBytes)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("responses: %w", err)
	}
	var answer llmclient.ResponsesToolAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, Usage{}, fmt.Errorf("responses: decode response: %w", err)
	}
	usage := Usage{InputTokens: answer.Usage.InputTokens, OutputTokens: answer.Usage.OutputTokens}
	if answer.Status == "incomplete" && answer.IncompleteDetails.Reason == "max_output_tokens" {
		return nil, usage, fmt.Errorf("response truncated at max_output_tokens (%d)", request.MaxTokens)
	}
	if answer.Status != "completed" {
		return nil, usage, fmt.Errorf("unexpected response status: %q", answer.Status)
	}
	var arguments json.RawMessage
	found := false
	for _, item := range answer.Output {
		if item.Type != "function_call" {
			continue
		}
		if item.Name != request.ToolName {
			return nil, usage, fmt.Errorf("unexpected function_call %q, want %q", item.Name, request.ToolName)
		}
		if found {
			return nil, usage, errors.New("multiple function_call items in response")
		}
		arguments, found = json.RawMessage(item.Arguments), true
	}
	if !found {
		return nil, usage, errors.New("no function_call item in response")
	}
	return arguments, usage, nil
}

// ChatClientConfig configures the OpenAI Chat Completions transport.
type ChatClientConfig struct {
	// BaseURL is the chat-completions endpoint, including any /v1 prefix.
	BaseURL    string
	APIKey     string
	Auth       llmclient.AuthStyle
	HTTPClient *http.Client
	// Headers are extra headers every request carries: the provider's.
	Headers http.Header
	// TokenParam is the model registry's chat_token_param for the model:
	// the field that carries the output limit. Empty means max_tokens.
	TokenParam llmclient.ChatTokenParam
	// ReasoningTokenAllowance is the model registry's
	// chat_reasoning_token_allowance for the model, added to each stage's
	// output limit.
	ReasoningTokenAllowance int
	// ExtraBody is the model registry's chat fields for the model.
	ExtraBody map[string]any
}

// ChatClient is the ToolClient that speaks OpenAI Chat Completions, forcing
// the tool with a function tool_choice.
type ChatClient struct {
	client     *llmclient.Client
	tokenParam llmclient.ChatTokenParam
	allowance  int
	extraBody  map[string]any
}

// NewChatClient validates cfg without making a network request.
func NewChatClient(cfg ChatClientConfig) (*ChatClient, error) {
	client, err := llmclient.New(llmclient.Config{
		BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Auth: cfg.Auth, HTTPClient: cfg.HTTPClient, Headers: cfg.Headers,
	})
	if err != nil {
		return nil, fmt.Errorf("sessionsummary chat: %w", err)
	}
	tokenParam, err := llmclient.ParseChatTokenParam(string(cfg.TokenParam))
	if err != nil {
		return nil, fmt.Errorf("sessionsummary chat: %w", err)
	}
	if cfg.ReasoningTokenAllowance < 0 {
		return nil, errors.New("sessionsummary chat: reasoning token allowance must not be negative")
	}
	c := &ChatClient{client: client, tokenParam: tokenParam, allowance: cfg.ReasoningTokenAllowance, extraBody: cfg.ExtraBody}
	if _, err := llmclient.WithExtraFields(c.request(ToolRequest{}), cfg.ExtraBody); err != nil {
		return nil, fmt.Errorf("sessionsummary chat: %w", err)
	}
	return c, nil
}

type chatToolRequest struct {
	Model string `json:"model"`
	llmclient.ChatTokenLimit
	Messages   []chatMessage  `json:"messages"`
	Tools      []chatTool     `json:"tools"`
	ToolChoice chatToolChoice `json:"tool_choice"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatToolChoice struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type chatToolAnswer struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (c *ChatClient) request(request ToolRequest) chatToolRequest {
	body := chatToolRequest{
		Model:          request.ModelID,
		ChatTokenLimit: c.tokenParam.Limit(int(request.MaxTokens) + c.allowance),
		Messages: []chatMessage{
			{Role: "system", Content: request.System},
			{Role: "user", Content: request.User},
		},
		Tools: []chatTool{{Type: "function", Function: chatFunction{
			Name: request.ToolName, Description: request.ToolDescription, Parameters: request.ToolSchema,
		}}},
		ToolChoice: chatToolChoice{Type: "function"},
	}
	body.ToolChoice.Function.Name = request.ToolName
	return body
}

func (c *ChatClient) CallTool(ctx context.Context, request ToolRequest) (json.RawMessage, Usage, error) {
	payload, err := llmclient.WithExtraFields(c.request(request), c.extraBody)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("chat: %w", err)
	}
	raw, err := c.client.PostJSON(ctx, "/chat/completions", payload, gatewaySummaryMaxResponseBytes)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("chat: %w", err)
	}
	var answer chatToolAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, Usage{}, fmt.Errorf("chat: decode response: %w", err)
	}
	usage := Usage{InputTokens: answer.Usage.PromptTokens, OutputTokens: answer.Usage.CompletionTokens}
	if len(answer.Choices) != 1 {
		return nil, usage, fmt.Errorf("chat: response had %d choices, want 1", len(answer.Choices))
	}
	choice := answer.Choices[0]
	if choice.FinishReason == "length" {
		return nil, usage, fmt.Errorf("response truncated at %s (%d)", c.tokenParam, int(request.MaxTokens)+c.allowance)
	}
	calls := choice.Message.ToolCalls
	switch {
	case len(calls) == 0:
		return nil, usage, errors.New("no tool call in response")
	case len(calls) > 1:
		return nil, usage, errors.New("multiple tool calls in response")
	case calls[0].Function.Name != request.ToolName:
		return nil, usage, fmt.Errorf("unexpected tool call %q, want %q", calls[0].Function.Name, request.ToolName)
	}
	return json.RawMessage(calls[0].Function.Arguments), usage, nil
}

var (
	_ ToolClient = (*ResponsesClient)(nil)
	_ ToolClient = (*ChatClient)(nil)
)
