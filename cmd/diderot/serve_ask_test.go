package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
)

// fakeAskModel stands in for the provider of Ask's model: it answers the
// Responses API with the final answer Ask asks for, numbering each answer,
// and records every request. A question carrying a hold's marker waits for
// the hold to be released before its first model call is answered, and one
// containing "LIST" first gets a call to the list tool.
type fakeAskModel struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	answered int
	holds    map[string]*askHold
	requests []fakeAskRequest
}

// fakeAskRequest is one model request the fake answered.
type fakeAskRequest struct {
	Model string
	// Question is the last user message: the turn's question in its envelope.
	Question string
	// Raw is the whole request, history included.
	Raw string
	// Answer is the answer the fake gave, empty for a tool call.
	Answer string
}

// askHold holds the first model call of a question carrying marker.
type askHold struct {
	marker              string
	arrived, release    chan struct{}
	arrivedOnce, letOne sync.Once
}

func newFakeAskModel(t *testing.T) *fakeAskModel {
	t.Helper()
	fake := &fakeAskModel{t: t, holds: map[string]*askHold{}}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(func() {
		fake.releaseAll()
		fake.server.Close()
	})
	return fake
}

// hold makes the first model call of a question containing marker wait until
// the returned hold is released.
func (f *fakeAskModel) hold(marker string) *askHold {
	h := &askHold{marker: marker, arrived: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	f.holds[marker] = h
	f.mu.Unlock()
	return h
}

func (h *askHold) let() { h.letOne.Do(func() { close(h.release) }) }

// waitArrived waits for the held call to reach the fake.
func (h *askHold) waitArrived(t *testing.T) {
	t.Helper()
	select {
	case <-h.arrived:
	case <-time.After(time.Minute):
		t.Fatalf("no model call for %s arrived", h.marker)
	}
}

func (f *fakeAskModel) releaseAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.holds {
		h.let()
	}
}

// containing returns the requests whose question contains text.
func (f *fakeAskModel) containing(text string) []fakeAskRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeAskRequest
	for _, request := range f.requests {
		if strings.Contains(request.Question, text) {
			out = append(out, request)
		}
	}
	return out
}

