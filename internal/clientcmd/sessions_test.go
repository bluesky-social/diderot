package clientcmd

import (
	"bufio"
	"bytes"
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/client"
	"github.com/bluesky-social/diderot/internal/sessionship"
)

// filesUnder lists every file below dir, relative to it.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, rel)
		}
		return err
	}))
	return files
}

func TestSetupOffersToShipSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "GEMINI_CLI_HOME", "OPENCODE_DB", "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR", "POLYTOKEN_DATA_PATH"} {
		t.Setenv(key, "")
	}
	session := filepath.Join(".claude", "projects", "-work-demo", "0f6b8a8e-1d47-4f6c-9a0e-3f0c4f1b2a10.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(home, session)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, session), []byte("{}\n"), 0o600))

	// The server takes sessions until pushOff, when it answers as one with
	// session push off does.
	pushOff := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if pushOff {
			http.Error(w, "push ingest is not enabled", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}))
	t.Cleanup(srv.Close)
	cfg := &client.Config{URL: srv.URL + "/mcp", Auth: client.AuthNone}
	cfgPath := filepath.Join(home, ".config", "diderot", "client.json")
	ctx := context.Background()
	ask := func(interactive bool, answer string, opts setupOptions) (*sessionsSetupReport, string) {
		var prompt bytes.Buffer
		out := &cliIO{stderr: &prompt, interactive: interactive}
		return offerSessions(ctx, out, bufio.NewReader(strings.NewReader(answer)), cfgPath, cfg, opts), prompt.String()
	}

	report, prompt := ask(false, "y\n", setupOptions{})
	assert.Nil(t, report, "off a terminal with no flag, nothing is asked or done")
	assert.Empty(t, prompt)

	report, prompt = ask(true, "y\n", setupOptions{sessions: false, sessionsSet: true})
	assert.Equal(t, "declined", report.Action)
	assert.Empty(t, prompt, "--sessions=false does not ask")

	for _, answer := range []string{"\n", "n\n", ""} {
		report, prompt = ask(true, answer, setupOptions{})
		assert.Equal(t, "declined", report.Action, "answer %q", answer)
	}
	assert.Contains(t, prompt, filepath.Join(home, ".claude", "projects")+" (1 sessions)")
	assert.Contains(t, prompt, "Ship them to "+hostOf(cfg.URL)+"? "+sessionsNotice+" [y/N]: ")
	assert.Contains(t, prompt, "a model for both session-summary jobs on its Models page ("+srv.URL+"/admin/settings/models); the model has to pass each job's test, which requires it to answer a forced tool call. ")
	assert.NotContains(t, prompt, "codex", "a harness with no sessions here is not offered")

	// A server that would refuse the sessions is not offered them: setup
	// says what it lacks before anything is installed, and asks nothing.
	pushOff = true
	report, prompt = ask(true, "yes\n", setupOptions{})
	assert.Contains(t, report.Action, "not offered: ")
	assert.Contains(t, report.Action, "DIDEROT_SESSION_INGEST_SOURCE=push")
	assert.Empty(t, report.Error, "which is not a failed setup")
	assert.Empty(t, prompt)
	// Asked for outright, it is a failure.
	report, _ = ask(true, "", setupOptions{sessions: true, sessionsSet: true})
	assert.Equal(t, "failed", report.Action)
	assert.Contains(t, report.Error, "does not accept pushed sessions")

	assert.Equal(t, []string{session}, filesUnder(t, home), "declining, or a failed install, leaves nothing behind")
}

