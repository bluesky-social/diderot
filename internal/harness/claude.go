package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

// claudeCode registers through `claude mcp add-json --scope user` and reads
// the user-scope entry back from mcpServers in ~/.claude.json.
type claudeCode struct{}

func (claudeCode) Name() string   { return "claude-code" }
func (claudeCode) Detected() bool { return onPath("claude") }

// configDir is $CLAUDE_CONFIG_DIR, or ~/.claude.
func (claudeCode) configDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".claude"), nil
}

// stateFile holds user-scope MCP servers: ~/.claude.json, or .claude.json
// inside $CLAUDE_CONFIG_DIR when that is set.
func (c claudeCode) stateFile() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json"), nil
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".claude.json"), nil
}

func (c claudeCode) SkillsDir() (string, error) {
	dir, err := c.configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "skills"), nil
}

func (claudeCode) want(e Entry) map[string]any {
	entry := map[string]any{"type": "http", "url": e.URL}
	if auth := e.authorization(); auth != "" {
		entry["headers"] = map[string]any{"Authorization": auth}
	}
	if e.ClientID != "" {
		entry["oauth"] = map[string]any{"clientId": e.ClientID}
	}
	return entry
}

func (c claudeCode) existing() (string, any, error) {
	file, err := c.stateFile()
	if err != nil {
		return "", nil, err
	}
	entry, err := readJSONEntry(file, "mcpServers", ServerName)
	return file, entry, err
}

func (c claudeCode) Configure(ctx context.Context, env Env, e Entry) (Change, error) {
	_, existing, err := c.existing()
	if err != nil {
		return Change{}, err
	}
	want := c.want(e)
	body, err := json.Marshal(want)
	if err != nil {
		return Change{}, err
	}
	// Not on stdin: Claude Code refuses that inside one of its own sessions,
	// which is where an agent running setup is.
	args := []string{"mcp", "add-json", ServerName, string(body), "--scope", "user"}
	change := Change{Action: "added", Where: shown(e, "claude", args...)}
	if e.OAuth {
		change.Next = "claude mcp login " + ServerName + " (or /mcp in Claude Code) to sign Claude Code in"
	}
	if existing != nil {
		if equalJSON(existing, want) {
			change.Action = "unchanged"
			return change, nil
		}
		if err := run(ctx, env, nil, "claude", "mcp", "remove", ServerName, "--scope", "user"); err != nil {
			return Change{}, err
		}
		change.Action = "updated"
	}
	return change, run(ctx, env, nil, "claude", args...)
}

func (c claudeCode) Check(_ context.Context, e Entry) error {
	file, existing, err := c.existing()
	if err != nil {
		return err
	}
	if !equalJSON(existing, c.want(e)) {
		return mismatch(file, existing)
	}
	return nil
}
