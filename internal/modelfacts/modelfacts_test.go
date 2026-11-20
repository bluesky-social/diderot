package modelfacts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/testdb"
)

// fixture is the two published lists cut down to the providers the matching
// rules name and the models these tests ask about (testdata/README.md).
func fixture(t *testing.T) *Catalog {
	t.Helper()
	return Merge(fixtureList(t, "models.dev.json", ParseModelsDev), fixtureList(t, "litellm.json", ParseLiteLLM))
}

func fixtureList(t *testing.T, name string, parse func([]byte) (List, error)) List {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	list, err := parse(raw)
	require.NoError(t, err)
	return list
}

func TestParseModelsDevKeepsWhatJobsUse(t *testing.T) {
	c := fixture(t)

	sol, ok := c.Lookup("openai/gpt-6.1-sol")
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-6.1-sol", sol.Canonical)
	assert.True(t, sol.Images)
	assert.True(t, sol.Tools)
	require.NotNil(t, sol.Temperature)
	assert.False(t, *sol.Temperature, "the catalog says it refuses a temperature")
	assert.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, sol.Efforts)
	assert.Equal(t, 1050000, sol.ContextTokens)
	assert.Equal(t, 128000, sol.OutputTokens)
	assert.True(t, sol.Priced)
	assert.InDelta(t, 2.0, sol.InputPrice, 1e-9)
	assert.InDelta(t, 10.0, sol.OutputPrice, 1e-9)

	sonnet, ok := c.Lookup("anthropic/claude-sonnet-5")
	require.True(t, ok)
	assert.True(t, sonnet.Toggle, "its reasoning can be switched off")

	_, ok = c.Lookup("openai/gpt-image-1.5")
	assert.False(t, ok, "a model that answers in pictures is no use to a job")

	embedding, ok := c.Lookup("openai/text-embedding-3-small")
	require.True(t, ok)
	assert.True(t, embedding.Embedding)
	assert.Equal(t, 1536, embedding.Dimensions, "the vector size comes from LiteLLM")
	assert.Zero(t, embedding.OutputTokens)
	assert.Nil(t, embedding.Facts().Images, "an embedding model has no chat facts")
	assert.Equal(t, 1536, embedding.Facts().Dimensions)
}

func TestMergeAddsLiteLLMsRetirementDates(t *testing.T) {
	c := fixture(t)

	vertex, ok := c.Lookup("google-vertex/gemini-3.6-flash")
	require.True(t, ok)
	assert.True(t, vertex.Deprecated, "models.dev marks it deprecated on Vertex")
	assert.Equal(t, "2026-11-19", vertex.Retires, "and LiteLLM dates it")
	assert.Equal(t, "gemini-3.8-flash", vertex.Successor)

	studio, ok := c.Lookup("google/gemini-3.6-flash")
	require.True(t, ok)
	assert.False(t, studio.Deprecated, "a retirement is one provider's, not the model's")
	assert.Empty(t, studio.Retires)

	azure, ok := c.Lookup("azure/gpt-5.4")
	require.True(t, ok)
	assert.Equal(t, "2027-09-02", azure.Retires)
	_, ok = c.Lookup("azure/us/gpt-5.4")
	assert.False(t, ok, "a dated entry models.dev does not describe adds no model")
}

func TestLiteLLMNamespaces(t *testing.T) {
	assert.Equal(t, "google-vertex", LiteLLMNamespace("vertex_ai-language-models"))
	assert.Equal(t, "google", LiteLLMNamespace("gemini"))
	assert.Equal(t, "fireworks-ai", LiteLLMNamespace("fireworks_ai"))
	assert.Equal(t, "gemini-3.6-flash", withoutLiteLLMPrefix("vertex_ai/gemini-3.6-flash", "vertex_ai"))
	assert.Equal(t, "claude-opus-5", withoutLiteLLMPrefix("vertex_ai/claude-opus-5", "vertex_ai-anthropic_models"))
	assert.Equal(t, "us/gpt-6-sol", withoutLiteLLMPrefix("azure/us/gpt-6-sol", "azure"))
	assert.Equal(t, "gemini-3.6-flash", withoutLiteLLMPrefix("gemini-3.6-flash", "vertex_ai-language-models"))
}

