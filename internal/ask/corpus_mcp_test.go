package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/retrieve"
)

type fakeCorpusRetriever struct {
	retrieveReq   retrieve.Request
	retrieveCalls int
	listReq       retrieve.ListRequest
	result        *retrieve.Result
	retrieveErr   error
	listErr       error
}

func (f *fakeCorpusRetriever) Retrieve(_ context.Context, req retrieve.Request) (*retrieve.Result, error) {
	f.retrieveReq = req
	f.retrieveCalls++
	return f.result, f.retrieveErr
}

// fakeExactReferenceResolver stands in for the production exact resolver and,
// like it, gates every handle on the caller's scopes before returning content:
// an item outside them is unavailable exactly like a missing one.
type fakeExactReferenceResolver struct {
	items   map[string]fakeExactReferenceItem // keyed by CorpusReference.Value
	err     error
	calls   int
	refs    []CorpusReference
	allowed []string
}

type fakeExactReferenceItem struct {
	scope    string
	resolved ResolvedReference
}

func (f *fakeExactReferenceResolver) PrepareReferences(_ context.Context, refs []CorpusReference, allowed []string) (ReferencePrerequisiteState, error) {
	f.calls++
	f.refs = append([]CorpusReference(nil), refs...)
	f.allowed = append([]string(nil), allowed...)
	if f.err != nil {
		return ReferencePrerequisiteState{}, f.err
	}
	state := ReferencePrerequisiteState{References: refs}
	for _, ref := range refs {
		item, ok := f.items[ref.Value]
		if !ok || !events.ScopeAllowed(item.scope, allowed) {
			state.Unavailable = append(state.Unavailable, ref)
			state.PrimaryBlocked = state.PrimaryBlocked || ref.Role == ReferencePrimary
			continue
		}
		resolved := item.resolved
		resolved.Reference = ref
		state.Resolved = append(state.Resolved, resolved)
	}
	return state, nil
}

func (f *fakeCorpusRetriever) List(_ context.Context, req retrieve.ListRequest) (*retrieve.Result, error) {
	f.listReq = req
	return f.result, f.listErr
}

type fakeCorpusAskRunner struct {
	mu       sync.Mutex
	owner    string
	request  TurnRequest
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
	// byCaller is whether the turn's context, once canceled is closed,
	// ended with ErrTurnCancelled.
	byCaller   bool
	result     TurnResult
	err        error
	startOnce  sync.Once
	cancelOnce sync.Once
	// closed refuses every Hold; held counts the tasks holding the runner.
	closed bool
	held   int
	// created counts CreateOwnedSession calls; sessions maps each session to
	// its owner; artifacts holds bytes by "<session>/<artifact>".
	created   int
	sessions  map[uuid.UUID]string
	artifacts map[string][]byte
}

type fakeCorpusAskNotifier struct {
	mu       sync.Mutex
	done     chan struct{}
	calls    int
	owner    string
	toolName string
	session  string
	question string
}

func newFakeCorpusAskNotifier() *fakeCorpusAskNotifier {
	return &fakeCorpusAskNotifier{done: make(chan struct{}, 1)}
}

func (f *fakeCorpusAskNotifier) NotifyAskInvocation(_ context.Context, owner, session, question string) error {
	return f.record(owner, "ask", session, question)
}

func (f *fakeCorpusAskNotifier) NotifyMCPInvocation(_ context.Context, owner, toolName, session, question string) error {
	return f.record(owner, toolName, session, question)
}

func (f *fakeCorpusAskNotifier) record(owner, toolName, session, question string) error {
	f.mu.Lock()
	f.calls++
	f.owner = owner
	f.toolName = toolName
	f.session = session
	f.question = question
	f.mu.Unlock()
	f.done <- struct{}{}
	return nil
}

func newFakeCorpusAskRunner() *fakeCorpusAskRunner {
	return &fakeCorpusAskRunner{
		started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{}),
		result: TurnResult{Reply: "the answer"},
	}
}

// fakeCreatedSession is the session CreateOwnedSession hands out.
var fakeCreatedSession = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func (f *fakeCorpusAskRunner) CreateOwnedSession(_ context.Context, owner string) (uuid.UUID, error) {
	f.mu.Lock()
	f.owner = owner
	f.created++
	if f.sessions == nil {
		f.sessions = map[uuid.UUID]string{}
	}
	f.sessions[fakeCreatedSession] = owner
	f.mu.Unlock()
	return fakeCreatedSession, nil
}

// OwnsSession answers from the sessions this runner created or was seeded
// with, as the production rule answers from ask_sessions.owner_email.
func (f *fakeCorpusAskRunner) OwnsSession(_ context.Context, sessionID uuid.UUID, owner string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return owner != "" && f.sessions[sessionID] == owner, nil
}

