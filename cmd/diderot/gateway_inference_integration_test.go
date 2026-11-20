//go:build gateway

package main

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/embedder"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/sessionsummary"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/stretchr/testify/require"
)

// gatewayTestProvider is the real model provider the gateway-tagged tests
// call, from the environment. A test skips without it:
//
//	DIDEROT_TEST_PROVIDER_URL  base URL of an OpenAI-compatible provider or gateway, with any /v1
//	DIDEROT_TEST_PROVIDER_KEY  its API key
func gatewayTestProvider(t *testing.T) modelregistry.Provider {
	t.Helper()
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("DIDEROT_TEST_PROVIDER_URL")), "/")
	apiKey := strings.TrimSpace(os.Getenv("DIDEROT_TEST_PROVIDER_KEY"))
	if baseURL == "" || apiKey == "" {
		t.Skip("set DIDEROT_TEST_PROVIDER_URL and DIDEROT_TEST_PROVIDER_KEY to run")
	}
	return modelregistry.Provider{Name: "gateway", Kind: modelregistry.KindOpenAI, Endpoint: llmclient.Endpoint{
		BaseURL: baseURL, APIKey: apiKey, Auth: llmclient.AuthBearer,
	}}
}

// gatewayTestModels are the variables that name each job's model for the
// gateway-tagged tests. A job whose variable is unset is skipped.
var gatewayTestModels = map[modelregistry.Role]string{
	modelregistry.RoleAsk:                  "DIDEROT_TEST_MODEL_ASK",
	modelregistry.RoleTitles:               "DIDEROT_TEST_MODEL_TITLES",
	modelregistry.RoleSessionSummaryMap:    "DIDEROT_TEST_MODEL_SUMMARY_MAP",
	modelregistry.RoleSessionSummaryReduce: "DIDEROT_TEST_MODEL_SUMMARY_REDUCE",
	modelregistry.RoleImageCaptions:        "DIDEROT_TEST_MODEL_CAPTIONS",
	modelregistry.RoleDocumentExtraction:   "DIDEROT_TEST_MODEL_DOCUMENTS",
	modelregistry.RoleThreadSummaries:      "DIDEROT_TEST_MODEL_THREAD_SUMMARIES",
}

// gatewayTestEmbeddingModel names the embedding model.
const gatewayTestEmbeddingModel = "DIDEROT_TEST_MODEL_EMBEDDINGS"

