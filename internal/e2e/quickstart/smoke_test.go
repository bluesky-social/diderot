// Package quickstart holds the end-to-end smoke test for the Docker Compose
// quickstart (compose.quickstart.yaml). scripts/quickstart-smoke.sh brings the
// stack up with the stub model provider (compose.smoke.yaml), runs
// TestQuickstartSmoke against it, and tears it down; `just quickstart-smoke`
// and the quickstart CI workflow run that script. Without the
// DIDEROT_QUICKSTART_SMOKE_* variables the test skips.
package quickstart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bluesky-social/diderot/internal/events"
)

const (
	question = "What is the quickstart smoke codeword?"
	// sentinel appears only in the codeword document, so an Ask reply that
	// contains it proves the answer came through retrieval of the corpus.
	sentinel   = "periwinkle-kestrel-7781"
	codewordID = "quickstart-smoke:codeword"
)

// fixture is a small corpus: one document that answers the question and two
// that should rank below it.
var fixture = []events.Event{
	{
		ExternalID: codewordID,
		Title:      "Quickstart smoke codeword",
		Body: "The quickstart smoke codeword is " + sentinel + ". The smoke test asks for the " +
			"codeword to prove that ingest, embedding, retrieval, and Ask work end to end.",
	},
	{
		ExternalID: "quickstart-smoke:deploys",
		Title:      "Deploy checklist",
		Body:       "Deploys run database migrations first, then roll the new pods once readiness passes.",
	},
	{
		ExternalID: "quickstart-smoke:oncall",
		Title:      "On-call handoff",
		Body:       "The outgoing on-call engineer posts open incidents and pending alerts in the handoff thread.",
	},
}

type smokeEnv struct {
	baseURL string
	token   string
	dbURL   string
}

func loadSmokeEnv(t *testing.T) smokeEnv {
	t.Helper()
	env := smokeEnv{
		baseURL: strings.TrimRight(os.Getenv("DIDEROT_QUICKSTART_SMOKE_URL"), "/"),
		token:   os.Getenv("DIDEROT_QUICKSTART_SMOKE_TOKEN"),
		dbURL:   os.Getenv("DIDEROT_QUICKSTART_SMOKE_DB_URL"),
	}
	if env.baseURL == "" || env.token == "" || env.dbURL == "" {
		t.Skip("DIDEROT_QUICKSTART_SMOKE_URL, _TOKEN, and _DB_URL are not set; run scripts/quickstart-smoke.sh")
	}
	return env
}