func (f *fakeCorpusAskRunner) ReadArtifact(_ context.Context, sessionID, artifactID uuid.UUID) (ArtifactRecord, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.artifacts[sessionID.String()+"/"+artifactID.String()]
	if !ok {
		return ArtifactRecord{}, nil, pgx.ErrNoRows
	}
	return ArtifactRecord{ID: artifactID, SessionID: sessionID, MediaType: "image/png"}, data, nil
}

func (f *fakeCorpusAskRunner) ExecuteTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	f.mu.Lock()
	f.request = req
	f.mu.Unlock()
	f.startOnce.Do(func() { close(f.started) })
	select {
	case <-f.release:
		return f.result, f.err
	case <-ctx.Done():
		f.cancelOnce.Do(func() {
			f.byCaller = TurnCancelled(ctx)
			close(f.canceled)
		})
		return TurnResult{}, ctx.Err()
	}
}

func (f *fakeCorpusAskRunner) Hold() (func(), bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, false
	}
	f.held++
	return func() {
		f.mu.Lock()
		f.held--
		f.mu.Unlock()
	}, true
}

func (f *fakeCorpusAskRunner) holding() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newCorpusMCPHTTPServer(t *testing.T, retriever CorpusRetriever, runner CorpusAskRunner) *httptest.Server {
	t.Helper()
	return newCorpusMCPHTTPServerWithBackend(t, retriever, runner, newMemoryCorpusTaskBackend())
}

func newCorpusMCPHTTPServerWithBackend(t *testing.T, retriever CorpusRetriever, runner CorpusAskRunner, backend corpusTaskBackend) *httptest.Server {
	t.Helper()
	return newCorpusMCPHTTPServerWithProvider(t, retriever, func() CorpusAskLease {
		return CorpusAskLease{Runner: runner, Context: t.Context()}
	}, backend)
}

func newCorpusMCPHTTPServerWithProvider(t *testing.T, retriever CorpusRetriever, provider CorpusAskRunnerProvider, backend corpusTaskBackend) *httptest.Server {
	t.Helper()
	return newCorpusMCPHTTPServerWithExpander(t, retriever, &fakeExpander{}, &fakeScopeLookup{}, provider, backend)
}

func newCorpusMCPHTTPServerWithExpander(t *testing.T, retriever CorpusRetriever, expander SourceExpander, scopes ScopeLookup, provider CorpusAskRunnerProvider, backend corpusTaskBackend) *httptest.Server {
	t.Helper()
	return newCorpusMCPHTTPServerWithResolver(t, retriever, expander, scopes, &fakeExactReferenceResolver{}, provider, backend)
}

func newCorpusMCPHTTPServerWithResolver(t *testing.T, retriever CorpusRetriever, expander SourceExpander, scopes ScopeLookup, references ReferencePrerequisiteResolver, provider CorpusAskRunnerProvider, backend corpusTaskBackend) *httptest.Server {
	t.Helper()
	return newCorpusMCPHTTPServerWithAttachments(t, retriever, expander, scopes, &fakeAttachmentReader{}, references, provider, backend)
}

func newCorpusMCPHTTPServerWithAttachments(t *testing.T, retriever CorpusRetriever, expander SourceExpander, scopes ScopeLookup, attachments AttachmentReader, references ReferencePrerequisiteResolver, provider CorpusAskRunnerProvider, backend corpusTaskBackend) *httptest.Server {
	t.Helper()
	srv, err := newCorpusMCPServer(retriever, testSources, expander, scopes, attachments, references, testLinearTeams, provider, backend, t.Context(), "test")
	require.NoError(t, err)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email := r.Header.Get("X-Test-User")
		ctx := WithMCPCaller(r.Context(), email, []string{"public", "internal", "slack-channel:C1"})
		transport.ServeHTTP(w, r.WithContext(ctx))
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func modernMeta(withTasks bool) map[string]any {
	extensions := map[string]any{}
	if withTasks {
		extensions[tasksExtensionID] = map[string]any{}
	}
	return map[string]any{
		mcp.MetaKeyProtocolVersion:    "2026-07-28",
		mcp.MetaKeyClientInfo:         map[string]any{"name": "test-client", "version": "1"},
		mcp.MetaKeyClientCapabilities: map[string]any{"extensions": extensions},
	}
}

func callRPC(t *testing.T, server *httptest.Server, user, method, name string, params map[string]any, withTasks bool) rpcResponse {
	t.Helper()
	params["_meta"] = modernMeta(withTasks)
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("MCP-Method", method)
	if name != "" {
		req.Header.Set("MCP-Name", name)
	}
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, []int{http.StatusOK, http.StatusBadRequest}, resp.StatusCode, string(raw))
	var decoded rpcResponse
	require.NoError(t, json.Unmarshal(raw, &decoded), string(raw))
	return decoded
}