// gatewayListing is what a model gateway lists for the models a
// deployment uses: IDs and owners only. Each ":main" model is a virtual route
// the listing says "virtual" owns, beside the model it routes to.
var gatewayListing = map[string]string{
	"gpt-6-sol": "openai", "gpt-6.1-sol": "openai", "gpt-6-astra": "openai", "gpt-5.6-sol": "openai",
	"gpt-5.4-mini": "openai", "gpt-5.4": "openai",
	"claude-opus-5": "anthropic", "claude-sonnet-5": "anthropic", "claude-haiku-4-5": "anthropic",
	"gemini-3.8-flash": "gcp.vertex_ai", "gemini-3.6-flash": "gcp.vertex_ai",
	"deepseek-v4.1-flash": "fireworks", "local-embed": "local-vllm",
	"gpt-6-sol:main": "virtual", "gpt-6.1-sol:main": "virtual", "gpt-6-astra:main": "virtual",
	"gpt-5.6-sol:main": "virtual", "gpt-5.6-sol:bedrock": "virtual", "gpt-5.6-sol:openai": "virtual",
	"gpt-5.4-mini:main": "virtual", "gpt-5.4:main": "virtual",
	"claude-opus-5:main": "virtual", "claude-sonnet-5:main": "virtual", "claude-haiku-4-5:main": "virtual",
	"deepseek-v4.1-flash:main": "virtual",
}

func TestMatchGatewayAliases(t *testing.T) {
	c := fixture(t)
	const gateway = "gateway.example.svc.cluster.local"
	for _, tc := range []struct {
		id, want string
		// suggested is an entry offered for confirmation when none is
		// certain.
		suggested string
	}{
		{id: "gpt-6-sol:main", want: "openai/gpt-6-sol"},
		{id: "gpt-6.1-sol:main", want: "openai/gpt-6.1-sol"},
		{id: "gpt-6-astra:main", want: "openai/gpt-6-astra"},
		{id: "gpt-5.6-sol:main", want: "openai/gpt-5.6-sol"},
		{id: "gpt-5.6-sol:openai", want: "openai/gpt-5.6-sol"},
		{id: "gpt-5.4-mini:main", want: "openai/gpt-5.4-mini"},
		{id: "gpt-5.4:main", want: "openai/gpt-5.4"},
		{id: "claude-opus-5:main", want: "anthropic/claude-opus-5"},
		{id: "claude-sonnet-5:main", want: "anthropic/claude-sonnet-5"},
		{id: "claude-haiku-4-5:main", want: "anthropic/claude-haiku-4-5"},
		{id: "gemini-3.8-flash", want: "google-vertex/gemini-3.8-flash"},
		// Fireworks spells the model another way, so the catalog's entry is
		// only offered.
		{id: "deepseek-v4.1-flash:main", suggested: "fireworks-ai/accounts/fireworks/models/deepseek-v4p1-flash"},
		// Bedrock lists no model of exactly this ID.
		{id: "gpt-5.6-sol:bedrock"},
		// A self-hosted alias no catalog knows.
		{id: "local-embed"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			match := c.Match(Query{ID: tc.id, Host: gateway, Owners: gatewayListing})
			if tc.want == "" {
				assert.False(t, match.Found, "matched %s", match.Entry.Ref())
				if tc.suggested != "" {
					require.NotEmpty(t, match.Suggestions)
					assert.Equal(t, tc.suggested, match.Suggestions[0].Ref(), "the plain model comes before its routers")
				}
				return
			}
			require.True(t, match.Found, "suggestions: %v", refs(match.Suggestions))
			assert.Equal(t, tc.want, match.Entry.Ref())
			assert.True(t, match.ServedBy, "the listing names who serves it")
			assert.NotEmpty(t, match.How)
			assert.False(t, match.Pinned)
		})
	}

	// A model its provider has retired: matched to Vertex's entry, which
	// carries the retirement.
	retired := c.Match(Query{ID: "gemini-3.6-flash", Host: gateway, Owners: gatewayListing})
	require.True(t, retired.Found)
	assert.Equal(t, "google-vertex/gemini-3.6-flash", retired.Entry.Ref())
	assert.Equal(t, "2026-11-19", retired.Entry.Facts().Retires)
	assert.True(t, retired.Entry.Facts().Deprecated)
}