type fakeResponsesRequest struct {
	Model string `json:"model"`
	// Input is a string or a list of items.
	Input json.RawMessage `json:"input"`
	Tools []struct {
		Name       string         `json:"name"`
		Parameters map[string]any `json:"parameters"`
	} `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Text       *struct {
		Format struct {
			Type string `json:"type"`
		} `json:"format"`
	} `json:"text"`
}

func (f *fakeAskModel) serve(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err == nil && strings.HasSuffix(r.URL.Path, "/chat/completions") {
		// A chat job's test.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
		return
	}
	if err != nil || !strings.HasSuffix(r.URL.Path, "/responses") {
		http.Error(w, "unexpected request", http.StatusNotFound)
		return
	}
	var req fakeResponsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	question, toolOutputs := "", 0
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(req.Input, &question) != nil {
		_ = json.Unmarshal(req.Input, &items)
	}
	for _, item := range items {
		switch {
		case item.Type == "function_call_output":
			toolOutputs++
		case item.Role == "user":
			question, toolOutputs = fakeContentText(item.Content), 0
		}
	}

	var forced struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	_ = json.Unmarshal(req.ToolChoice, &forced)
	switch {
	case forced.Type == "function":
		// Session titles force their recording function.
		f.respond(w, req.Model, map[string]any{
			"type": "function_call", "id": "fc_title", "call_id": "call_title", "name": forced.Name,
			"arguments": `{"title":"A title"}`, "status": "completed",
		})
		return
	case toolOutputs == 0:
		f.waitForHold(r.Context(), question)
		if strings.Contains(question, "LIST") {
			f.record(fakeAskRequest{Model: req.Model, Question: question, Raw: string(raw)})
			f.respond(w, req.Model, map[string]any{
				"type": "function_call", "id": "fc_list", "call_id": "call_list", "name": "list",
				"arguments": `{"top_k":1}`, "status": "completed",
			})
			return
		}
	}
	f.mu.Lock()
	f.answered++
	answer := fmt.Sprintf("answer %d", f.answered)
	f.requests = append(f.requests, fakeAskRequest{Model: req.Model, Question: question, Raw: string(raw), Answer: answer})
	f.mu.Unlock()
	text := answer
	if req.Text != nil && req.Text.Format.Type == "json_schema" {
		encoded, _ := json.Marshal(map[string]any{"answer": answer, "artifacts": []any{}})
		text = string(encoded)
	}
	f.respond(w, req.Model, map[string]any{
		"type": "message", "id": "msg_1", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	})
}

// waitForHold waits while a hold whose marker question carries is held.
func (f *fakeAskModel) waitForHold(ctx context.Context, question string) {
	f.mu.Lock()
	var held *askHold
	for marker, h := range f.holds {
		if strings.Contains(question, marker) {
			held = h
		}
	}
	f.mu.Unlock()
	if held == nil {
		return
	}
	held.arrivedOnce.Do(func() { close(held.arrived) })
	select {
	case <-held.release:
	case <-ctx.Done():
	}
}

func (f *fakeAskModel) record(request fakeAskRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
}

func (f *fakeAskModel) respond(w http.ResponseWriter, model string, output map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "resp_1", "object": "response", "created_at": time.Now().Unix(), "status": "completed",
		"model": model, "parallel_tool_calls": false, "output": []any{output},
		"usage": map[string]any{
			"input_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0},
			"output_tokens": 1, "output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens": 2,
		},
	})
}

// fakeContentText flattens a Responses content value: a string, or parts
// that carry text.
func fakeContentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part.Text)
	}
	return b.String()
}

// askServeEnv is serveTestEnv with Ask on, answered by model, and every
// listener a free loopback port chosen as each replica starts.
func askServeEnv(t *testing.T, db string, model *fakeAskModel) {
	t.Helper()
	serveTestEnv(t, db)
	t.Setenv("DIDEROT_ASK_ENABLED", "true")
	saved := gatewayMigration(map[string]string{"ask": "gpt-6.1-sol:main"})
	saved.AskReasoningEffort, saved.AskMaxToolTurns = "", 0
	saved.Embedding = nil // serveTestEnv's
	seedModelSettings(t, os.Getenv("DIDEROT_DB_URL"), model.server.URL+"/v1", saved)
}

// askReplica is one serve under test.
type askReplica struct {
	name   string
	base   string
	logs   *capturedLog
	stop   context.CancelFunc
	served chan error
}

// startAskReplica starts serve with the environment as it is, on listeners
// of its own, and waits until it is ready and its Ask is attached. Replicas
// start one at a time: serve reads the environment only before it logs that
// it is starting.
func startAskReplica(t *testing.T, name string) *askReplica {
	t.Helper()
	public := freeLoopbackAddr(t)
	for key, value := range map[string]string{
		"DIDEROT_LISTEN_ADDR":           public,
		"DIDEROT_INTERNAL_LISTEN_ADDR":  freeLoopbackAddr(t),
		"DIDEROT_METRICS_LISTEN_ADDR":   freeLoopbackAddr(t),
		"DIDEROT_PAGERDUTY_LISTEN_ADDR": freeLoopbackAddr(t),
		"DIDEROT_PUBLIC_URL":            "http://" + public,
	} {
		t.Setenv(key, value)
	}
	replica := &askReplica{name: name, base: "http://" + public, logs: newCapturedLog(t, name), served: make(chan error, 1)}
	ctx, stop := context.WithCancel(t.Context())
	replica.stop = stop
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		replica.served <- runServe(ctx, replica.logs)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-returned:
		case <-time.After(time.Minute):
			t.Errorf("%s did not return after its stop", name)
		}
	})
	replica.waitLog(t, "diderot starting")
	waitFor(t, replica.served, 2*time.Minute, name+"'s /readyz to answer 200", func() bool {
		return httpStatus(replica.base+"/readyz") == http.StatusOK
	})
	replica.waitLog(t, "ask service attached")
	return replica
}

// waitLog waits until the replica has logged msg.
func (r *askReplica) waitLog(t *testing.T, msg string) {
	t.Helper()
	waitFor(t, r.served, 2*time.Minute, r.name+" to log "+msg, func() bool { return r.logged(msg) > 0 })
}

// logged is how many times the replica has logged msg.
func (r *askReplica) logged(msg string) int {
	n := 0
	for _, line := range r.logs.lines() {
		if line.Msg == msg {
			n++
		}
	}
	return n
}

// shutDown stops the replica and waits for serve to return.
func (r *askReplica) shutDown(t *testing.T) {
	t.Helper()
	r.stop()
	r.waitReturned(t)
}

// waitReturned waits for serve, already stopped, to return without error.
func (r *askReplica) waitReturned(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.served:
		require.NoError(t, err)
	case <-time.After(2 * time.Minute):
		t.Fatalf("%s did not return after the shutdown signal", r.name)
	}
}

// errorsLogged are the ERROR lines the replica logged, but for a worker cut
// off mid-call by its end logging the cancellation.
func (r *askReplica) errorsLogged() []string {
	var out []string
	for _, line := range r.logs.lines() {
		if line.Level == "ERROR" && line.Err != context.Canceled.Error() {
			out = append(out, line.raw)
		}
	}
	return out
}

type askReply struct {
	Status    int
	Body      string
	SessionID string `json:"session_id"`
	Reply     string `json:"reply"`
}

// ask posts question to the replica's /v1/ask, continuing session unless it
// is empty.
func (r *askReplica) ask(t *testing.T, session, question string) askReply {
	t.Helper()
	body := map[string]any{"question": question}
	if session != "" {
		body["session_id"] = session
	}
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.base+"/v1/ask", bytes.NewReader(encoded))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return askReply{Body: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	reply := askReply{Status: resp.StatusCode, Body: string(raw)}
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.Unmarshal(raw, &reply), string(raw))
	}
	return reply
}

// askAsync is ask in the background.
func (r *askReplica) askAsync(t *testing.T, session, question string) <-chan askReply {
	done := make(chan askReply, 1)
	go func() { done <- r.ask(t, session, question) }()
	return done
}

// mcpRequest is the headers and body of one JSON-RPC request to /mcp as the
// gateway sends it, with the tasks extension.
func mcpRequest(t *testing.T, method, name string, params map[string]any) (http.Header, []byte) {
	t.Helper()
	params["_meta"] = map[string]any{
		mcp.MetaKeyProtocolVersion:    "2026-07-28",
		mcp.MetaKeyClientInfo:         map[string]any{"name": "test-client", "version": "1"},
		mcp.MetaKeyClientCapabilities: map[string]any{"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}}},
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	header := http.Header{}
	header.Set("Authorization", "Bearer test-key")
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json, text/event-stream")
	header.Set("MCP-Protocol-Version", "2026-07-28")
	header.Set("MCP-Method", method)
	if name != "" {
		header.Set("MCP-Name", name)
	}
	return header, body
}

// mcpResult is the result object of a JSON-RPC reply from /mcp.
func mcpResult(t *testing.T, status int, raw string) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, status, raw)
	var decoded struct {
		Result map[string]any `json:"result"`
		Error  any            `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded), raw)
	require.Nil(t, decoded.Error, raw)
	return decoded.Result
}