func resultObject(t *testing.T, response rpcResponse) map[string]any {
	t.Helper()
	require.Nil(t, response.Error)
	var result map[string]any
	require.NoError(t, json.Unmarshal(response.Result, &result))
	return result
}

func TestCorpusMCPDiscoveryToolsAndScopedCalls(t *testing.T) {
	retriever := &fakeCorpusRetriever{result: &retrieve.Result{Hits: []retrieve.Hit{{EventID: 7, Content: "answer"}}}}
	server := newCorpusMCPHTTPServer(t, retriever, newFakeCorpusAskRunner())

	discovered := resultObject(t, callRPC(t, server, "", "server/discover", "", map[string]any{}, true))
	require.Equal(t, "complete", discovered["resultType"])
	require.Contains(t, discovered["supportedVersions"], "2026-07-28")
	require.NotNil(t, discovered["capabilities"].(map[string]any)["extensions"].(map[string]any)[tasksExtensionID])

	listed := resultObject(t, callRPC(t, server, "", "tools/list", "", map[string]any{}, true))
	tools := listed["tools"].([]any)
	require.Len(t, tools, 5)
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		names = append(names, raw.(map[string]any)["name"].(string))
	}
	require.ElementsMatch(t, []string{"ask", "attachment", "expand", "list", "retrieve"}, names)

	for _, name := range []string{"retrieve", "list", "expand", "attachment", "ask"} {
		params := map[string]any{"name": name, "arguments": map[string]any{}}
		result := resultObject(t, callRPC(t, server, "", "tools/call", name, params, true))
		require.Equal(t, true, result["isError"])
		require.Contains(t, result["content"].([]any)[0].(map[string]any)["text"], "verified user identity is required")
	}

	result := resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "retrieve", map[string]any{
		"name": "retrieve", "arguments": map[string]any{"question": "gateway decision", "top_k": 5},
	}, true))
	require.NotEqual(t, true, result["isError"])
	require.Equal(t, "gateway decision", retriever.retrieveReq.Question)
	require.Equal(t, []string{"public", "internal", "slack-channel:C1"}, retriever.retrieveReq.Filters.AllowedScopes)
}

func TestCorpusMCPAskTaskCompletesAndIsIdentityScoped(t *testing.T) {
	runner := newFakeCorpusAskRunner()
	server := newCorpusMCPHTTPServer(t, &fakeCorpusRetriever{}, runner)
	create := resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "what changed?"},
	}, true))
	require.Equal(t, "task", create["resultType"])
	require.Equal(t, "working", create["status"])
	taskID := create["taskId"].(string)
	require.NotEmpty(t, taskID)

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("Ask did not start")
	}
	runner.mu.Lock()
	require.Equal(t, "alex@example.com", runner.owner)
	require.Equal(t, "what changed?", runner.request.UserInput)
	require.Equal(t, []string{"public", "internal", "slack-channel:C1"}, runner.request.AllowedScopes)
	runner.mu.Unlock()

	other := callRPC(t, server, "mallory@example.com", "tasks/get", taskID, map[string]any{"taskId": taskID}, true)
	require.NotNil(t, other.Error)
	require.Equal(t, -32602, other.Error.Code)
	require.Equal(t, "unknown task", other.Error.Message)

	close(runner.release)
	var completed map[string]any
	require.Eventually(t, func() bool {
		completed = resultObject(t, callRPC(t, server, "alex@example.com", "tasks/get", taskID, map[string]any{"taskId": taskID}, true))
		return completed["status"] == "completed"
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, "complete", completed["resultType"])
	toolResult := completed["result"].(map[string]any)
	require.Contains(t, toolResult["content"].([]any)[0].(map[string]any)["text"], "the answer")
}

func TestCorpusMCPAskNotifiesAcceptedInvocationExactlyOnce(t *testing.T) {
	runner := newFakeCorpusAskRunner()
	notifier := newFakeCorpusAskNotifier()
	server := newCorpusMCPHTTPServerWithProvider(t, &fakeCorpusRetriever{}, func() CorpusAskLease {
		return CorpusAskLease{Runner: runner, Context: t.Context(), Notifier: notifier}
	}, newMemoryCorpusTaskBackend())

	create := resultObject(t, callRPC(t, server, "dana@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "what changed?"},
	}, true))
	taskID := create["taskId"].(string)

	select {
	case <-notifier.done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for MCP Ask notification")
	}
	notifier.mu.Lock()
	require.Equal(t, 1, notifier.calls)
	require.Equal(t, "dana@example.com", notifier.owner)
	require.Equal(t, "ask", notifier.toolName)
	require.Equal(t, "11111111-1111-1111-1111-111111111111", notifier.session)
	require.Equal(t, "what changed?", notifier.question)
	notifier.mu.Unlock()

	close(runner.release)
	require.Eventually(t, func() bool {
		result := resultObject(t, callRPC(t, server, "dana@example.com", "tasks/get", taskID, map[string]any{"taskId": taskID}, true))
		return result["status"] == "completed"
	}, time.Second, 10*time.Millisecond)
	notifier.mu.Lock()
	require.Equal(t, 1, notifier.calls)
	notifier.mu.Unlock()
}