func refs(entries []Entry) []string {
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Ref())
	}
	return out
}

func TestMatchOtherIDStyles(t *testing.T) {
	c := fixture(t)
	for _, tc := range []struct {
		name string
		q    Query
		want string
		// uncertain marks a match that must be confirmed: suggested, when
		// set, is among the entries offered.
		uncertain bool
		suggested string
		served    bool
	}{
		{name: "a provider's own API", q: Query{ID: "gpt-5.4", Host: "api.openai.com"}, want: "openai/gpt-5.4", served: true},
		{name: "its host comes from the catalog", q: Query{ID: "anthropic/claude-opus-4.6", Host: "openrouter.ai"}, want: "openrouter/anthropic/claude-opus-4.6", served: true},
		{name: "a provider's API lists no such model", q: Query{ID: "ft:gpt-5.4:acme::abc", Host: "api.openai.com"}, uncertain: true},
		{name: "the Anthropic kind behind a proxy", q: Query{ID: "claude-sonnet-5", Host: "proxy.example", Anthropic: true}, want: "anthropic/claude-sonnet-5", served: true},
		{name: "a LiteLLM prefix", q: Query{ID: "vertex_ai/gemini-3.6-flash", Host: "litellm.example"}, want: "google-vertex/gemini-3.6-flash", served: true},
		{name: "a LiteLLM prefix for OpenAI", q: Query{ID: "openai/gpt-6-sol", Host: "litellm.example"}, want: "openai/gpt-6-sol", served: true},
		{name: "a Fireworks path anywhere", q: Query{ID: "accounts/fireworks/models/deepseek-v4p1-flash", Host: "gateway.example"}, want: "fireworks-ai/accounts/fireworks/models/deepseek-v4p1-flash", served: true},
		{name: "an OpenRouter spelling elsewhere is only suggested", q: Query{ID: "anthropic/claude-opus-4.6", Host: "gateway.example"}, uncertain: true, suggested: "anthropic/claude-opus-4-6"},
		{name: "a dated snapshot is only suggested", q: Query{ID: "claude-opus-5-20260724", Host: "gateway.example", Owners: map[string]string{"claude-opus-5-20260724": "anthropic"}}, uncertain: true, suggested: "anthropic/claude-opus-5"},
		{name: "a bare ID is its maker's model", q: Query{ID: "gpt-6-astra", Host: "gateway.example"}, want: "openai/gpt-6-astra"},
		{name: "a bare ID whose maker's entry has no canonical name", q: Query{ID: "gpt-5.4", Host: "gateway.example"}, want: "openai/gpt-5.4"},
		{name: "a tag nothing vouches for is not stripped", q: Query{ID: "gpt-6-sol:8b", Host: "ollama.example"}, uncertain: true, suggested: "openai/gpt-6-sol"},
		{name: "a suffix naming a provider is a route", q: Query{ID: "gpt-6-sol:openai", Host: "gateway.example"}, want: "openai/gpt-6-sol", served: true},
		{name: "a model no catalog lists", q: Query{ID: "my-finetune", Host: "localhost"}, uncertain: true},
		{name: "a local server is nobody's API", q: Query{ID: "openai/gpt-oss-20b", Host: "127.0.0.1"}, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			match := c.Match(tc.q)
			if tc.uncertain {
				assert.False(t, match.Found, "matched %s: %s", match.Entry.Ref(), match.How)
				if tc.suggested != "" {
					assert.Contains(t, refs(match.Suggestions), tc.suggested)
				}
				return
			}
			require.True(t, match.Found, "suggestions: %v", refs(match.Suggestions))
			assert.Equal(t, tc.want, match.Entry.Ref())
			assert.Equal(t, tc.served, match.ServedBy)
		})
	}
}