// mcpCall sends one JSON-RPC request to the replica's /mcp and returns the
// result object.
func (r *askReplica) mcpCall(t *testing.T, method, name string, params map[string]any) map[string]any {
	t.Helper()
	header, body := mcpRequest(t, method, name, params)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.base+"/mcp", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return mcpResult(t, resp.StatusCode, string(raw))
}

// stallRequest posts to the replica's path a request whose body never
// comes, as a client that stalls mid-request does. It returns once the
// replica's handler waits for the body, which it asks for with a 100
// Continue. ended then receives what the connection reads next: an error
// once the replica drops the request, nil if it answers.
func (r *askReplica) stallRequest(t *testing.T, path string, header http.Header) (ended <-chan error) {
	t.Helper()
	host := strings.TrimPrefix(r.base, "http://")
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	var head bytes.Buffer
	fmt.Fprintf(&head, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: 1024\r\nExpect: 100-continue\r\n", path, host)
	require.NoError(t, header.Write(&head))
	head.WriteString("\r\n")
	_, err = conn.Write(head.Bytes())
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Minute)))
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "HTTP/1.1 100 Continue\r\n", status, "%s %s waits for its body", r.name, path)
	blank, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "\r\n", blank)
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	result := make(chan error, 1)
	go func() {
		_, err := reader.ReadByte()
		result <- err
	}()
	return result
}