// The caller's scopes gate expand before any content fetch: an event in a
// channel outside them reads exactly like a missing one, so expand can't be
// used to read private content or probe which event ids exist.
func TestCorpusMCPExpandIsScopedToCaller(t *testing.T) {
	fetches := 0
	expander := &fakeExpander{
		fetchCount: &fetches,
		results: map[int64]ask_fakeResult{
			7: {res: ExpandResult{Source: "slack", Content: "the whole thread"}},
			9: {res: ExpandResult{Source: "slack", Content: "private channel"}},
		},
	}
	scopes := &fakeScopeLookup{scopes: map[int64]string{7: "slack-channel:C1", 9: "slack-channel:C2"}}
	server := newCorpusMCPHTTPServerWithExpander(t, &fakeCorpusRetriever{}, expander, scopes,
		func() CorpusAskLease { return CorpusAskLease{} }, newMemoryCorpusTaskBackend())

	expand := func(eventID int) map[string]any {
		result := resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "expand", map[string]any{
			"name": "expand", "arguments": map[string]any{"event_id": eventID},
		}, true))
		require.NotEqual(t, true, result["isError"])
		return result["structuredContent"].(map[string]any)
	}

	visible := expand(7)
	require.Equal(t, true, visible["supported"])
	require.Equal(t, "slack", visible["source"])
	require.Equal(t, "the whole thread", visible["content"])

	hidden := expand(9)
	require.Equal(t, false, hidden["supported"])
	require.Equal(t, "no event with event_id=9 accessible from this context", hidden["note"])
	require.Empty(t, hidden["content"])

	missing := expand(11)
	require.Equal(t, false, missing["supported"])
	require.Equal(t, "no event with event_id=11 accessible from this context", missing["note"])

	require.Equal(t, 1, fetches, "only the in-scope event should reach the expander")
}

func TestCorpusMCPRetrieveAndListNotifyMeaningfulInvocations(t *testing.T) {
	retriever := &fakeCorpusRetriever{result: &retrieve.Result{}}
	notifier := newFakeCorpusAskNotifier()
	server := newCorpusMCPHTTPServerWithProvider(t, retriever, func() CorpusAskLease {
		return CorpusAskLease{Notifier: notifier}
	}, newMemoryCorpusTaskBackend())

	resultObject(t, callRPC(t, server, "dana@example.com", "tools/call", "retrieve", map[string]any{
		"name": "retrieve", "arguments": map[string]any{"question": "what changed?"},
	}, true))
	select {
	case <-notifier.done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for MCP retrieve notification")
	}
	notifier.mu.Lock()
	require.Equal(t, "dana@example.com", notifier.owner)
	require.Equal(t, "retrieve", notifier.toolName)
	require.Empty(t, notifier.session)
	require.Equal(t, "what changed?", notifier.question)
	notifier.mu.Unlock()

	resultObject(t, callRPC(t, server, "dana@example.com", "tools/call", "list", map[string]any{
		"name": "list", "arguments": map[string]any{"sources": []string{"slack"}, "actors": []string{"Dana"}},
	}, true))
	select {
	case <-notifier.done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for MCP list notification")
	}
	notifier.mu.Lock()
	require.Equal(t, 2, notifier.calls)
	require.Equal(t, "list", notifier.toolName)
	require.JSONEq(t, `{"sources":["slack"],"actors":["Dana"]}`, notifier.question)
	notifier.mu.Unlock()
}

func TestCorpusMCPRetrieveAndListNotifyAcceptedInvocationWhenBackendFails(t *testing.T) {
	retriever := &fakeCorpusRetriever{
		retrieveErr: errors.New("embedding unavailable"),
		listErr:     errors.New("database unavailable"),
	}
	notifier := newFakeCorpusAskNotifier()
	server := newCorpusMCPHTTPServerWithProvider(t, retriever, func() CorpusAskLease {
		return CorpusAskLease{Notifier: notifier}
	}, newMemoryCorpusTaskBackend())

	for _, tc := range []struct {
		tool      string
		arguments map[string]any
	}{
		{tool: "retrieve", arguments: map[string]any{"question": "what changed?"}},
		{tool: "list", arguments: map[string]any{"sources": []string{"slack"}}},
	} {
		result := resultObject(t, callRPC(t, server, "dana@example.com", "tools/call", tc.tool, map[string]any{
			"name": tc.tool, "arguments": tc.arguments,
		}, true))
		require.Equal(t, true, result["isError"])
		select {
		case <-notifier.done:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for accepted MCP %s notification", tc.tool)
		}
	}

	notifier.mu.Lock()
	require.Equal(t, 2, notifier.calls)
	notifier.mu.Unlock()
}

