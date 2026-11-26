package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
)

func TestResponsesTextWorkersUseNativeInputs(t *testing.T) {
	for _, test := range []struct {
		role  modelregistry.Role
		input any
		text  string
	}{
		{modelregistry.RoleImageCaptions, []llmclient.ResponsesMessage{{Role: "user", Content: []llmclient.ResponsesPart{
			{Type: "input_image", ImageURL: "data:image/png;base64,AQID"},
			{Type: "input_text", Text: "Describe and transcribe the image."},
		}}}, "CAPTION: A red square."},
		{modelregistry.RoleDocumentExtraction, "input_file", "--- PAGE 1 ---\nFixture document."},
		{modelregistry.RoleThreadSummaries, "Fixture thread.", "The thread discusses a migration."},
	} {
		t.Run(string(test.role), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/v1/responses", r.URL.Path)
				require.Equal(t, "Bearer fixture", r.Header.Get("Authorization"))
				var body struct {
					Model string          `json:"model"`
					Input json.RawMessage `json:"input"`
					Store bool            `json:"store"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, "vision", body.Model)
				require.False(t, body.Store)
				switch test.role {
				case modelregistry.RoleImageCaptions:
					var messages []llmclient.ResponsesMessage
					require.NoError(t, json.Unmarshal(body.Input, &messages))
					require.Equal(t, test.input, messages)
				case modelregistry.RoleDocumentExtraction:
					var messages []llmclient.ResponsesMessage
					require.NoError(t, json.Unmarshal(body.Input, &messages))
					require.Len(t, messages, 1)
					require.Len(t, messages[0].Content, 2)
					require.Equal(t, llmclient.ResponsesPart{
						Type: "input_file", FileData: "data:application/pdf;base64,AQID", Filename: "fixture.pdf",
					}, messages[0].Content[0])
					require.Equal(t, "input_text", messages[0].Content[1].Type)
				case modelregistry.RoleThreadSummaries:
					var transcript string
					require.NoError(t, json.Unmarshal(body.Input, &transcript))
					require.Equal(t, test.input, transcript)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "completed", "usage": map[string]int{"input_tokens": 8, "output_tokens": 3},
					"output": []map[string]any{{"type": "message", "content": []map[string]string{
						{"type": "output_text", "text": test.text},
					}}},
				})
			}))
			defer server.Close()
			model := boundModel{
				Model:    modelregistry.Model{Provider: "fixture", ID: "vision", Protocols: []modelregistry.Protocol{modelregistry.ProtocolResponses}},
				endpoint: llmclient.Endpoint{BaseURL: server.URL + "/v1", APIKey: "fixture"},
			}
			switch test.role {
			case modelregistry.RoleImageCaptions:
				worker, err := newCaptioner(model)
				require.NoError(t, err)
				got, err := worker.Caption(t.Context(), "image/png", []byte{1, 2, 3})
				require.NoError(t, err)
				require.Equal(t, "A red square.", got.Caption)
			case modelregistry.RoleDocumentExtraction:
				worker, err := newDocumentExtractor(model, nil)
				require.NoError(t, err)
				got, err := worker.Extract(t.Context(), "pdf", "fixture.pdf", []byte{1, 2, 3})
				require.NoError(t, err)
				require.Equal(t, test.text, got.Markdown)
				require.Equal(t, 8, got.InputTokens)
				require.Equal(t, 3, got.OutputTokens)
			case modelregistry.RoleThreadSummaries:
				worker, err := newThreadSummarizer(model)
				require.NoError(t, err)
				got, usage, err := worker.Summarize(t.Context(), test.input.(string))
				require.NoError(t, err)
				require.Equal(t, test.text, got)
				require.Equal(t, 8, usage.PromptTokens)
			}
		})
	}
}
