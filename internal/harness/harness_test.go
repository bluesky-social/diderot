package harness

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/skills"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// isolate points every harness at a fresh home and a PATH holding only the
// fake harness CLIs the test writes, so no test touches a real harness.
func isolate(t *testing.T) (home, bin, log string) {
	t.Helper()
	home = t.TempDir()
	bin = filepath.Join(home, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	log = filepath.Join(home, "calls.log")
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("FAKE_LOG", log)
	for _, key := range []string{"XDG_CONFIG_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR", TokenEnv} {
		t.Setenv(key, "")
	}
	return home, bin, log
}

// fakeCLI writes a harness CLI that records each call, one argument per
// line and a "--" after, and prints stdout for `--version`.
func fakeCLI(t *testing.T, bin, name, version string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo " + version + "; exit 0; fi\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$FAKE_LOG\"; done\n" +
		"echo -- >> \"$FAKE_LOG\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755)) //nolint:gosec // an executable fake
}

// calls returns the recorded calls, each as its arguments.
func calls(t *testing.T, log string) [][]string {
	t.Helper()
	b, err := os.ReadFile(log) //nolint:gosec // the test's own log
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "--" {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return out
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644)) //nolint:gosec // a test fixture
	}
	want, err := os.ReadFile(path) //nolint:gosec // a test fixture
	require.NoError(t, err)
	assert.Equal(t, string(want), got, "golden %s (go test -run %s -update to rewrite)", name, t.Name())
}

func write(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), perm))
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // under the test's home
	require.NoError(t, err)
	return string(b)
}

var (
	tokenEntry = Entry{URL: "https://diderot.example.com/mcp", Token: "tok-123"}
	oauthEntry = Entry{URL: "https://gateway.example.com/mcp", OAuth: true, ClientID: "public-client"}
)

func TestCursorPatchesItsFile(t *testing.T) {
	home, _, _ := isolate(t)
	file := filepath.Join(home, ".cursor", "mcp.json")
	h, _ := Lookup("cursor")

	change, err := h.Configure(t.Context(), Env{}, tokenEntry)
	require.NoError(t, err)
	assert.Equal(t, Change{Action: "added", Where: file}, change)
	golden(t, "cursor_new.json", read(t, file))
	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a new file holding a token is private")
	require.NoError(t, h.Check(t.Context(), tokenEntry))

	before := read(t, file)
	change, err = h.Configure(t.Context(), Env{}, tokenEntry)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", change.Action)
	assert.Equal(t, before, read(t, file), "an entry that matches is not rewritten")

	assert.ErrorContains(t, h.Check(t.Context(), oauthEntry), "differs")
}

func TestCursorKeepsCommentsAndOtherServers(t *testing.T) {
	home, _, _ := isolate(t)
	file := filepath.Join(home, ".cursor", "mcp.json")
	write(t, file, `{
  // my servers
  "mcpServers": {
    "github": { "url": "https://api.example.com/mcp" }, // keep me
    "diderot": { "url": "https://old.example.com/mcp" }
  },
  "other": true
}
`, 0o644)
	h, _ := Lookup("cursor")
	change, err := h.Configure(t.Context(), Env{}, oauthEntry)
	require.NoError(t, err)
	assert.Equal(t, "updated", change.Action)
	assert.Contains(t, change.Next, "Cursor's MCP settings")
	golden(t, "cursor_existing.jsonc", read(t, file))
	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "an existing file keeps its mode")

	_, err = h.Configure(t.Context(), Env{}, tokenEntry)
	assert.ErrorContains(t, err, "chmod 600 "+file, "no token goes into a file others can read")
	assert.NotContains(t, read(t, file), "tok-123")
}

func TestCursorCreatesTheServersObject(t *testing.T) {
	home, _, _ := isolate(t)
	file := filepath.Join(home, ".cursor", "mcp.json")
	write(t, file, "{\n  \"theme\": \"dark\" /* trailing */\n}\n", 0o600)
	h, _ := Lookup("cursor")
	_, err := h.Configure(t.Context(), Env{}, oauthEntry)
	require.NoError(t, err)
	golden(t, "cursor_no_servers.jsonc", read(t, file))
	require.NoError(t, h.Check(t.Context(), oauthEntry))
}

func TestCursorOneLineFileStaysOneLine(t *testing.T) {
	home, _, _ := isolate(t)
	file := filepath.Join(home, ".cursor", "mcp.json")
	write(t, file, `{"mcpServers":{"github":{"url":"https://api.example.com/mcp"}}}`, 0o600)
	h, _ := Lookup("cursor")
	_, err := h.Configure(t.Context(), Env{}, oauthEntry)
	require.NoError(t, err)
	assert.Equal(t, `{"mcpServers":{"github":{"url":"https://api.example.com/mcp"},"diderot":{"auth":{"CLIENT_ID":"public-client"},"url":"https://gateway.example.com/mcp"}}}`, read(t, file))
}