// TestGatewayInferenceSurface is the acceptance test for every Diderot
// inference client against a real model provider. For each job whose model
// is named in the environment it runs the job's own test, as choosing the
// model on the Models page does, builds the client the server builds from
// the entry that test makes, and sends one real request through it.
//
// It is built only with the gateway tag and skips without
// DIDEROT_TEST_PROVIDER_URL and DIDEROT_TEST_PROVIDER_KEY; each job skips
// without its model: DIDEROT_TEST_MODEL_ASK, _TITLES, _SUMMARY_MAP,
// _SUMMARY_REDUCE, _CAPTIONS, _DOCUMENTS, _THREAD_SUMMARIES and
// _EMBEDDINGS. DIDEROT_TEST_PROVIDER_DENIED_KEY, a valid key the provider
// does not authorize for the embedding model, also checks that it is
// refused.
//
//	go test -tags=gateway -v -count=1 -timeout 20m ./cmd/diderot -run TestGatewayInferenceSurface
//
// No credential is ever printed.
func TestGatewayInferenceSurface(t *testing.T) {
	provider := gatewayTestProvider(t)
	apiKey := provider.Endpoint.APIKey
	deniedAPIKey := strings.TrimSpace(os.Getenv("DIDEROT_TEST_PROVIDER_DENIED_KEY"))
	quietLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// bound is role's model as the server would bind it: the entry the
	// role's test makes of the model its variable names.
	bound := func(t *testing.T, role modelregistry.Role) boundModel {
		t.Helper()
		id := strings.TrimSpace(os.Getenv(gatewayTestModels[role]))
		if id == "" {
			t.Skip("set " + gatewayTestModels[role] + " to run")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		entry, err := modelregistry.TestForRole(ctx, provider, modelregistry.NewEntry(provider.Name, id), role, modelregistry.TestOptions{})
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		return boundModel{Model: entry, endpoint: provider.Endpoint}
	}

	t.Run("embeddings", func(t *testing.T) {
		model := strings.TrimSpace(os.Getenv(gatewayTestEmbeddingModel))
		if model == "" {
			t.Skip("set " + gatewayTestEmbeddingModel + " to run")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		dimensions, err := modelregistry.EmbeddingDimensions(ctx, provider, model, nil)
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		client, err := embedder.NewGatewayEmbedder(embedder.GatewayEmbedderConfig{
			BaseURL: provider.Endpoint.BaseURL, APIKey: apiKey, Model: model, Dim: dimensions,
		})
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		vectors, err := client.Embed(ctx, []string{"gateway test document", "gateway test query"})
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		require.Len(t, vectors, 2)
		require.Len(t, vectors[0], dimensions)
		require.Len(t, vectors[1], dimensions)
		require.Equal(t, "self:"+model+":v1", client.ID())

		if deniedAPIKey == "" {
			return
		}
		denied, err := embedder.NewGatewayEmbedder(embedder.GatewayEmbedderConfig{
			BaseURL: provider.Endpoint.BaseURL, APIKey: deniedAPIKey, Model: model, Dim: dimensions,
		})
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		_, err = denied.Embed(ctx, []string{"must be denied"})
		gatewayTestRequireErrorContains(t, err, "403", apiKey, deniedAPIKey)
	})

	t.Run("image caption", func(t *testing.T) {
		model := bound(t, modelregistry.RoleImageCaptions)
		client, err := newCaptioner(model)
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		image, err := base64.StdEncoding.DecodeString(gatewayTestTinyPNG)
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		caption, err := client.Caption(ctx, "image/png", image)
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		require.NotEmpty(t, caption.Caption)
		require.Equal(t, model.ID, caption.ModelID)
	})

	t.Run("pdf document extraction", func(t *testing.T) {
		client, err := newDocumentExtractor(bound(t, modelregistry.RoleDocumentExtraction), quietLogger)
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		result, err := client.Extract(ctx, "pdf", "gateway-test.pdf", []byte(gatewayTestPDF))
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		require.Contains(t, result.Markdown, gatewayTestPDFSentinel)
		require.Positive(t, result.InputTokens)
	})

	t.Run("session title forced tool", func(t *testing.T) {
		model := bound(t, modelregistry.RoleTitles)
		client, err := newTitleGenerator(model, "")
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		result, err := client.Generate(ctx, "Move every inference call to the provider chosen on the Models page.")
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		require.NotEmpty(t, result.Title)
		require.Equal(t, model.ID, result.ModelID)
	})

	t.Run("thread summary", func(t *testing.T) {
		client, err := newThreadSummarizer(bound(t, modelregistry.RoleThreadSummaries))
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		result, usage, err := client.Summarize(ctx, "Alice: Large changes must be tested against the staging provider first.\nBob: Agreed, I will run the inference acceptance tests.")
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		require.NotEmpty(t, result)
		require.Positive(t, usage.PromptTokens)
	})

	t.Run("session summary map and reduce", func(t *testing.T) {
		mapModel := bound(t, modelregistry.RoleSessionSummaryMap)
		reduceModel := bound(t, modelregistry.RoleSessionSummaryReduce)
		client, err := sessionsummary.NewGatewayClient(sessionsummary.GatewayClientConfig{
			BaseURL: provider.Endpoint.BaseURL, APIKey: apiKey,
		})
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		result, usage, err := sessionsummary.MapCall(ctx, client, sessionsummary.EmbeddedPromptBundle,
			sessionsummary.CallOptions{ModelID: mapModel.ID, MaxTokens: 1024},
			[]byte(`{"role":"user","content":"verify the session summary path"}`), 1, 1)
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		require.NotEmpty(t, result.Summary)
		require.Positive(t, usage.InputTokens)
		final, reduceUsage, err := sessionsummary.ReduceCall(ctx, client, sessionsummary.EmbeddedPromptBundle,
			sessionsummary.CallOptions{ModelID: reduceModel.ID, MaxTokens: 4096},
			[]sessionsummary.ChunkSummary{result})
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		require.NotEmpty(t, final.Narrative)
		require.Positive(t, reduceUsage.InputTokens)
	})

	t.Run("ask agent tool turn", func(t *testing.T) {
		model, err := ask.NewModel(t.Context(), ask.ModelConfig{
			BaseURL: provider.Endpoint.BaseURL, APIKey: apiKey, Model: bound(t, modelregistry.RoleAsk).Model,
			Timeout: 2 * time.Minute, MaxTokens: 4096,
		})
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)

		const nonce = "gateway-ask-tool-gate"
		var called atomic.Bool
		probeTool, err := utils.InferTool("gateway_next_probe", "Call this tool exactly once with the requested nonce.",
			func(_ context.Context, input gatewayTestProbeInput) (string, error) {
				require.Equal(t, nonce, input.Nonce)
				called.Store(true)
				return `{"verified":true}`, nil
			})
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)

		agents, err := ask.NewAskAgents(t.Context(), model, []tool.BaseTool{probeTool},
			"Always call gateway_next_probe with the user's nonce before answering. After it succeeds, return a concise confirmation.", 4)
		gatewayTestRequireNoError(t, err, apiKey, deniedAPIKey)
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		iter := agents.HTTP.Query(ctx, "Run the gateway probe with nonce "+nonce+" and then answer.")
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			gatewayTestRequireNoError(t, event.Err, apiKey, deniedAPIKey)
		}
		require.True(t, called.Load(), "the model did not complete the forced tool call")
	})
}

func gatewayTestRequireNoError(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		return
	}
	require.FailNow(t, "gateway request failed", "%s", gatewayTestRedact(err.Error(), secrets...))
}

func gatewayTestRequireErrorContains(t *testing.T, err error, want string, secrets ...string) {
	t.Helper()
	if err == nil {
		require.FailNow(t, "gateway request unexpectedly succeeded")
	}
	safe := gatewayTestRedact(err.Error(), secrets...)
	if !strings.Contains(safe, want) {
		require.FailNow(t, "gateway request returned an unexpected error", "%s", safe)
	}
}

func gatewayTestRedact(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

type gatewayTestProbeInput struct {
	Nonce string `json:"nonce"`
}

const gatewayTestTinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

const gatewayTestPDFSentinel = "Model provider smoke test succeeded"

const gatewayTestPDF = `%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>
endobj
4 0 obj
<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>
endobj
5 0 obj
<< /Length 68 >>
stream
BT /F1 24 Tf 100 700 Td (Model provider smoke test succeeded) Tj ET
endstream
endobj
xref
0 6
0000000000 65535 f
0000000009 00000 n
0000000058 00000 n
0000000115 00000 n
0000000241 00000 n
0000000311 00000 n
trailer
<< /Size 6 /Root 1 0 R >>
startxref
428
%%EOF
`