func TestCorpusMCPTaskCancellationAndTaskIdentityRequirement(t *testing.T) {
	runner := newFakeCorpusAskRunner()
	server := newCorpusMCPHTTPServer(t, &fakeCorpusRetriever{}, runner)
	create := resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "slow question"},
	}, true))
	taskID := create["taskId"].(string)
	<-runner.started

	for _, method := range []string{"tasks/get", "tasks/update", "tasks/cancel"} {
		params := map[string]any{"taskId": taskID}
		if method == "tasks/update" {
			params["inputResponses"] = map[string]any{}
		}
		response := callRPC(t, server, "", method, taskID, params, true)
		require.NotNil(t, response.Error)
		require.Contains(t, response.Error.Message, "verified user identity is required")
	}

	cancel := resultObject(t, callRPC(t, server, "alex@example.com", "tasks/cancel", taskID, map[string]any{"taskId": taskID}, true))
	require.Equal(t, "complete", cancel["resultType"])
	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("Ask context was not cancelled")
	}
	require.True(t, runner.byCaller, "the turn ends as cancelled by its caller")
	got := resultObject(t, callRPC(t, server, "alex@example.com", "tasks/get", taskID, map[string]any{"taskId": taskID}, true))
	require.Equal(t, "cancelled", got["status"])
}

func TestCorpusMCPAskBackendFailureProducesFailedTask(t *testing.T) {
	runner := newFakeCorpusAskRunner()
	runner.err = errors.New("model unavailable")
	server := newCorpusMCPHTTPServer(t, &fakeCorpusRetriever{}, runner)
	create := resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "will this fail?"},
	}, true))
	taskID := create["taskId"].(string)
	<-runner.started
	close(runner.release)

	var failed map[string]any
	require.Eventually(t, func() bool {
		failed = resultObject(t, callRPC(t, server, "alex@example.com", "tasks/get", taskID, map[string]any{"taskId": taskID}, true))
		return failed["status"] == "failed"
	}, time.Second, 10*time.Millisecond)
	require.Nil(t, failed["result"])
	require.Contains(t, failed["error"].(map[string]any)["message"], "model unavailable")
}

func TestCorpusMCPTaskPollAndCancellationWorkAcrossServers(t *testing.T) {
	backend := newMemoryCorpusTaskBackend()
	runner := newFakeCorpusAskRunner()
	first := newCorpusMCPHTTPServerWithBackend(t, &fakeCorpusRetriever{}, runner, backend)
	second := newCorpusMCPHTTPServerWithBackend(t, &fakeCorpusRetriever{}, runner, backend)
	create := resultObject(t, callRPC(t, first, "alex@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "cross-replica cancellation"},
	}, true))
	taskID := create["taskId"].(string)
	<-runner.started

	working := resultObject(t, callRPC(t, second, "alex@example.com", "tasks/get", taskID, map[string]any{"taskId": taskID}, true))
	require.Equal(t, "working", working["status"])
	resultObject(t, callRPC(t, second, "alex@example.com", "tasks/cancel", taskID, map[string]any{"taskId": taskID}, true))
	select {
	case <-runner.canceled:
	case <-time.After(2 * taskCancellationPollPeriod):
		t.Fatal("remote cancellation did not reach the executing server")
	}
	require.True(t, runner.byCaller, "a cancellation through another server ends the turn as cancelled by its caller")
}

func TestCorpusMCPLeaseEndFailsRunningTask(t *testing.T) {
	backend := newMemoryCorpusTaskBackend()
	runner := newFakeCorpusAskRunner()
	replicaCtx, stopReplica := context.WithCancel(t.Context())
	server := newCorpusMCPHTTPServerWithProvider(t, &fakeCorpusRetriever{}, func() CorpusAskLease {
		return CorpusAskLease{Runner: runner, Context: replicaCtx}
	}, backend)
	poller := newCorpusMCPHTTPServerWithBackend(t, &fakeCorpusRetriever{}, runner, backend)
	create := resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "shutdown-sensitive question"},
	}, true))
	taskID := create["taskId"].(string)
	<-runner.started
	stopReplica()

	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("the lease's end did not cancel Ask")
	}
	require.False(t, runner.byCaller, "a replica stopping is not the caller's cancellation")
	var failed map[string]any
	require.Eventually(t, func() bool {
		failed = resultObject(t, callRPC(t, poller, "alex@example.com", "tasks/get", taskID, map[string]any{"taskId": taskID}, true))
		return failed["status"] == "failed"
	}, time.Second, 10*time.Millisecond)
	require.Contains(t, failed["error"].(map[string]any)["message"], "interrupted")
}