func TestOpenCodePatchesItsGlobalConfig(t *testing.T) {
	home, _, _ := isolate(t)
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	file := filepath.Join(xdg, "opencode", "opencode.jsonc")
	write(t, file, `{
  "$schema": "https://opencode.ai/config.json",
  // models I like
  "model": "provider/model",
  "mcp": {
    "local-tool": { "type": "local", "command": ["tool"] },
  },
}
`, 0o600)
	h, _ := Lookup("opencode")
	change, err := h.Configure(t.Context(), Env{}, oauthEntry)
	require.NoError(t, err)
	assert.Equal(t, Change{Action: "added", Where: file, Next: "opencode mcp auth diderot to sign OpenCode in"}, change)
	golden(t, "opencode_existing.jsonc", read(t, file))
	require.NoError(t, h.Check(t.Context(), oauthEntry))

	change, err = h.Configure(t.Context(), Env{}, oauthEntry)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", change.Action)
}

func TestOpenCodeNewFile(t *testing.T) {
	home, _, _ := isolate(t)
	h, _ := Lookup("opencode")
	_, err := h.Configure(t.Context(), Env{}, tokenEntry)
	require.NoError(t, err)
	golden(t, "opencode_new.json", read(t, filepath.Join(home, ".config", "opencode", "opencode.json")))
}

func TestClaudeCodeThroughItsCLI(t *testing.T) {
	home, bin, log := isolate(t)
	fakeCLI(t, bin, "claude", "2.1.0")
	h, _ := Lookup("claude-code")
	assert.True(t, h.Detected())

	change, err := h.Configure(t.Context(), Env{}, oauthEntry)
	require.NoError(t, err)
	want := `{"oauth":{"clientId":"public-client"},"type":"http","url":"https://gateway.example.com/mcp"}`
	assert.Equal(t, [][]string{{"mcp", "add-json", "diderot", want, "--scope", "user"}}, calls(t, log))
	assert.Equal(t, "added", change.Action)
	assert.Contains(t, change.Next, "claude mcp login diderot")

	// What claude wrote, as it writes it: the entry, unchanged.
	state := filepath.Join(home, ".claude.json")
	write(t, state, `{"numStartups": 3, "mcpServers": {"diderot": `+want+`}}`, 0o600)
	require.NoError(t, os.Remove(log))
	change, err = h.Configure(t.Context(), Env{}, oauthEntry)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", change.Action)
	assert.Empty(t, calls(t, log))
	require.NoError(t, h.Check(t.Context(), oauthEntry))

	change, err = h.Configure(t.Context(), Env{}, tokenEntry)
	require.NoError(t, err)
	assert.Equal(t, "updated", change.Action)
	got := calls(t, log)
	require.Len(t, got, 2)
	assert.Equal(t, []string{"mcp", "remove", "diderot", "--scope", "user"}, got[0])
	assert.JSONEq(t, `{"type":"http","url":"https://diderot.example.com/mcp","headers":{"Authorization":"Bearer tok-123"}}`, got[1][3])
	assert.NotContains(t, change.Where, "tok-123", "the token is not shown")
	assert.Contains(t, change.Where, "Bearer <token>")
}

func TestClaudeCodeConfigDir(t *testing.T) {
	home, _, _ := isolate(t)
	dir := filepath.Join(home, "claude-config")
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	write(t, filepath.Join(dir, ".claude.json"), `{"mcpServers":{"diderot":{"type":"http","url":"https://gateway.example.com/mcp","oauth":{"clientId":"public-client"}}}}`, 0o600)
	h, _ := Lookup("claude-code")
	require.NoError(t, h.Check(t.Context(), oauthEntry))
	skillsDir, err := h.SkillsDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "skills"), skillsDir)
}

func TestCodexThroughItsCLI(t *testing.T) {
	home, bin, log := isolate(t)
	fakeCLI(t, bin, "codex", "codex-cli 0.169.0")
	h, _ := Lookup("codex")

	change, err := h.Configure(t.Context(), Env{}, tokenEntry)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"mcp", "add", "diderot", "--url", "https://diderot.example.com/mcp", "--bearer-token-env-var", "DIDEROT_TOKEN"}}, calls(t, log))
	assert.Contains(t, change.Next, "export DIDEROT_TOKEN=")

	// Codex's own sign-in would start, and no one is at a terminal.
	require.NoError(t, os.Remove(log))
	_, err = h.Configure(t.Context(), Env{}, oauthEntry)
	assert.ErrorIs(t, err, ErrNeedsTerminal)
	assert.Empty(t, calls(t, log))

	_, err = h.Configure(t.Context(), Env{Interactive: true}, oauthEntry)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"mcp", "add", "diderot", "--url", "https://gateway.example.com/mcp", "--oauth-client-id", "public-client"}}, calls(t, log))

	// What codex writes; an entry that matches needs no terminal.
	write(t, filepath.Join(home, ".codex", "config.toml"), `model = "m"

[mcp_servers.diderot]
url = "https://gateway.example.com/mcp"

[mcp_servers.diderot.oauth]
client_id = "public-client"
callback_url = "http://127.0.0.1/callback/abc"
`, 0o600)
	require.NoError(t, os.Remove(log))
	change, err = h.Configure(t.Context(), Env{}, oauthEntry)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", change.Action)
	assert.Empty(t, calls(t, log))
	require.NoError(t, h.Check(t.Context(), oauthEntry))
	assert.ErrorContains(t, h.Check(t.Context(), tokenEntry), "differs")
}