func TestMatchWithoutAServerShowsRetirementsElsewhere(t *testing.T) {
	c := fixture(t)
	match := c.Match(Query{ID: "gemini-3.6-flash", Host: "gateway.example"})
	require.True(t, match.Found)
	assert.Equal(t, "google/gemini-3.6-flash", match.Entry.Ref(), "its maker's entry")
	assert.False(t, match.ServedBy)
	assert.Empty(t, match.Entry.Retires, "Google's own API has no retirement for it")
	require.Len(t, match.Elsewhere, 1)
	assert.Equal(t, "google-vertex/gemini-3.6-flash", match.Elsewhere[0].Ref())
	assert.Equal(t, "2026-11-19", match.Elsewhere[0].Retires, "but Vertex retires it, which the page must say")
}

func TestMatchPinned(t *testing.T) {
	c := fixture(t)
	pinned := c.Match(Query{ID: "deepseek-v4.1-flash:main", Pinned: "fireworks-ai/accounts/fireworks/models/deepseek-v4p1-flash"})
	require.True(t, pinned.Found)
	assert.True(t, pinned.Pinned)
	assert.True(t, pinned.ServedBy)

	cleared := c.Match(Query{ID: "gpt-6-sol", Host: "api.openai.com", Pinned: modelregistry.CatalogNone})
	assert.False(t, cleared.Found)
	assert.True(t, cleared.Cleared)

	gone := c.Match(Query{ID: "gpt-6-sol", Pinned: "openai/gpt-0"})
	assert.False(t, gone.Found)
	assert.True(t, gone.Missing)

	assert.False(t, Merge().Match(Query{ID: "gpt-6-sol", Host: "api.openai.com"}).Found, "an empty catalog matches nothing")
	var none *Catalog
	assert.False(t, none.Match(Query{ID: "gpt-6-sol"}).Found)
}

func TestEntryFacts(t *testing.T) {
	c := fixture(t)
	facts := func(ref string) modelregistry.Facts {
		entry, ok := c.Lookup(ref)
		require.True(t, ok, ref)
		return entry.Facts()
	}
	sonnet := facts("anthropic/claude-sonnet-5")
	assert.Equal(t, []string{"none", "low", "medium", "high", "xhigh", "max"}, sonnet.Efforts, "a switch is the effort none")
	opus := facts("anthropic/claude-opus-5")
	assert.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, opus.Efforts, "no switch, so no none")
	haiku := facts("anthropic/claude-haiku-4-5")
	require.NotNil(t, haiku.Reasons)
	assert.True(t, *haiku.Reasons)
	assert.Nil(t, haiku.Efforts, "a token budget is no control Diderot sends")
	gemini := facts("google-vertex/gemini-3.8-flash")
	require.NotNil(t, gemini.Images)
	assert.True(t, *gemini.Images)
	require.NotNil(t, gemini.InputPrice)
	assert.InDelta(t, 0.75, *gemini.InputPrice, 1e-9)
}

