package modelregistry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func provider(kind Kind, url string) Provider {
	return Provider{Name: "p", Kind: kind, Endpoint: llmclient.Endpoint{BaseURL: url, APIKey: "test-key", Auth: kind.Auth()}}
}

func TestListOpenAICompatibleReadsKnownMetadata(t *testing.T) {
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-b","object":"model","owned_by":"openai","shutdown_date":"2026-12-01"},
			{"id":"router/model","name":"Router Model","context_length":200000,
			 "architecture":{"input_modalities":["text","image"]},
			 "pricing":{"prompt":"0.000003","completion":"0.000015"},"expiration_date":"2027-01-31"},
			{"id":"together-embed","type":"embedding","pricing":{"input":0.02,"output":0}},
			{"id":"gw-virtual:main","owned_by":"virtual"},
			{"id":"text-only","architecture":{"input_modalities":["text"]},"context_window":8192,"status":"deprecated"},
			{"id":""},
			{"id":"gpt-b"}
		]}`))
	}))
	defer server.Close()

	listed, err := List(context.Background(), server.Client(), provider(KindOpenAI, server.URL+"/v1"))
	require.NoError(t, err)
	assert.Equal(t, "Bearer test-key", auth)
	require.Len(t, listed, 5)
	byID := map[string]Listed{}
	for _, model := range listed {
		byID[model.ID] = model
	}
	assert.Equal(t, "2026-12-01", byID["gpt-b"].Retires)
	assert.Nil(t, byID["gpt-b"].Images)

	router := byID["router/model"]
	assert.Equal(t, "Router Model", router.Name)
	assert.Equal(t, 200000, router.ContextTokens)
	require.NotNil(t, router.Images)
	assert.True(t, *router.Images)
	require.NotNil(t, router.InputPrice)
	assert.InDelta(t, 3.0, *router.InputPrice, 1e-9)
	assert.InDelta(t, 15.0, *router.OutputPrice, 1e-9)
	assert.Equal(t, "2027-01-31", router.Retires)

	assert.True(t, byID["together-embed"].Embedding)
	assert.InDelta(t, 0.02, *byID["together-embed"].InputPrice, 1e-9)
	assert.Equal(t, Listed{ID: "gw-virtual:main", OwnedBy: "virtual"}, byID["gw-virtual:main"], "a gateway's route says only who owns it")
	assert.Equal(t, "openai", byID["gpt-b"].OwnedBy)
	assert.False(t, *byID["text-only"].Images)
	assert.True(t, byID["text-only"].Deprecated)
	assert.Equal(t, 8192, byID["text-only"].ContextTokens)
}

func TestListAnthropicFollowsPagesAndReadsCapabilities(t *testing.T) {
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/models", r.URL.Path)
		require.Equal(t, "test-key", r.Header.Get("X-Api-Key"))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Equal(t, anthropicVersion, r.Header.Get("anthropic-version"))
		pages = append(pages, r.URL.Query().Get("after_id"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-a","display_name":"Claude A","max_input_tokens":1000000,
				"lifecycle":"active","retires_at":null,
				"capabilities":{"image_input":{"supported":true},
				  "effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":null,"max":{"supported":true}},
				  "thinking":{"supported":true,"types":{"adaptive":{"supported":true},"disabled":{"supported":true}}}}}],
				"has_more":true,"last_id":"claude-a"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-old","display_name":"Claude Old","lifecycle":"deprecated",
			"retires_at":"2026-11-19T00:00:00Z","capabilities":null}],"has_more":false,"last_id":"claude-old"}`))
	}))
	defer server.Close()

	// The base URL may or may not carry /v1.
	listed, err := List(context.Background(), server.Client(), provider(KindAnthropic, server.URL))
	require.NoError(t, err)
	assert.Equal(t, []string{"", "claude-a"}, pages)
	require.Len(t, listed, 2)
	assert.Equal(t, "Claude A", listed[0].Name)
	assert.Equal(t, 1000000, listed[0].ContextTokens)
	assert.True(t, *listed[0].Images)
	assert.Equal(t, []string{"none", "low", "medium", "high", "max"}, listed[0].Efforts)
	assert.False(t, listed[0].Deprecated)
	assert.True(t, listed[1].Deprecated)
	assert.Equal(t, "2026-11-19", listed[1].Retires)
}

func TestListWithoutAnEndpointOrWithARefusedKey(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusNotFound:            ErrNoListing.Error(),
		http.StatusMethodNotAllowed:    ErrNoListing.Error(),
		http.StatusUnauthorized:        "the provider refused the key (HTTP 401)",
		http.StatusInternalServerError: "the provider answered HTTP 500 Internal Server Error",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "secret upstream detail", status)
		}))
		_, err := List(context.Background(), server.Client(), provider(KindOpenAI, server.URL+"/v1"))
		server.Close()
		require.Error(t, err)
		assert.Equal(t, want, err.Error())
		assert.NotContains(t, err.Error(), "secret upstream detail")
		if status == http.StatusNotFound {
			assert.ErrorIs(t, err, ErrNoListing)
		}
	}
}

func TestListRefusesARedirectAndJunk(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/models", http.StatusFound)
	}))
	defer redirect.Close()
	_, err := List(context.Background(), redirect.Client(), provider(KindOpenAI, redirect.URL))
	assert.EqualError(t, err, "the provider answered HTTP 302 Found")

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>`))
	}))
	defer junk.Close()
	_, err = List(context.Background(), junk.Client(), provider(KindOpenAI, junk.URL))
	assert.EqualError(t, err, "the model list is not JSON in the expected shape")
}
