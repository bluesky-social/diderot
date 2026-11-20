package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// askOutcome is one recorded turn: how it ended, and the error it recorded.
type askOutcome struct{ Outcome, Error string }

// sessionOutcomes are the recorded turns of a session, oldest first.
func sessionOutcomes(t *testing.T, pool *pgxpool.Pool, sessionID string) []askOutcome {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT outcome, COALESCE(error, '') FROM ask_runs
		 WHERE session_id = $1 ORDER BY started_at, id`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var out []askOutcome
	for rows.Next() {
		var run askOutcome
		require.NoError(t, rows.Scan(&run.Outcome, &run.Error))
		out = append(out, run)
	}
	require.NoError(t, rows.Err())
	return out
}

// metricSamples reads the samples of the server at addr's /metrics, by
// their name and labels as written.
func metricSamples(t *testing.T, addr string) map[string]float64 {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	samples := map[string]float64{}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		cut := strings.LastIndexByte(line, ' ')
		if strings.HasPrefix(line, "#") || cut < 0 {
			continue
		}
		if value, err := strconv.ParseFloat(line[cut+1:], 64); err == nil {
			samples[line[:cut]] = value
		}
	}
	require.NoError(t, scanner.Err())
	return samples
}

// adminPage reads a page of the replica's admin UI.
func (r *askReplica) adminPage(t *testing.T, path string) string {
	t.Helper()
	resp, err := http.Get(r.base + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	return string(raw)
}

// TestAskTurnItsCallerCancels pins what happens to a turn whose caller goes
// away while the model works: a /v1/ask whose client closes the connection,
// and an MCP ask task its client cancels. The turn stops, starting no
// further model or tool call; it is recorded as cancelled, with no error;
// the admin UI shows it as cancelled; and neither a failed turn nor a failed
// model request is counted.
func TestAskTurnItsCallerCancels(t *testing.T) {
	model := newFakeAskModel(t)
	askServeEnv(t, "cmd_serve_ask_cancel", model)
	t.Setenv("DIDEROT_LEADER_ELECTION", "false")
	replica := startAskReplica(t, "replica")
	metricsAddr := os.Getenv("DIDEROT_METRICS_LISTEN_ADDR")
	pool, err := pgxpool.New(t.Context(), os.Getenv("DIDEROT_DB_URL"))
	require.NoError(t, err)
	defer pool.Close()
	before := metricSamples(t, metricsAddr)

	const (
		turnsCancelled = `diderot_ask_turns_total{outcome="cancelled",surface="http"}`
		turnsFailed    = `diderot_ask_turns_total{outcome="error",surface="http"}`
		modelCancelled = `diderot_ask_model_requests_total{outcome="cancelled",surface="http"}`
		modelFailed    = `diderot_ask_model_requests_total{outcome="error",surface="http"}`
	)

	t.Run("a /v1/ask whose client closes the connection", func(t *testing.T) {
		first := replica.ask(t, "", "The question before the cancelled one")
		require.Equal(t, http.StatusOK, first.Status, first.Body)
		// The admin UI orders a turn's end (the server's clock) against the
		// answer before it (the database's): keep them further apart than
		// the two clocks are.
		time.Sleep(time.Second)

		// LIST makes the model's first reply a tool call, so a turn that
		// went on would call the tool and the model again.
		held := model.hold("HOLD-API")
		body, err := json.Marshal(map[string]any{"question": "HOLD-API LIST: the caller leaves", "session_id": first.SessionID})
		require.NoError(t, err)
		ctx, hangUp := context.WithCancel(t.Context())
		defer hangUp()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, replica.base+"/v1/ask", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		done := make(chan error, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()
		held.waitArrived(t)
		hangUp()
		require.ErrorIs(t, <-done, context.Canceled)

		replica.waitLog(t, "ask: turn cancelled by its caller")
		require.Eventually(t, func() bool { return len(sessionOutcomes(t, pool, first.SessionID)) == 2 }, time.Minute, 50*time.Millisecond)
		assert.Equal(t, []askOutcome{{Outcome: "answered"}, {Outcome: "cancelled"}}, sessionOutcomes(t, pool, first.SessionID))
		assert.Zero(t, replica.logged("ask: turn failed"))

		time.Sleep(time.Second)
		assert.Len(t, model.containing("HOLD-API"), 1, "the cancelled turn made no model call after the one it was in")
		var toolCalls int
		require.NoError(t, pool.QueryRow(t.Context(), `
			SELECT count(*) FROM ask_tool_calls c JOIN ask_runs r ON r.id = c.run_id
			 WHERE r.session_id = $1`, first.SessionID).Scan(&toolCalls))
		assert.Zero(t, toolCalls, "the cancelled turn called no tool")

		list := replica.adminPage(t, "/admin/ask/sessions?status=cancelled")
		assert.Contains(t, list, first.SessionID, "the conversation is listed as cancelled")
		assert.NotContains(t, replica.adminPage(t, "/admin/ask/sessions?status=failed"), first.SessionID)
		page := replica.adminPage(t, "/admin/ask/sessions/"+first.SessionID)
		assert.Contains(t, page, ">cancelled<", "the conversation's page shows its state")
		assert.NotContains(t, page, "got no answer")
		assert.NotContains(t, page, "context canceled")

		// The session is free again, and its history holds no half turn.
		next := replica.ask(t, first.SessionID, "The question after the cancelled one")
		require.Equal(t, http.StatusOK, next.Status, next.Body)
	})

	t.Run("an MCP ask task its client cancels", func(t *testing.T) {
		held := model.hold("HOLD-MCP")
		started := time.Now()
		taskID := replica.startMCPAsk(t, "HOLD-MCP LIST: the client cancels")
		held.waitArrived(t)
		replica.mcpCall(t, "tasks/cancel", taskID, map[string]any{"taskId": taskID})

		task := replica.pollMCPTask(t, taskID)
		assert.Equal(t, "cancelled", task["status"], "%v", task)
		assert.Nil(t, task["error"], "a cancelled task did not fail")

		// The task's turn is the newest one recorded.
		var run askOutcome
		require.Eventually(t, func() bool {
			err := pool.QueryRow(t.Context(), `
				SELECT outcome, COALESCE(error, '') FROM ask_runs
				 WHERE started_at > $1 ORDER BY started_at DESC LIMIT 1`, started).Scan(&run.Outcome, &run.Error)
			return err == nil
		}, time.Minute, 50*time.Millisecond)
		assert.Equal(t, askOutcome{Outcome: "cancelled"}, run)

		time.Sleep(time.Second)
		assert.Len(t, model.containing("HOLD-MCP"), 1, "the cancelled task made no model call after the one it was in")
	})

	after := metricSamples(t, metricsAddr)
	assert.Equal(t, float64(2), after[turnsCancelled]-before[turnsCancelled])
	assert.Positive(t, after[modelCancelled]-before[modelCancelled], "the model requests cut short are counted as cancelled")
	assert.Zero(t, after[turnsFailed]-before[turnsFailed], "a cancelled turn is not a failed one")
	assert.Zero(t, after[modelFailed]-before[modelFailed], "a model request its caller cut short did not fail")

	replica.shutDown(t)
	assert.Empty(t, replica.errorsLogged())
}