func TestQuickstartSmoke(t *testing.T) {
	env := loadSmokeEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	waitFor(ctx, t, "readiness", func() error {
		status, body, err := env.call(ctx, http.MethodGet, "/readyz", "", nil)
		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("status %d: %s", status, body)
		}
		return err
	})

	t.Run("API rejects a request without the token", func(t *testing.T) {
		status, body, err := env.call(ctx, http.MethodPost, "/v1/retrieve", "", map[string]any{"question": question})
		if err != nil {
			t.Fatal(err)
		}
		if status != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401: %s", status, body)
		}
	})

	// The init container generated the browser session key and the config
	// key: the sign-in page offers the bootstrap admin and the API token,
	// and an administrator who signs in with the token reaches the setup
	// checklist and can paste connector secrets.
	t.Run("the bootstrap admin can sign in", func(t *testing.T) {
		status, body, err := env.call(ctx, http.MethodGet, "/auth/login", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if status != http.StatusOK || !strings.Contains(body, `id="bootstrap-heading"`) {
			t.Fatalf("status %d, want 200 with the bootstrap admin form: %s", status, body)
		}
	})
	t.Run("the API token signs in to the browser admin", func(t *testing.T) {
		browser := signIn(ctx, t, env)
		status, body := browser.get(ctx, t, "/admin/settings")
		if status != http.StatusOK || !strings.Contains(body, "Setup checklist") || !strings.Contains(body, `id="go-live"`) {
			t.Fatalf("status %d, want 200 with the setup checklist: %s", status, body)
		}
		status, body = browser.get(ctx, t, "/admin/connectors/notion")
		if status != http.StatusOK || !strings.Contains(body, `name="secret.`) {
			t.Fatalf("status %d, want 200 with a field to paste the Notion token in: %s", status, body)
		}
	})
	t.Run("the browser admin refuses a request that has not signed in", func(t *testing.T) {
		status, body := (&browser{env: env}).get(ctx, t, "/admin/settings")
		if status != http.StatusSeeOther && status != http.StatusUnauthorized {
			t.Fatalf("status %d, want a redirect to the sign-in page: %s", status, body)
		}
	})

	// The server started with no model configured. Before one is chosen,
	// search answers that it is not set up.
	status, body, err := env.call(ctx, http.MethodPost, "/v1/retrieve", env.token, map[string]any{"question": question})
	if err != nil {
		t.Fatal(err)
	}
	if status == http.StatusOK || !strings.Contains(body, "not set up") {
		t.Fatalf("retrieve before any model is chosen answered %d: %s", status, body)
	}
	chooseModels(ctx, t, signIn(ctx, t, env))

	ingestFixture(ctx, t, env.dbURL)

	// The embedder worker polls for unembedded chunks; retrieval finds the
	// codeword document once its chunk is in Qdrant.
	waitFor(ctx, t, "the fixture to be embedded and retrieved first", func() error {
		var result struct {
			Hits []struct {
				ExternalID string `json:"external_id"`
				Content    string `json:"content"`
			} `json:"hits"`
		}
		if err := env.callJSON(ctx, "/v1/retrieve", map[string]any{"question": question, "top_k": 3}, &result); err != nil {
			return err
		}
		if len(result.Hits) == 0 {
			return fmt.Errorf("no hits yet")
		}
		if result.Hits[0].ExternalID != codewordID {
			return fmt.Errorf("top hit is %q, want %q", result.Hits[0].ExternalID, codewordID)
		}
		if !strings.Contains(result.Hits[0].Content, sentinel) {
			return fmt.Errorf("top hit does not carry the codeword: %q", result.Hits[0].Content)
		}
		return nil
	})

	// The stub model calls retrieve for the question, then answers by quoting
	// what retrieval returned. The server answers Ask once it has built it,
	// a moment after it starts listening, so 503 answers are retried.
	var answer struct {
		SessionID string `json:"session_id"`
		Reply     string `json:"reply"`
	}
	waitFor(ctx, t, "an Ask answer", func() error {
		err := env.callJSON(ctx, "/v1/ask", map[string]any{"question": question}, &answer)
		var status statusError
		if errors.As(err, &status) && status.code != http.StatusServiceUnavailable {
			t.Fatalf("ask: %v", err)
		}
		return err
	})
	t.Logf("ask session %s replied: %q", answer.SessionID, answer.Reply)
	if !strings.Contains(answer.Reply, sentinel) {
		t.Fatalf("Ask reply does not contain the retrieved codeword %q: %q", sentinel, answer.Reply)
	}

	t.Run("an MCP client lists the corpus with the token", func(t *testing.T) { checkMCP(ctx, t, env.baseURL+"/mcp", env.token) })
}

// mcpTools is every tool the corpus MCP server offers.
var mcpTools = []string{"ask", "attachment", "expand", "list", "retrieve"}