func TestHostNamespaceIgnoresLocalAddresses(t *testing.T) {
	c := fixture(t)
	assert.Equal(t, "openrouter", c.hostNamespace("openrouter.ai"))
	assert.Equal(t, "openai", c.hostNamespace("API.OpenAI.com"))
	assert.Equal(t, "amazon-bedrock", c.hostNamespace("bedrock-runtime.us-east-1.amazonaws.com"))
	assert.Equal(t, "azure", c.hostNamespace("acme.openai.azure.com"))
	assert.Empty(t, c.hostNamespace("127.0.0.1"), "LM Studio's catalog address is every local server's")
	assert.Equal(t, "minimax", c.hostNamespace("api.minimax.io"), "a host two namespaces share names the provider, not its plan")
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testdb.URL(t, "modelfacts"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(t.Context(), `DELETE FROM source_state WHERE source = $1`, stateSource)
	require.NoError(t, err)
	return pool
}

// published serves the two fixture lists, and records what each request
// carried.
type published struct {
	*httptest.Server
	requests atomic.Int64
	headers  atomic.Value // http.Header of the latest request
	// fail makes every answer an HTTP 503.
	fail atomic.Bool
}

func servePublished(t *testing.T) *published {
	t.Helper()
	p := &published{}
	mux := http.NewServeMux()
	serve := func(file, etag string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p.requests.Add(1)
			p.headers.Store(r.Header.Clone())
			if p.fail.Load() {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			http.ServeFile(w, r, "testdata/"+file)
		}
	}
	mux.HandleFunc("GET /api.json", serve("models.dev.json", `"m1"`))
	mux.HandleFunc("GET /litellm.json", serve("litellm.json", `"l1"`))
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

func (p *published) fetcher(now *time.Time) Fetcher {
	return Fetcher{
		URLs: map[string]string{SourceModelsDev: p.URL + "/api.json", SourceLiteLLM: p.URL + "/litellm.json"},
		Now:  func() time.Time { return *now },
	}
}

func TestRefreshFetchesKeepsAndServesTheLists(t *testing.T) {
	if os.Getenv("DIDEROT_TEST_DB_URL") == "" {
		t.Skip("DIDEROT_TEST_DB_URL not set")
	}
	pool := testPool(t)
	server := servePublished(t)
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	fetcher := server.fetcher(&now)
	reader := NewReader(pool)
	ctx := context.Background()

	catalog, status, err := reader.Load(ctx)
	require.NoError(t, err)
	assert.True(t, catalog.Empty(), "nothing is fetched yet")
	assert.Empty(t, status)

	status, err = fetcher.Refresh(ctx, pool, false)
	require.NoError(t, err)
	assert.Equal(t, int64(2), server.requests.Load())
	assert.Empty(t, status[SourceModelsDev].Error)
	assert.Equal(t, now, status[SourceModelsDev].FetchedAt)
	assert.Positive(t, status[SourceLiteLLM].Entries)

	// The request says nothing about the deployment.
	sent := server.headers.Load().(http.Header)
	for name := range sent {
		assert.Contains(t, []string{"Accept", "Accept-Encoding", "User-Agent"}, name)
	}
	assert.Equal(t, "Go-http-client/1.1", sent.Get("User-Agent"), "Go's own, with no name or version of Diderot")

	catalog, _, err = reader.Load(ctx)
	require.NoError(t, err)
	entry, ok := catalog.Lookup("google-vertex/gemini-3.6-flash")
	require.True(t, ok)
	assert.Equal(t, "2026-11-19", entry.Retires)
	again, _, err := reader.Load(ctx)
	require.NoError(t, err)
	assert.Same(t, catalog, again, "an unchanged catalog is not read again")

	// Within a day nothing is fetched; after one, an unchanged list is not
	// downloaded again.
	now = now.Add(23 * time.Hour)
	_, err = fetcher.Refresh(ctx, pool, false)
	require.NoError(t, err)
	assert.Equal(t, int64(2), server.requests.Load())
	now = now.Add(2 * time.Hour)
	status, err = fetcher.Refresh(ctx, pool, false)
	require.NoError(t, err)
	assert.Equal(t, int64(4), server.requests.Load())
	assert.Contains(t, []string{`"m1"`, `"l1"`}, server.headers.Load().(http.Header).Get("If-None-Match"),
		"only the version of the copy held is sent back")
	assert.Equal(t, now, status[SourceModelsDev].FetchedAt, "unchanged counts as read")
	catalog, _, err = reader.Load(ctx)
	require.NoError(t, err)
	assert.False(t, catalog.Empty(), "the copy is kept")
}

func TestRefreshFailureKeepsTheLastCopyAndGoesStale(t *testing.T) {
	if os.Getenv("DIDEROT_TEST_DB_URL") == "" {
		t.Skip("DIDEROT_TEST_DB_URL not set")
	}
	pool := testPool(t)
	server := servePublished(t)
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	fetched := now
	fetcher := server.fetcher(&now)
	reader := NewReader(pool)
	ctx := context.Background()
	_, err := fetcher.Refresh(ctx, pool, false)
	require.NoError(t, err)

	server.fail.Store(true)
	now = now.Add(25 * time.Hour)
	status, err := fetcher.Refresh(ctx, pool, false)
	require.NoError(t, err, "a list that can't be read is not an error of the worker")
	assert.Equal(t, "it answered HTTP 503 Service Unavailable", status[SourceModelsDev].Error)
	assert.Equal(t, fetched, status[SourceModelsDev].FetchedAt)
	assert.Equal(t, now, status[SourceModelsDev].FailingSince)
	catalog, _, err := reader.Load(ctx)
	require.NoError(t, err)
	_, ok := catalog.Lookup("openai/gpt-6-sol")
	assert.True(t, ok, "the last copy still serves")
	_, _, _, stale := status.StaleSince(now)
	assert.False(t, stale, "a day of failures needs nobody")

	// A failed read is tried again after an hour, not at once.
	requests := server.requests.Load()
	now = now.Add(30 * time.Minute)
	_, err = fetcher.Refresh(ctx, pool, false)
	require.NoError(t, err)
	assert.Equal(t, requests, server.requests.Load())
	now = now.Add(time.Hour)
	_, err = fetcher.Refresh(ctx, pool, false)
	require.NoError(t, err)
	assert.Equal(t, requests+2, server.requests.Load())

	now = fetched.Add(StaleAfter + time.Hour)
	status, err = fetcher.Refresh(ctx, pool, false)
	require.NoError(t, err)
	since, source, reason, stale := status.StaleSince(now)
	require.True(t, stale, "a week of failures does")
	assert.Equal(t, fetched, since)
	assert.Equal(t, SourceModelsDev, source)
	assert.Contains(t, reason, "HTTP 503")

	// A read that works again clears it.
	server.fail.Store(false)
	now = now.Add(2 * time.Hour)
	status, err = fetcher.Refresh(ctx, pool, false)
	require.NoError(t, err)
	_, _, _, stale = status.StaleSince(now)
	assert.False(t, stale)
	assert.True(t, status[SourceModelsDev].FailingSince.IsZero())
}

func TestRefreshNeverFetchedGoesStaleFromTheFirstFailure(t *testing.T) {
	if os.Getenv("DIDEROT_TEST_DB_URL") == "" {
		t.Skip("DIDEROT_TEST_DB_URL not set")
	}
	pool := testPool(t)
	server := servePublished(t)
	server.fail.Store(true)
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	first := now
	fetcher := server.fetcher(&now)
	status, err := fetcher.Refresh(context.Background(), pool, false)
	require.NoError(t, err)
	_, _, _, stale := status.StaleSince(now)
	assert.False(t, stale)
	now = now.Add(StaleAfter + time.Hour)
	status, err = fetcher.Refresh(context.Background(), pool, true)
	require.NoError(t, err)
	since, _, _, stale := status.StaleSince(now)
	assert.True(t, stale)
	assert.Equal(t, first, since)
	assert.True(t, slices.Contains([]string{SourceModelsDev, SourceLiteLLM}, SourceModelsDev))
}
