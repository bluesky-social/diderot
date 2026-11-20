package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// pi registers through `pi mcp add`, which replaces an entry of the same
// name, and reads it back from mcp.json in its agent directory. Pi has MCP
// from 1.0; earlier versions have no `pi mcp`, and running `pi mcp ...`
// there would start a session with "mcp" as the prompt, so the version is
// checked first.
type pi struct{}

func (pi) Name() string   { return "pi" }
func (pi) Detected() bool { return onPath("pi") }

// agentDir is $PI_CODING_AGENT_DIR, or ~/.pi/agent.
func (pi) agentDir() (string, error) {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return dir, nil
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".pi", "agent"), nil
}

func (pi) SkillsDir() (string, error) { return agentsSkillsDir() }

func (p pi) file() (string, error) {
	dir, err := p.agentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mcp.json"), nil
}

func (pi) checkVersion(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "pi", "--version").Output()
	if err != nil {
		return fmt.Errorf("pi --version: %w", err)
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	major, err := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	if err != nil {
		return fmt.Errorf("pi --version printed %q, not a version", version)
	}
	if major < 1 {
		return fmt.Errorf("pi %s has no MCP support; Pi 1.0 or later does (pi update)", version)
	}
	return nil
}

func (pi) args(e Entry) []string {
	args := []string{"mcp", "add", ServerName, "--url", e.URL}
	if e.ClientID != "" {
		args = append(args, "--oauth-client-id", e.ClientID)
	}
	if auth := e.authorization(); auth != "" {
		args = append(args, "--header", "Authorization="+auth)
	}
	return args
}

// matches compares what setup decides: the URL, the Authorization header,
// and the OAuth client. Pi may add fields of its own.
func (pi) matches(existing any, e Entry) bool {
	entry, ok := existing.(map[string]any)
	if !ok || entry["command"] != nil || entry["url"] != e.URL {
		return false
	}
	headers, _ := entry["headers"].(map[string]any)
	auth, _ := headers["Authorization"].(string)
	oauth, _ := entry["oauth"].(map[string]any)
	clientID, _ := oauth["clientId"].(string)
	return auth == e.authorization() && clientID == e.ClientID
}

func (p pi) Configure(ctx context.Context, env Env, e Entry) (Change, error) {
	if err := p.checkVersion(ctx); err != nil {
		return Change{}, err
	}
	file, err := p.file()
	if err != nil {
		return Change{}, err
	}
	existing, err := readJSONEntry(file, "mcpServers", ServerName)
	if err != nil {
		return Change{}, err
	}
	args := p.args(e)
	change := Change{Action: "added", Where: shown(e, "pi", args...)}
	if e.OAuth {
		change.Next = "pi mcp login " + ServerName + " to sign Pi in"
	}
	if p.matches(existing, e) {
		change.Action = "unchanged"
		return change, nil
	}
	if existing != nil {
		change.Action = "updated"
	}
	return change, run(ctx, env, nil, "pi", args...)
}

func (p pi) Check(_ context.Context, e Entry) error {
	file, err := p.file()
	if err != nil {
		return err
	}
	existing, err := readJSONEntry(file, "mcpServers", ServerName)
	if err != nil {
		return err
	}
	if !p.matches(existing, e) {
		return mismatch(file, existing)
	}
	return nil
}