// A runner shutting down holds no more tasks: ask is refused at once, telling
// the client to retry, and no task is created or turn started.
func TestCorpusMCPAskRefusedWhileRunnerHoldsNoTasks(t *testing.T) {
	backend := newMemoryCorpusTaskBackend()
	runner := newFakeCorpusAskRunner()
	runner.closed = true
	server := newCorpusMCPHTTPServerWithBackend(t, &fakeCorpusRetriever{}, runner, backend)
	result := resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "asked of a replica shutting down"},
	}, true))
	require.Equal(t, true, result["isError"])
	require.Equal(t, ErrUnavailable.Error(), result["content"].([]any)[0].(map[string]any)["text"])
	backend.mu.Lock()
	created := len(backend.tasks)
	backend.mu.Unlock()
	require.Zero(t, created, "no task is created")
	select {
	case <-runner.started:
		t.Fatal("a turn started on a runner holding no tasks")
	default:
	}
}

// A task holds its runner until its result is recorded, so a replica
// waiting for its tasks before it stops never loses a finished answer.
func TestCorpusMCPTaskHoldsRunnerUntilResultRecorded(t *testing.T) {
	backend := newMemoryCorpusTaskBackend()
	runner := newFakeCorpusAskRunner()
	server := newCorpusMCPHTTPServerWithBackend(t, &fakeCorpusRetriever{}, runner, backend)
	create := resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "held until recorded"},
	}, true))
	taskID := create["taskId"].(string)
	<-runner.started
	require.Equal(t, 1, runner.holding())

	close(runner.release)
	require.Eventually(t, func() bool { return runner.holding() == 0 }, time.Second, 10*time.Millisecond)
	task, err := backend.Get(t.Context(), "alex@example.com", taskID)
	require.NoError(t, err)
	require.Equal(t, "completed", task.descriptor.Status, "the result is recorded before the hold is released")
}

func TestCorpusMCPAskRequiresTaskCapability(t *testing.T) {
	server := newCorpusMCPHTTPServer(t, &fakeCorpusRetriever{}, newFakeCorpusAskRunner())
	response := callRPC(t, server, "alex@example.com", "tools/call", "ask", map[string]any{
		"name": "ask", "arguments": map[string]any{"question": "hello"},
	}, false)
	require.NotNil(t, response.Error)
	require.Equal(t, -32602, response.Error.Code)
	require.Contains(t, response.Error.Message, tasksExtensionID)
}

const mcpTestSlackPermalink = "https://example.slack.com/archives/C1/p1700000000123456"

func mcpReferenceFixture() *fakeExactReferenceResolver {
	return &fakeExactReferenceResolver{items: map[string]fakeExactReferenceItem{
		"ENG-770": {scope: "internal", resolved: ResolvedReference{
			EventID: 42, Source: "linear", Content: "ENG-770: rotate the gateway key",
		}},
		"C1\x001700000000.123456": {scope: "slack-channel:C1", resolved: ResolvedReference{
			EventID: 43, Source: "slack", URL: mcpTestSlackPermalink, Content: "Exact linked Slack message:\nship it",
		}},
		"ENG-9": {scope: "slack-channel:C2", resolved: ResolvedReference{
			EventID: 44, Source: "linear", Content: "ENG-9: private",
		}},
	}}
}

func callMCPRetrieve(t *testing.T, server *httptest.Server, question string) map[string]any {
	t.Helper()
	return resultObject(t, callRPC(t, server, "alex@example.com", "tools/call", "retrieve", map[string]any{
		"name": "retrieve", "arguments": map[string]any{"question": question},
	}, true))
}

func newMCPReferenceServer(t *testing.T, retriever CorpusRetriever, resolver ReferencePrerequisiteResolver) *httptest.Server {
	t.Helper()
	return newCorpusMCPHTTPServerWithResolver(t, retriever, &fakeExpander{}, &fakeScopeLookup{}, resolver,
		func() CorpusAskLease { return CorpusAskLease{} }, newMemoryCorpusTaskBackend())
}

