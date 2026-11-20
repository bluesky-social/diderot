package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/auth/authtest"
	"github.com/bluesky-social/diderot/internal/clientcmd"
)

// The client commands' documented exit codes.
const (
	exitFailure = 1
	exitUsage   = 2
	exitSignIn  = 3
)

// clientBinary builds the client-only binary the releases carry, so the
// end-to-end tests run what people download. Call it before the test's HOME
// moves: the go command keeps its module cache under HOME.
func clientBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "diderot")
	build := exec.Command("go", "build", "-o", bin, "github.com/bluesky-social/diderot")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return bin
}

// clientRun runs one command of the client-only binary the way a script
// would: stdin is not a terminal and stdout is read as JSON. The test's
// environment (its HOME) is the binary's.
func clientRun(t *testing.T, bin, stdin string, args ...string) (map[string]any, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	var exit *exec.ExitError
	if err := cmd.Run(); errors.As(err, &exit) {
		code = exit.ExitCode()
	} else {
		require.NoError(t, err)
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out), "stdout of %v is one JSON object: %s\nstderr: %s", args, stdout.String(), stderr.String())
	return out, code
}

// TestClientAgainstARealServer runs setup, doctor and ask, as a user's
// machine would, against a real server with an API token.
func TestClientAgainstARealServer(t *testing.T) {
	model := newFakeAskModel(t)
	askServeEnv(t, "cmd_client_e2e", model)
	t.Setenv("DIDEROT_LEADER_ELECTION", "false")
	token := "client-e2e-token-0123456789abcdef"
	digest := sha256.Sum256([]byte(token))
	tokens := filepath.Join(t.TempDir(), "tokens.yaml")
	require.NoError(t, os.WriteFile(tokens, []byte("tokens:\n  - sha256: "+hex.EncodeToString(digest[:])+
		"\n    email: person@example.com\n    groups: [admins]\n"), 0o600))
	t.Setenv("DIDEROT_AUTH_MODE", "static")
	t.Setenv("DIDEROT_AUTH_STATIC_TOKENS_FILE", tokens)
	t.Setenv("DIDEROT_AUTH_NONE_EMAIL", "")
	replica := startAskReplica(t, "client")

	bin := clientBinary(t)
	home := isolateClientHome(t)
	url := replica.base + "/mcp"

	out, code := clientRun(t, bin, "", "setup")
	assert.Equal(t, exitUsage, code, "%v", out)
	assert.Contains(t, out["error"], "one argument")
	out, code = clientRun(t, bin, "", "ask", "--no-such-flag", "hello")
	assert.Equal(t, exitUsage, code, "%v", out)

	out, code = clientRun(t, bin, "", "setup", replica.base, "--harness", "cursor,opencode")
	assert.Equal(t, exitSignIn, code, "%v", out)
	assert.Contains(t, out["error"], "--token-stdin")

	out, code = clientRun(t, bin, "not-the-token", "setup", replica.base, "--harness", "cursor", "--token-stdin")
	assert.Equal(t, exitSignIn, code, "%v", out)

	out, code = clientRun(t, bin, token+"\n", "setup", replica.base, "--harness", "cursor,opencode", "--token-stdin")
	require.Equal(t, 0, code, "%v", out)
	assert.Equal(t, url, out["url"])
	assert.Equal(t, "token", out["auth"])
	assert.Equal(t, "new", out["sign_in"])
	cursorFile := filepath.Join(home, ".cursor", "mcp.json")
	harnesses, _ := out["harnesses"].([]any)
	require.Len(t, harnesses, 2)
	assert.Equal(t, map[string]any{"name": "cursor", "action": "added", "where": cursorFile}, harnesses[0])
	cursorConfig, err := os.ReadFile(cursorFile) //nolint:gosec // under the test's home
	require.NoError(t, err)
	assert.JSONEq(t, `{"mcpServers":{"diderot":{"url":"`+url+`","headers":{"Authorization":"Bearer `+token+`"}}}}`, string(cursorConfig))
	assert.FileExists(t, filepath.Join(home, ".agents", "skills", "diderot", "SKILL.md"))
	saved, err := os.Stat(filepath.Join(home, ".config", "diderot", "client.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), saved.Mode().Perm())

	out, code = clientRun(t, bin, "", "setup", replica.base, "--harness", "cursor")
	require.Equal(t, 0, code, "%v", out)
	assert.Equal(t, "kept", out["sign_in"], "a working saved token is kept")
	harnesses, _ = out["harnesses"].([]any)
	require.Len(t, harnesses, 2, "harnesses set up before are kept in view")
	for _, h := range harnesses {
		assert.Equal(t, "unchanged", h.(map[string]any)["action"])
	}

	out, code = clientRun(t, bin, "", "doctor")
	require.Equal(t, 0, code, "%v", out)
	assert.Equal(t, true, out["ok"])
	var names []string
	for _, c := range out["checks"].([]any) {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	assert.Equal(t, []string{"config", "sessions", "server", "sign-in", "protocol", "tools", "cursor", "opencode", "skill"}, names)

	out, code = clientRun(t, bin, "", "ask", "Where is the first question?")
	require.Equal(t, 0, code, "%v", out)
	session, _ := out["session_id"].(string)
	require.NotEmpty(t, session)
	first, _ := out["reply"].(string)
	require.NotEmpty(t, first)

	out, code = clientRun(t, bin, "And the follow-up?", "ask", "--session", session)
	require.Equal(t, 0, code, "%v", out)
	assert.Equal(t, session, out["session_id"], "the follow-up continues the session")
	followUp := model.containing("And the follow-up?")
	require.Len(t, followUp, 1)
	assert.Contains(t, followUp[0].Raw, first, "the follow-up carries the conversation so far")

	out, code = clientRun(t, bin, "", "ask", "--session", "00000000-0000-4000-8000-000000000000", "Whose session is this?")
	assert.Equal(t, exitFailure, code, "%v", out)
	assert.Contains(t, out["error"], "not an ask session of yours")

	require.NoError(t, os.WriteFile(cursorFile, []byte(`{"mcpServers":{}}`), 0o600))
	out, code = clientRun(t, bin, "", "doctor")
	assert.Equal(t, exitFailure, code)
	for _, c := range out["checks"].([]any) {
		check := c.(map[string]any)
		assert.Equal(t, check["name"] != "cursor", check["ok"], "%v", check)
	}
}

// isolateClientHome points the client and every harness at a fresh home.
func isolateClientHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR", "DIDEROT_TOKEN"} {
		t.Setenv(key, "")
	}
	return home
}

// TestClientSignsInToAnOIDCServer: a server in oidc mode, with no gateway,
// publishes where to sign in, and setup signs in there in the browser, with
// the client ID its administrator gives; doctor and ask then work with the
// access token the identity provider issued.
func TestClientSignsInToAnOIDCServer(t *testing.T) {
	model := newFakeAskModel(t)
	askServeEnv(t, "cmd_client_oidc_e2e", model)
	t.Setenv("DIDEROT_LEADER_ELECTION", "false")
	idp := authtest.NewIdP(t)
	idp.RegisterClient("diderot-cli", "")
	idp.SetUser(map[string]any{
		"sub": "person-1", "email": "person@example.com", "email_verified": true, "groups": []string{"admins"},
	})
	idp.IssueAccessTokens("diderot")
	for key, value := range map[string]string{
		"DIDEROT_AUTH_MODE": "oidc", "DIDEROT_AUTH_NONE_EMAIL": "",
		"DIDEROT_OIDC_ISSUER": idp.Issuer, "DIDEROT_OIDC_JWKS_URL": idp.JWKSURL, "DIDEROT_OIDC_AUDIENCE": "diderot",
	} {
		t.Setenv(key, value)
	}
	replica := startAskReplica(t, "oidc")
	bin := clientBinary(t)
	isolateClientHome(t)

	// The browser: follow the authorization page to the loopback redirect,
	// as a person signing in would.
	var opened []string
	browser := func(u string) error {
		opened = append(opened, u)
		go func() {
			resp, err := http.Get(u) //nolint:gosec,noctx // the test's own identity provider
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}
	// A person at a terminal runs setup: the client commands as both
	// binaries mount them, with the browser above.
	personRuns := func(args ...string) (map[string]any, int) {
		root := &cobra.Command{Use: "diderot", SilenceUsage: true, SilenceErrors: true}
		clientcmd.AddCommands(root, clientcmd.Options{
			Version: "test", OpenBrowser: browser, Interactive: func(io.Reader) bool { return true },
		})
		var stdout bytes.Buffer
		root.SetArgs(args)
		root.SetIn(strings.NewReader(""))
		root.SetOut(&stdout)
		root.SetErr(io.Discard)
		code := clientcmd.ExitCode(clientcmd.Execute(t.Context(), root))
		var out map[string]any
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &out), "%s", stdout.String())
		return out, code
	}

	// Off a terminal, a browser sign-in is refused, not waited for.
	out, code := clientRun(t, bin, "", "setup", replica.base, "--harness", "none")
	assert.Equal(t, exitSignIn, code, "%v", out)
	assert.Contains(t, out["error"], "in a terminal")

	// The identity provider registers no clients, so without the
	// administrator's client ID setup says to ask for it.
	out, code = personRuns("setup", replica.base, "--harness", "none")
	assert.Equal(t, exitSignIn, code, "%v", out)
	assert.Contains(t, out["error"], "--client-id")

	out, code = personRuns("setup", replica.base, "--harness", "none", "--client-id", "diderot-cli")
	require.Equal(t, 0, code, "%v", out)
	assert.Equal(t, "person@example.com", out["account"])
	require.Len(t, opened, 1)
	authorize, err := url.Parse(opened[0])
	require.NoError(t, err)
	assert.Equal(t, idp.Issuer+"/authorize", authorize.Scheme+"://"+authorize.Host+authorize.Path)
	// The client library lists the scopes in no fixed order (it collects
	// them from a map), so the set is what is compared.
	assert.ElementsMatch(t, []string{"openid", "email", "profile"}, strings.Fields(authorize.Query().Get("scope")),
		"the scopes the server advertises")
	assert.Equal(t, replica.base+"/mcp", authorize.Query().Get("resource"))

	out, code = clientRun(t, bin, "", "doctor")
	require.Equal(t, 0, code, "%v", out)
	checks := map[string]string{}
	for _, c := range out["checks"].([]any) {
		check := c.(map[string]any)
		checks[check["name"].(string)] = check["detail"].(string)
	}
	assert.Equal(t, "person@example.com", checks["sign-in"])

	out, code = clientRun(t, bin, "", "ask", "Where is the first question?")
	require.Equal(t, 0, code, "%v", out)
	session, _ := out["session_id"].(string)
	require.NotEmpty(t, session)
	out, code = clientRun(t, bin, "", "ask", "--session", session, "And the follow-up?")
	require.Equal(t, 0, code, "%v", out)
	assert.Equal(t, session, out["session_id"])
}