// listening reports whether the replica's public listener takes new
// connections.
func (r *askReplica) listening() bool {
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(r.base, "http://"), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// shorten sets one of serve's shutdown timings to d until the test ends.
func shorten(t *testing.T, timing *time.Duration, d time.Duration) {
	t.Helper()
	previous := *timing
	*timing = d
	t.Cleanup(func() { *timing = previous })
}

// startMCPAsk starts an MCP ask task on the replica and returns its id.
func (r *askReplica) startMCPAsk(t *testing.T, question string) string {
	t.Helper()
	created := r.mcpCall(t, "tools/call", "ask", map[string]any{"name": "ask", "arguments": map[string]any{"question": question}})
	require.Equal(t, "working", created["status"], "%v", created)
	taskID, _ := created["taskId"].(string)
	require.NotEmpty(t, taskID)
	return taskID
}

// pollMCPTask polls the task on the replica until it leaves working.
func (r *askReplica) pollMCPTask(t *testing.T, taskID string) map[string]any {
	t.Helper()
	var task map[string]any
	require.Eventually(t, func() bool {
		task = r.mcpCall(t, "tasks/get", taskID, map[string]any{"taskId": taskID})
		return task["status"] != "working"
	}, time.Minute, 100*time.Millisecond)
	return task
}

// mcpAnswer is the answer a completed MCP ask task carries.
func mcpAnswer(t *testing.T, task map[string]any) string {
	t.Helper()
	require.Equal(t, "completed", task["status"], "%v", task)
	result, _ := task["result"].(map[string]any)
	structured, _ := result["structuredContent"].(map[string]any)
	reply, _ := structured["reply"].(string)
	require.NotEmpty(t, reply, "%v", task)
	return reply
}

// TestAskOnOneReplica pins what one replica does with Ask: it answers
// /v1/ask and continues a session with its history; it runs the MCP ask as a
// task its poller collects; and a second turn for a session whose turn is
// running waits for it, then answers with that turn in its history.
func TestAskOnOneReplica(t *testing.T) {
	model := newFakeAskModel(t)
	askServeEnv(t, "cmd_serve_ask_one", model)
	t.Setenv("DIDEROT_LEADER_ELECTION", "false")
	replica := startAskReplica(t, "replica")

	first := replica.ask(t, "", "Where is the first question?")
	require.Equal(t, http.StatusOK, first.Status, first.Body)
	require.NotEmpty(t, first.Reply)
	next := replica.ask(t, first.SessionID, "And the follow-up?")
	require.Equal(t, http.StatusOK, next.Status, next.Body)
	followUp := model.containing("And the follow-up?")
	require.Len(t, followUp, 1)
	assert.Contains(t, followUp[0].Raw, first.Reply, "a continuation replays the session's history")

	task := replica.startMCPAsk(t, "What does MCP ask answer?")
	assert.NotEmpty(t, mcpAnswer(t, replica.pollMCPTask(t, task)))

	held := model.hold("HOLD-ONE")
	running := replica.askAsync(t, first.SessionID, "HOLD-ONE: the running turn")
	held.waitArrived(t)
	waiting := replica.askAsync(t, first.SessionID, "The waiting turn")
	time.Sleep(time.Second)
	assert.Empty(t, model.containing("The waiting turn"), "a second turn of a busy session waits for the first")
	held.let()
	ran := <-running
	require.Equal(t, http.StatusOK, ran.Status, ran.Body)
	waited := <-waiting
	require.Equal(t, http.StatusOK, waited.Status, waited.Body)
	waitedCall := model.containing("The waiting turn")
	require.Len(t, waitedCall, 1)
	assert.Contains(t, waitedCall[0].Raw, ran.Reply, "the waiting turn sees the turn it waited for")

	replica.shutDown(t)
	assert.Empty(t, replica.errorsLogged())
}

// TestAskOnEveryReplica runs two replicas, and later a third, against one
// database under the Postgres leader election, and checks that Ask belongs
// to every replica: the one not leading answers it; an MCP ask task started
// on one is collected on another; two replicas never run turns of one
// session at once; a saved Ask model reaches the replica not leading while
// a turn it is running finishes on the model it started with; a turn
// survives a change of leader; a replica shutting down starts no new turn
// and lets those it started finish while it still serves requests; one
// whose turns outlive its drain deadline cancels them, fails its MCP task,
// frees the session at once, and logs no error; and a request that never
// ends holds a shutdown only for the listeners' short grace.
func TestAskOnEveryReplica(t *testing.T) {
	model := newFakeAskModel(t)
	askServeEnv(t, "cmd_serve_ask_replicas", model)
	t.Setenv("DIDEROT_LEADER_BACKEND", "postgres")
	leader := startAskReplica(t, "leader")
	leader.waitLog(t, "acquired leader lock; waiting out takeover delay")
	other := startAskReplica(t, "other")
	other.waitLog(t, "leader lock held by another contender; waiting")

	t.Run("the replica not leading answers Ask", func(t *testing.T) {
		first := other.ask(t, "", "Asked of the replica that does not lead")
		require.Equal(t, http.StatusOK, first.Status, first.Body)
		next := other.ask(t, first.SessionID, "Continued on it")
		require.Equal(t, http.StatusOK, next.Status, next.Body)
		assert.Contains(t, model.containing("Continued on it")[0].Raw, first.Reply)
	})

	t.Run("an MCP ask started on one replica is collected on another", func(t *testing.T) {
		task := other.startMCPAsk(t, "An MCP question started on the replica not leading")
		assert.NotEmpty(t, mcpAnswer(t, leader.pollMCPTask(t, task)))
		task = leader.startMCPAsk(t, "An MCP question started on the leader")
		assert.NotEmpty(t, mcpAnswer(t, other.pollMCPTask(t, task)))
	})

	t.Run("two replicas run one session's turns one at a time", func(t *testing.T) {
		start := leader.ask(t, "", "A session both replicas continue")
		require.Equal(t, http.StatusOK, start.Status, start.Body)
		held := model.hold("HOLD-SESSION")
		running := leader.askAsync(t, start.SessionID, "HOLD-SESSION: running on the leader")
		held.waitArrived(t)
		waiting := other.askAsync(t, start.SessionID, "Waiting on the other replica")
		time.Sleep(2 * time.Second)
		assert.Empty(t, model.containing("Waiting on the other replica"), "the other replica waits for the running turn")
		held.let()
		ran := <-running
		require.Equal(t, http.StatusOK, ran.Status, ran.Body)
		waited := <-waiting
		require.Equal(t, http.StatusOK, waited.Status, waited.Body)
		calls := model.containing("Waiting on the other replica")
		require.Len(t, calls, 1)
		assert.Contains(t, calls[0].Raw, ran.Reply, "the waiting turn sees the turn it waited for")
	})

	t.Run("a saved Ask model reaches the replica not leading", func(t *testing.T) {
		held := model.hold("HOLD-MODEL")
		inFlight := other.askAsync(t, "", "HOLD-MODEL LIST: started before the change")
		held.waitArrived(t)
		builds := other.logged("ask service attached")
		saveModel(t, leader.base, leader.base, "ask", "gpt-6-sol:main")
		waitFor(t, other.served, time.Minute, "the replica not leading to build Ask with the saved model", func() bool {
			return other.logged("ask service attached") > builds
		})
		held.let()
		reply := <-inFlight
		require.Equal(t, http.StatusOK, reply.Status, reply.Body)
		calls := model.containing("HOLD-MODEL LIST")
		require.Len(t, calls, 2, "a list call, then the answer")
		assert.Equal(t, calls[0].Model, calls[1].Model, "a turn in flight finishes on the model it started with")

		after := other.ask(t, "", "Asked after the change")
		require.Equal(t, http.StatusOK, after.Status, after.Body)
		assert.NotEqual(t, calls[0].Model, model.containing("Asked after the change")[0].Model)
	})

	t.Run("a turn survives a change of leader", func(t *testing.T) {
		held := model.hold("HOLD-LEADER")
		inFlight := other.askAsync(t, "", "HOLD-LEADER: across a change of leader")
		held.waitArrived(t)
		leader.shutDown(t)
		other.waitLog(t, "became leader")
		held.let()
		reply := <-inFlight
		require.Equal(t, http.StatusOK, reply.Status, reply.Body)
		assert.Empty(t, leader.errorsLogged())
	})

	t.Run("a replica shutting down finishes the turns it started and starts no more", func(t *testing.T) {
		draining := startAskReplica(t, "draining")
		draining.waitLog(t, "leader lock held by another contender; waiting")
		heldTurn, heldTask := model.hold("HOLD-DRAIN-TURN"), model.hold("HOLD-DRAIN-TASK")
		inFlight := draining.askAsync(t, "", "HOLD-DRAIN-TURN: an HTTP ask in flight at the shutdown")
		// The second turn starts once the first has reached the model: two
		// runs starting at the same instant on one Eino agent both write the
		// agent's cancel context (nil for both here), which the race
		// detector reports.
		heldTurn.waitArrived(t)
		task := draining.startMCPAsk(t, "HOLD-DRAIN-TASK: an MCP task in flight at the shutdown")
		heldTask.waitArrived(t)

		draining.stop()
		draining.waitLog(t, "shutdown: draining the work in progress")
		// The replica closed the connections it was keeping alive, so the
		// requests below dial it anew. Its listeners serve while its Ask
		// drains, but it starts no new turn.
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		late := draining.ask(t, "", "LATE-HTTP: asked once the replica is shutting down")
		assert.Equal(t, http.StatusServiceUnavailable, late.Status, late.Body)
		assert.Contains(t, late.Body, ask.ErrUnavailable.Error())
		refused := draining.mcpCall(t, "tools/call", "ask",
			map[string]any{"name": "ask", "arguments": map[string]any{"question": "LATE-MCP: asked once the replica is shutting down"}})
		assert.Equal(t, true, refused["isError"], "%v", refused)
		assert.Contains(t, fmt.Sprint(refused["content"]), ask.ErrUnavailable.Error())
		assert.Empty(t, model.containing("LATE-"), "no turn starts once the replica is shutting down")
		working := draining.mcpCall(t, "tasks/get", task, map[string]any{"taskId": task})
		assert.Equal(t, "working", working["status"], "tasks/get answers on the replica while its Ask drains")

		// The turns in flight hold serve open until they finish.
		select {
		case err := <-draining.served:
			t.Fatalf("serve returned with turns in flight: %v", err)
		default:
		}
		heldTurn.let()
		heldTask.let()
		reply := <-inFlight
		require.Equal(t, http.StatusOK, reply.Status, reply.Body)
		assert.NotEmpty(t, reply.Reply)
		draining.waitReturned(t)
		assert.NotEmpty(t, mcpAnswer(t, other.pollMCPTask(t, task)), "the MCP task's answer is recorded")
		assert.Zero(t, draining.logged("ask: shutdown deadline reached; cancelling the turns still running"))
		assert.Zero(t, draining.logged("ask: still in use as the replica stops"))
		assert.Empty(t, draining.errorsLogged())
	})

	t.Run("a replica cancels the turns that outlive its drain deadline", func(t *testing.T) {
		stopping := startAskReplica(t, "stopping")
		stopping.waitLog(t, "leader lock held by another contender; waiting")
		start := stopping.ask(t, "", "A session the stopping replica holds")
		require.Equal(t, http.StatusOK, start.Status, start.Body)
		heldTurn, heldTask := model.hold("HOLD-STOP"), model.hold("HOLD-TASK")
		cutOff := stopping.askAsync(t, start.SessionID, "HOLD-STOP: cut off at the drain deadline")
		// As above, the second turn starts once the first has reached the
		// model.
		heldTurn.waitArrived(t)
		task := stopping.startMCPAsk(t, "HOLD-TASK: an MCP task cut off at the drain deadline")
		heldTask.waitArrived(t)

		shorten(t, &drainTimeout, 2*time.Second)
		stopping.shutDown(t)
		stopped := time.Now()
		next := other.ask(t, start.SessionID, "Continued at once on another replica")
		require.Equal(t, http.StatusOK, next.Status, next.Body)
		assert.Less(t, time.Since(stopped), 10*time.Second, "the stopped replica released the session's lease")
		select {
		case cut := <-cutOff:
			assert.Equal(t, http.StatusInternalServerError, cut.Status, "the cut-off ask still writes its error: %s", cut.Body)
			assert.Contains(t, cut.Body, context.Canceled.Error())
		case <-time.After(5 * time.Second):
			t.Fatal("the cut-off turn's request outlived its replica")
		}
		interrupted := other.pollMCPTask(t, task)
		require.Equal(t, "failed", interrupted["status"], "%v", interrupted)
		assert.Contains(t, fmt.Sprint(interrupted["error"]), "interrupted")
		assert.Equal(t, 1, stopping.logged("ask: shutdown deadline reached; cancelling the turns still running"))
		assert.Zero(t, stopping.logged("ask: still in use as the replica stops"))
		assert.Empty(t, stopping.errorsLogged())
	})

	t.Run("a request that never ends holds shutdown only for the listeners' grace", func(t *testing.T) {
		lingering := startAskReplica(t, "lingering")
		lingering.waitLog(t, "leader lock held by another contender; waiting")
		shorten(t, &listenerGrace, time.Second)
		held := model.hold("HOLD-LINGER")
		inFlight := lingering.askAsync(t, "", "HOLD-LINGER: an HTTP ask in flight at the shutdown")
		held.waitArrived(t)
		header, _ := mcpRequest(t, "tools/call", "retrieve",
			map[string]any{"name": "retrieve", "arguments": map[string]any{"question": "a body that never comes"}})
		stalled := lingering.stallRequest(t, "/mcp", header)

		lingering.stop()
		lingering.waitLog(t, "shutdown: draining the work in progress")
		assert.True(t, lingering.listening(), "the listeners serve while Ask drains")
		held.let()
		reply := <-inFlight
		require.Equal(t, http.StatusOK, reply.Status, reply.Body)
		answered := time.Now()
		lingering.waitReturned(t)
		assert.Less(t, time.Since(answered), 15*time.Second,
			"once Ask's turns are done, the stalled request holds shutdown only for the listeners' grace")
		select {
		case err := <-stalled:
			assert.Error(t, err, "the replica drops the stalled request")
		case <-time.After(5 * time.Second):
			t.Fatal("the stalled request outlived its replica")
		}
		assert.Zero(t, lingering.logged("ask: shutdown deadline reached; cancelling the turns still running"))
	})
}