func TestCorpusMCPRetrieveReturnsExactReferencesBeforeHits(t *testing.T) {
	retriever := &fakeCorpusRetriever{result: &retrieve.Result{Hits: []retrieve.Hit{{EventID: 7, Source: "slack", Content: "nearby"}}}}
	resolver := mcpReferenceFixture()
	server := newMCPReferenceServer(t, retriever, resolver)

	question := "what did we decide in ENG-770? see " + mcpTestSlackPermalink + " and ENG-770 again"
	result := callMCPRetrieve(t, server, question)
	require.NotEqual(t, true, result["isError"])

	require.Equal(t, 1, resolver.calls)
	require.Equal(t, []string{"public", "internal", "slack-channel:C1"}, resolver.allowed)
	require.Equal(t, 1, retriever.retrieveCalls, "semantic retrieval still runs alongside exact references")
	require.Equal(t, question, retriever.retrieveReq.Question)

	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	require.True(t, strings.HasPrefix(text, `{"references":[`), "references must precede hits: %s", text)
	var decoded mcpRetrieveOutput
	require.NoError(t, json.Unmarshal([]byte(text), &decoded))
	require.Equal(t, []mcpResolvedReference{
		{Handle: "ENG-770", Kind: ReferenceLinearIssue, EventID: 42, Source: "linear",
			Content: "ENG-770: rotate the gateway key"},
		{Handle: mcpTestSlackPermalink, Kind: ReferenceSlackPermalink, EventID: 43, Source: "slack",
			URL: mcpTestSlackPermalink, Content: "Exact linked Slack message:\nship it"},
	}, decoded.References)
	require.Len(t, decoded.Hits, 1)
	require.Equal(t, int64(7), decoded.Hits[0].EventID)

	structured := result["structuredContent"].(map[string]any)
	require.Len(t, structured["references"], 2)
	require.Len(t, structured["hits"], 1)
	linear := structured["references"].([]any)[0].(map[string]any)
	require.NotContains(t, linear, "url", "url is omitted when the source has none")
}

func TestCorpusMCPRetrieveMarksTruncatedReferences(t *testing.T) {
	retriever := &fakeCorpusRetriever{result: &retrieve.Result{}}
	resolver := mcpReferenceFixture()
	linear := resolver.items["ENG-770"]
	linear.resolved.Truncated = true
	resolver.items["ENG-770"] = linear
	server := newMCPReferenceServer(t, retriever, resolver)

	result := callMCPRetrieve(t, server, "compare ENG-770 with "+mcpTestSlackPermalink)
	require.NotEqual(t, true, result["isError"])
	references := result["structuredContent"].(map[string]any)["references"].([]any)
	require.Len(t, references, 2)
	require.Equal(t, true, references[0].(map[string]any)["truncated"])
	require.NotContains(t, references[1].(map[string]any), "truncated", "an uncut reference omits the flag")
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	require.Contains(t, text, `"content":"ENG-770: rotate the gateway key","truncated":true}`)
}

// Every way an explicit handle can fail produces one indistinguishable result
// with no identifier or scope detail, and never falls back to semantic search.
func TestCorpusMCPRetrieveFailsClosedOnUnavailableReference(t *testing.T) {
	for _, tc := range []struct {
		name     string
		question string
		err      error
	}{
		{name: "missing", question: "what happened in ENG-1?"},
		{name: "out of scope", question: "what happened in ENG-9?"},
		{name: "one of several unavailable", question: "compare ENG-770 with event_id: 12"},
		{name: "resolver error", question: "what happened in ENG-770?", err: errors.New("database unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retriever := &fakeCorpusRetriever{result: &retrieve.Result{Hits: []retrieve.Hit{{EventID: 7, Content: "nearby"}}}}
			resolver := mcpReferenceFixture()
			resolver.err = tc.err
			server := newMCPReferenceServer(t, retriever, resolver)

			result := callMCPRetrieve(t, server, tc.question)
			require.Equal(t, 1, resolver.calls)
			require.Equal(t, 0, retriever.retrieveCalls, "an unavailable handle must not become a semantic question")
			require.Equal(t, true, result["isError"])
			require.Equal(t, []any{map[string]any{"type": "text", "text": ReferenceUnavailableText}}, result["content"])
			require.NotContains(t, result, "structuredContent")
		})
	}
}

func TestCorpusMCPRetrieveWithoutHandlesIsUnchanged(t *testing.T) {
	hits := &retrieve.Result{Hits: []retrieve.Hit{{EventID: 7, Source: "slack", Content: "answer"}}}
	retriever := &fakeCorpusRetriever{result: hits}
	resolver := mcpReferenceFixture()
	server := newMCPReferenceServer(t, retriever, resolver)

	result := callMCPRetrieve(t, server, "why did the gateway decision change")
	require.NotEqual(t, true, result["isError"])
	require.Equal(t, 0, resolver.calls, "a question without handles never reaches the resolver")

	want, err := json.Marshal(retrieveOutputFromResult(hits, testSources))
	require.NoError(t, err)
	require.Equal(t, string(want), result["content"].([]any)[0].(map[string]any)["text"])
	require.NotContains(t, result["structuredContent"].(map[string]any), "references")
}

