package llmclient

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