// checkMCP connects to the MCP endpoint as a client holding the API token
// (initialize), checks the tools it offers, and lists the fixture with one.
func checkMCP(ctx context.Context, t *testing.T, endpoint, token string) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "quickstart-smoke", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: bearerTransport{token: token}, Timeout: time.Minute},
	}, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	defer func() { _ = session.Close() }()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, mcpTools) {
		t.Fatalf("tools/list offers %v, want %v", names, mcpTools)
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list", Arguments: map[string]any{"sources": []string{"notion"}}})
	if err != nil {
		t.Fatalf("tools/call list: %v", err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || !strings.Contains(string(raw), codewordID) {
		t.Fatalf("list does not return the fixture document %q: %s", codewordID, raw)
	}

	// Without the token the endpoint answers nothing.
	_, err = mcp.NewClient(&mcp.Implementation{Name: "quickstart-smoke", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: endpoint, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err == nil {
		t.Fatal("an MCP client without the token connected")
	}
}

// bearerTransport presents the API token on every request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// publicOrigin is the quickstart's DIDEROT_PUBLIC_URL, which compose.smoke.yaml
// pins: the sign-in form accepts a post from that origin alone, whatever
// port the smoke reaches the server on.
const publicOrigin = "http://localhost:9091"

// browser is a signed-in browser: the cookies the server set.
type browser struct {
	env     smokeEnv
	cookies []*http.Cookie
}

// signIn signs in to the browser admin with the API token, as the sign-in
// page's token form does.
func signIn(ctx context.Context, t *testing.T, env smokeEnv) *browser {
	t.Helper()
	b := &browser{env: env}
	status, page := b.get(ctx, t, "/auth/login?return_to=/admin/settings")
	// The form's CSRF token is the value of the cookie the page set.
	csrf := slices.IndexFunc(b.cookies, func(c *http.Cookie) bool { return c.Name == "diderot_signin_csrf" })
	if status != http.StatusOK || csrf < 0 || !strings.Contains(page, `value="token"`) {
		t.Fatalf("status %d, want 200 with the token sign-in form and its CSRF cookie: %s", status, page)
	}
	form := url.Values{
		"csrf_token": {b.cookies[csrf].Value}, "method": {"token"}, "token": {env.token}, "return_to": {"/admin/settings"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.baseURL+"/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", publicOrigin)
	status, body := b.do(t, req)
	if status != http.StatusSeeOther {
		t.Fatalf("sign-in answered %d, want a redirect to the checklist: %s", status, body)
	}
	return b
}

func (b *browser) get(ctx context.Context, t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.env.baseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b.do(t, req)
}

// do sends req with the browser's cookies, keeps the ones the response sets,
// and follows no redirect. It keeps a cookie whatever its Secure attribute:
// the smoke reaches the server over plain HTTP on loopback.
func (b *browser) do(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	for _, cookie := range b.cookies {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for _, cookie := range resp.Cookies() {
		b.cookies = slices.DeleteFunc(b.cookies, func(old *http.Cookie) bool { return old.Name == cookie.Name })
		if cookie.MaxAge >= 0 {
			b.cookies = append(b.cookies, cookie)
		}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

// ingestFixture writes the fixture through the same event sink the
// connectors use, as Notion pages (company-wide "internal" scope, which every
// authenticated caller can read). It is the one step that bypasses the
// server. The public ingest path to move to is agent-session push
// (PUT /v1/ingest/agent-sessions/{id}), but pushed sessions are indexed only
// after map/reduce summaries over the Anthropic Messages API, so the stub
// must serve that protocol (with forced tool use) before the smoke can push.
func ingestFixture(ctx context.Context, t *testing.T, dbURL string) {
	t.Helper()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to the quickstart database: %v", err)
	}
	defer pool.Close()
	sink := events.NewDBSink(pool, nil)
	for _, doc := range fixture {
		doc.Source = "notion"
		doc.OccurredAt = time.Now().UTC()
		doc.Actor = "Quickstart Smoke"
		doc.URL = "https://quickstart.invalid/" + doc.ExternalID
		doc.Scope = events.ScopeInternal
		if _, err := sink.Upsert(ctx, doc); err != nil {
			t.Fatalf("ingest %s: %v", doc.ExternalID, err)
		}
	}
	t.Logf("ingested %d fixture documents", len(fixture))
}

// waitFor retries check every two seconds until it succeeds or ctx ends.
func waitFor(ctx context.Context, t *testing.T, what string, check func() error) {
	t.Helper()
	start := time.Now()
	for {
		err := check()
		if err == nil {
			t.Logf("%s: ok after %s", what, time.Since(start).Round(time.Second))
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s: %v", what, err)
		case <-time.After(2 * time.Second):
		}
	}
}

// call sends one request, with auth as the bearer token unless it is empty.
func (e smokeEnv) call(ctx context.Context, method, path, auth string, body any) (int, string, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, "", err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.baseURL+path, reader)
	if err != nil {
		return 0, "", err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(raw), err
}

// statusError is a non-200 API response.
type statusError struct {
	path string
	code int
	body string
}

func (e statusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.path, e.code, e.body)
}

// callJSON posts body with the token and decodes a 200 response into out.
func (e smokeEnv) callJSON(ctx context.Context, path string, body, out any) error {
	status, raw, err := e.call(ctx, http.MethodPost, path, e.token, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return statusError{path: path, code: status, body: strings.TrimSpace(raw)}
	}
	return json.Unmarshal([]byte(raw), out)
}

// The stub's models (internal/e2e/quickstart/stub): one for chat jobs, one
// for embeddings.
const (
	stubChatModel  = "stub-chat"
	stubEmbedModel = "stub-embed"
)

// chooseModels does what an installer does on the Models page: adds the stub
// as a provider with a key, which lists its models, chooses its embedding
// model, and gives Ask its chat model, which tests it.
func chooseModels(ctx context.Context, t *testing.T, b *browser) {
	t.Helper()
	status, body := b.post(ctx, t, "/admin/settings/providers/new", "/admin/settings/providers", url.Values{
		"kind": {"openai"}, "name": {"stub"}, "base_url": {"http://stub:8080/v1"}, "secret.key": {"quickstart-smoke"},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("adding the provider answered %d: %s", status, body)
	}
	status, page := b.get(ctx, t, "/admin/settings/providers/stub")
	if status != http.StatusOK || !strings.Contains(page, stubEmbedModel) || !strings.Contains(page, ">on<") {
		t.Fatalf("the provider's page does not show it on with its models (%d): %s", status, page)
	}
	version := func() string {
		t.Helper()
		_, page := b.get(ctx, t, "/admin/settings/models")
		match := regexp.MustCompile(`action="/admin/settings/models/embedding">\s*<input type="hidden" name="csrf_token" value="[^"]*">\s*<input type="hidden" name="version" value="(\d+)"`).FindStringSubmatch(page)
		if match == nil {
			t.Fatalf("no models version on the Models page: %s", page)
		}
		return match[1]
	}
	status, body = b.post(ctx, t, "/admin/settings/models", "/admin/settings/models/embedding", url.Values{
		"version": {version()}, "embedding": {"stub/" + stubEmbedModel},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("choosing the embedding model answered %d: %s", status, body)
	}
	status, body = b.post(ctx, t, "/admin/settings/models", "/admin/settings/models", url.Values{
		"version": {version()}, "role.ask": {"stub/" + stubChatModel}, "ask_reasoning_effort": {""}, "ask_max_tool_turns": {"8"},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("giving Ask its model answered %d: %s", status, body)
	}
}

// post submits a form of the admin page at page to action, with the CSRF
// token the page's cookie carries, as the page's form would.
func (b *browser) post(ctx context.Context, t *testing.T, page, action string, form url.Values) (int, string) {
	t.Helper()
	if status, body := b.get(ctx, t, page); status != http.StatusOK {
		t.Fatalf("GET %s answered %d: %s", page, status, body)
	}
	csrf := slices.IndexFunc(b.cookies, func(c *http.Cookie) bool { return c.Name == "diderot_admin_csrf" })
	if csrf < 0 {
		t.Fatalf("GET %s set no CSRF cookie", page)
	}
	form.Set("csrf_token", b.cookies[csrf].Value)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.env.baseURL+action, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", publicOrigin)
	return b.do(t, req)
}