func TestCodexTokenNeedsTheEnvironment(t *testing.T) {
	home, _, _ := isolate(t)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))
	write(t, filepath.Join(home, "codex-home", "config.toml"), "[mcp_servers.diderot]\nurl = \"https://diderot.example.com/mcp\"\nbearer_token_env_var = \"DIDEROT_TOKEN\"\n", 0o600)
	h, _ := Lookup("codex")
	assert.ErrorContains(t, h.Check(t.Context(), tokenEntry), "DIDEROT_TOKEN is not set")
	t.Setenv(TokenEnv, "tok-123")
	require.NoError(t, h.Check(t.Context(), tokenEntry))
}

func TestPiThroughItsCLI(t *testing.T) {
	home, bin, log := isolate(t)
	fakeCLI(t, bin, "pi", "0.72.1")
	h, _ := Lookup("pi")
	_, err := h.Configure(t.Context(), Env{}, oauthEntry)
	assert.ErrorContains(t, err, "pi 0.72.1 has no MCP support")
	assert.Empty(t, calls(t, log), "an old pi is never run with mcp arguments")

	fakeCLI(t, bin, "pi", "1.5.2")
	_, err = h.Configure(t.Context(), Env{}, tokenEntry)
	require.NoError(t, err)
	assert.Equal(t, [][]string{{"mcp", "add", "diderot", "--url", "https://diderot.example.com/mcp", "--header", "Authorization=Bearer tok-123"}}, calls(t, log))

	write(t, filepath.Join(home, ".pi", "agent", "mcp.json"), `{"mcpServers":{"diderot":{"url":"https://gateway.example.com/mcp","oauth":{"clientId":"public-client"}}}}`, 0o600)
	require.NoError(t, os.Remove(log))
	change, err := h.Configure(t.Context(), Env{}, oauthEntry)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", change.Action)
	assert.Empty(t, calls(t, log))
	require.NoError(t, h.Check(t.Context(), oauthEntry))
}

func TestSkillInstall(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "diderot")
	want, err := skills.FS.ReadFile("diderot/SKILL.md")
	require.NoError(t, err)

	assert.ErrorContains(t, CheckSkill(parent), "not installed")
	change, err := InstallSkill(parent)
	require.NoError(t, err)
	assert.Equal(t, Change{Action: "added", Where: dir}, change)
	assert.Equal(t, string(want), read(t, filepath.Join(dir, "SKILL.md")))
	require.NoError(t, CheckSkill(parent))

	change, err = InstallSkill(parent)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", change.Action)

	write(t, filepath.Join(dir, "SKILL.md"), "edited", 0o644)
	write(t, filepath.Join(dir, "old.md"), "gone in this version", 0o644)
	assert.ErrorContains(t, CheckSkill(parent), "differs")
	change, err = InstallSkill(parent)
	require.NoError(t, err)
	assert.Equal(t, "updated", change.Action)
	assert.Equal(t, string(want), read(t, filepath.Join(dir, "SKILL.md")))
	assert.NoFileExists(t, filepath.Join(dir, "old.md"))
}

func TestSkillSymlinkIsLeftAlone(t *testing.T) {
	parent := t.TempDir()
	checkout := t.TempDir()
	write(t, filepath.Join(checkout, "SKILL.md"), "a development copy", 0o644)
	require.NoError(t, os.Symlink(checkout, filepath.Join(parent, "diderot")))
	change, err := InstallSkill(parent)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", change.Action)
	assert.Contains(t, change.Where, "symlink")
	assert.Equal(t, "a development copy", read(t, filepath.Join(checkout, "SKILL.md")))
	require.NoError(t, CheckSkill(parent))
}

func TestSkillDirectories(t *testing.T) {
	home, _, _ := isolate(t)
	for _, name := range []string{"codex", "cursor", "opencode", "pi"} {
		h, _ := Lookup(name)
		dir, err := h.SkillsDir()
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(home, ".agents", "skills"), dir, name)
	}
	h, _ := Lookup("claude-code")
	dir, err := h.SkillsDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".claude", "skills"), dir)
}