// Shipping is agreed to for one server. When setup later saves another, the
// installed service's passes send nothing, and status says why.
func TestSessionsAreSentOnlyToTheServerAgreedTo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "GEMINI_CLI_HOME", "OPENCODE_DB", "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR", "POLYTOKEN_DATA_PATH"} {
		t.Setenv(key, "")
	}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	dir := filepath.Join(home, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "0f6b8a8e-1d47-4f6c-9a0e-3f0c4f1b2a10.jsonl"), []byte("{}\n"), 0o600))
	paths, err := sessionship.DefaultPaths()
	require.NoError(t, err)
	require.NoError(t, sessionship.SaveSettings(paths.Settings, sessionship.Settings{
		Server: "https://first.example.com/mcp", Sources: []sessionship.Source{{Harness: "claude-code", Dir: dir}},
	}))
	cfgPath, err := client.ConfigPath()
	require.NoError(t, err)
	require.NoError(t, client.SaveConfig(cfgPath, &client.Config{URL: srv.URL + "/mcp", Auth: client.AuthNone}))

	result, err := sessionPass(context.Background(), 0, true)
	require.ErrorContains(t, err, "was installed for https://first.example.com/mcp")
	assert.Empty(t, result.Sent)
	assert.Zero(t, requests, "nothing reaches the other server")
	assert.NoFileExists(t, paths.State)

	status, err := readSessionsStatus(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "https://first.example.com/mcp", status.URL)
	assert.Contains(t, status.LastError, "nothing is sent")

	// Setup for the other server, off a terminal, with or without
	// --sessions=false, leaves it that way.
	cfg := &client.Config{URL: srv.URL + "/mcp", Auth: client.AuthNone}
	for _, opts := range []setupOptions{{}, {sessionsSet: true}} {
		var prompt bytes.Buffer
		offerSessions(context.Background(), &cliIO{stderr: &prompt}, bufio.NewReader(strings.NewReader("")), cfgPath, cfg, opts)
		assert.Empty(t, prompt.String())
	}
	saved, _, err := sessionship.LoadSettings(paths.Settings)
	require.NoError(t, err)
	assert.Equal(t, "https://first.example.com/mcp", saved.Server)
	assert.Zero(t, requests)

	// With its settings gone, the background service sends nothing: it never
	// falls back to every directory it can find.
	require.NoError(t, os.Remove(paths.Settings))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755))
	_, err = sessionPass(context.Background(), 0, true)
	require.ErrorContains(t, err, "nothing is sent; run diderot sessions install")
	assert.Zero(t, requests)
}

// The state file is one server's. After an install for another server,
// status and doctor report that no pass has been made for it, not the first
// server's counts and times.
func TestStatusReportsOnlyTheConfiguredServersState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "GEMINI_CLI_HOME", "OPENCODE_DB", "PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR", "POLYTOKEN_DATA_PATH"} {
		t.Setenv(key, "")
	}
	paths, err := sessionship.DefaultPaths()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(paths.State), 0o700))
	require.NoError(t, os.WriteFile(paths.State, []byte(`{"url":"https://first.example.com/mcp",
		"files":{"/a.jsonl":{"revision":"r1"},"/b.jsonl":{"skipped":"undecodable"}},
		"last_pass":"2026-11-01T10:00:00Z","last_success":"2026-11-01T09:00:00Z",
		"last_error":"first server is down","last_error_at":"2026-11-01T10:00:00Z"}`), 0o600))
	cfgPath, err := client.ConfigPath()
	require.NoError(t, err)
	save := func(server string) {
		require.NoError(t, client.SaveConfig(cfgPath, &client.Config{URL: server, Auth: client.AuthNone}))
		require.NoError(t, sessionship.SaveSettings(paths.Settings, sessionship.Settings{Server: server}))
	}

	save("https://second.example.com/mcp")
	status, err := readSessionsStatus(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "https://second.example.com/mcp", status.URL)
	assert.Zero(t, status.Sent)
	assert.Empty(t, status.Skipped)
	assert.Nil(t, status.LastPass)
	assert.Nil(t, status.LastSuccess)
	assert.Empty(t, status.LastError)
	assert.Equal(t, "no pass yet for this server", ago(status.LastPass))
}

// Uninstall leaves the state file and its lock: a sync still running holds
// the lock, and removing it would let a second pass lock a new file.
func TestUninstallKeepsTheStateAndItsLock(t *testing.T) {
	dir := t.TempDir()
	paths := sessionship.Paths{
		Settings: filepath.Join(dir, "sessions.json"), State: filepath.Join(dir, "sessions-state.json"), Log: filepath.Join(dir, "sessions.log"),
	}
	for _, file := range []string{paths.Settings, paths.State, paths.State + ".lock", paths.Log} {
		require.NoError(t, os.WriteFile(file, nil, 0o600))
	}
	removed, err := removeInstalledFiles(paths)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{paths.Settings, paths.Log}, removed)
	assert.ElementsMatch(t, []string{"sessions-state.json", "sessions-state.json.lock"}, filesUnder(t, dir))
}

func TestSessionsCommandsAreUsageChecked(t *testing.T) {
	cmd := sessionsCmd(Options{})
	var names []string
	for _, sub := range cmd.Commands() {
		names = append(names, sub.Name())
		assert.Equal(t, exitUsage, exitCode(sub.Args(sub, []string{"extra"})), sub.Name())
	}
	assert.ElementsMatch(t, []string{"sync", "run", "install", "uninstall", "status"}, names)
	assert.Equal(t, exitUsage, exitCode(cmd.Args(cmd, []string{"bogus"})))
}