func TestCorpusMCPRequiresReferenceResolverAndLinearTeams(t *testing.T) {
	provider := func() CorpusAskLease { return CorpusAskLease{} }
	_, err := newCorpusMCPServer(&fakeCorpusRetriever{}, testSources, &fakeExpander{}, &fakeScopeLookup{}, &fakeAttachmentReader{}, nil, testLinearTeams,
		provider, newMemoryCorpusTaskBackend(), t.Context(), "test")
	require.ErrorContains(t, err, "exact reference resolver is required")
	_, err = newCorpusMCPServer(&fakeCorpusRetriever{}, testSources, &fakeExpander{}, &fakeScopeLookup{}, &fakeAttachmentReader{}, mcpReferenceFixture(), nil,
		provider, newMemoryCorpusTaskBackend(), t.Context(), "test")
	require.ErrorContains(t, err, "linear team provider is required")
	_, err = newCorpusMCPServer(&fakeCorpusRetriever{}, testSources, &fakeExpander{}, &fakeScopeLookup{}, nil, mcpReferenceFixture(), testLinearTeams,
		provider, newMemoryCorpusTaskBackend(), t.Context(), "test")
	require.ErrorContains(t, err, "attachment reader is required")
}

// The live repro, over MCP: a key-shaped word from an unknown team is ordinary
// search text, while an unresolvable key on a known team still fails closed.
func TestCorpusMCPRetrieveGatesBareLinearKeysOnKnownTeams(t *testing.T) {
	retriever := &fakeCorpusRetriever{result: &retrieve.Result{Hits: []retrieve.Hit{{EventID: 7, Content: "nearby"}}}}
	resolver := mcpReferenceFixture()
	server := newMCPReferenceServer(t, retriever, resolver)

	result := callMCPRetrieve(t, server, "SHA-256 checksum mismatch during deploys")
	require.NotEqual(t, true, result["isError"])
	require.Equal(t, 0, resolver.calls)
	require.Equal(t, 1, retriever.retrieveCalls)
	require.NotContains(t, result["structuredContent"].(map[string]any), "references")

	result = callMCPRetrieve(t, server, "status of ENG-999999")
	require.Equal(t, true, result["isError"])
	require.Equal(t, []any{map[string]any{"type": "text", "text": ReferenceUnavailableText}}, result["content"])
	require.Equal(t, 1, resolver.calls)
	require.Equal(t, "ENG-999999", resolver.refs[0].Value)
	require.Equal(t, 1, retriever.retrieveCalls, "an unresolvable known-team key never reaches semantic search")
}

// Tool descriptions are part of Ask's prompt. The MCP-only reference contract
// must not leak into Ask, and Ask's handle sentence is false over MCP.
func TestCorpusMCPReferenceDescriptionsStayPerSurface(t *testing.T) {
	const askHandleSentence = "have already been fetched and are in the user's message"
	askExpand, err := NewEinoExpandTool(&fakeExpander{}, &fakeScopeLookup{}, testSources).Tool.Info(t.Context())
	require.NoError(t, err)
	require.Equal(t, expandToolDescriptionHead+" Items the question named exactly (a Slack link, an issue key, a session ID) "+askHandleSentence+", so do not expand to find those. "+expandToolDescriptionTail, askExpand.Desc)
	// Ask has no attachment tool, so only MCP's expand points at one.
	require.NotContains(t, askExpand.Desc, "`attachment`")
	askRetrieve, err := NewEinoRetrieveTool(&fakeCorpusRetriever{}, testSources).Tool.Info(t.Context())
	require.NoError(t, err)
	require.Equal(t, retrieveToolDescription+askRetrieveCoverageNote, askRetrieve.Desc)
	// Only an Ask turn keeps a record of the sources it has searched.
	require.Contains(t, askRetrieve.Desc, "`not_searched`")
	require.NotContains(t, mcpRetrieveToolDescription, "`not_searched`")
	require.NotContains(t, askRetrieve.Desc, "'references'")

	server := newCorpusMCPHTTPServer(t, &fakeCorpusRetriever{}, newFakeCorpusAskRunner())
	listed := resultObject(t, callRPC(t, server, "", "tools/list", "", map[string]any{}, true))
	descriptions := map[string]string{}
	for _, raw := range listed["tools"].([]any) {
		tool := raw.(map[string]any)
		descriptions[tool["name"].(string)] = tool["description"].(string)
	}
	require.NotContains(t, descriptions["expand"], askHandleSentence)
	require.Contains(t, descriptions["expand"], "`references`")
	require.Contains(t, descriptions["expand"], "`attachment`")
	require.True(t, strings.HasPrefix(descriptions["retrieve"], retrieveToolDescription))
	require.Contains(t, descriptions["retrieve"], "'references'")
	require.Contains(t, descriptions["retrieve"], `"`+ReferenceUnavailableText+`"`)
}
