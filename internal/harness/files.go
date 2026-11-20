package harness

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// cursor has no CLI that adds a server, so setup patches ~/.cursor/mcp.json.
type cursor struct{}

func (cursor) Name() string { return "cursor" }

func (cursor) file() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".cursor", "mcp.json"), nil
}

func (c cursor) Detected() bool {
	if onPath("cursor") || onPath("cursor-agent") {
		return true
	}
	h, err := home()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(h, ".cursor"))
	return err == nil
}

func (cursor) SkillsDir() (string, error) { return agentsSkillsDir() }

func (cursor) want(e Entry) map[string]any {
	entry := map[string]any{"url": e.URL}
	if auth := e.authorization(); auth != "" {
		entry["headers"] = map[string]any{"Authorization": auth}
	}
	if e.ClientID != "" {
		entry["auth"] = map[string]any{"CLIENT_ID": e.ClientID}
	}
	return entry
}

func (c cursor) Configure(_ context.Context, _ Env, e Entry) (Change, error) {
	file, err := c.file()
	if err != nil {
		return Change{}, err
	}
	change, err := patchEntry(file, c.want(e), e.Token != "", "mcpServers", ServerName)
	if e.OAuth {
		change.Next = "sign in to " + ServerName + " in Cursor's MCP settings"
	}
	return change, err
}

func (c cursor) Check(_ context.Context, e Entry) error {
	file, err := c.file()
	if err != nil {
		return err
	}
	return checkEntry(file, c.want(e), "mcpServers", ServerName)
}

// openCode's `opencode mcp add` only asks questions, so setup patches its
// global configuration. The entry is the `mcp.<name>` shape OpenCode 1.x
// reads and 2.x still accepts.
type openCode struct{}

func (openCode) Name() string   { return "opencode" }
func (openCode) Detected() bool { return onPath("opencode") }

// openCodeRedirect is the callback OpenCode is told to use with a shared
// client: its default path (/mcp/oauth/callback) is not the loopback
// /callback form the other harnesses use and an identity provider registers.
const openCodeRedirect = "http://127.0.0.1:19876/callback"

func (openCode) dir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		h, err := home()
		if err != nil {
			return "", err
		}
		base = filepath.Join(h, ".config")
	}
	return filepath.Join(base, "opencode"), nil
}

// file is the global configuration that holds (or will hold) the entry:
// whichever of opencode.jsonc and opencode.json already has it, else the
// one that exists, else a new opencode.json.
func (o openCode) file() (string, error) {
	dir, err := o.dir()
	if err != nil {
		return "", err
	}
	candidates := []string{filepath.Join(dir, "opencode.jsonc"), filepath.Join(dir, "opencode.json")}
	for _, f := range candidates {
		if entry, err := readJSONEntry(f, "mcp", ServerName); err == nil && entry != nil {
			return f, nil
		}
	}
	for _, f := range candidates {
		if _, err := os.Stat(f); err == nil {
			return f, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return candidates[1], nil
}

func (o openCode) SkillsDir() (string, error) { return agentsSkillsDir() }

func (openCode) want(e Entry) map[string]any {
	entry := map[string]any{"type": "remote", "url": e.URL}
	if auth := e.authorization(); auth != "" {
		entry["headers"] = map[string]any{"Authorization": auth}
	}
	if e.ClientID != "" {
		entry["oauth"] = map[string]any{"clientId": e.ClientID, "redirectUri": openCodeRedirect}
	}
	return entry
}

func (o openCode) Configure(_ context.Context, _ Env, e Entry) (Change, error) {
	file, err := o.file()
	if err != nil {
		return Change{}, err
	}
	change, err := patchEntry(file, o.want(e), e.Token != "", "mcp", ServerName)
	if e.OAuth {
		change.Next = "opencode mcp auth " + ServerName + " to sign OpenCode in"
	}
	return change, err
}

func (o openCode) Check(_ context.Context, e Entry) error {
	file, err := o.file()
	if err != nil {
		return err
	}
	return checkEntry(file, o.want(e), "mcp", ServerName)
}

// patchEntry makes the entry at path in file equal want.
func patchEntry(file string, want any, secret bool, path ...string) (Change, error) {
	existing, err := readJSONEntry(file, path...)
	if err != nil {
		return Change{}, err
	}
	change := Change{Action: "added", Where: file}
	if existing != nil {
		if equalJSON(existing, want) {
			change.Action = "unchanged"
			return change, nil
		}
		change.Action = "updated"
	}
	return change, writeJSONEntry(file, want, secret, path...)
}

func checkEntry(file string, want any, path ...string) error {
	existing, err := readJSONEntry(file, path...)
	if err != nil {
		return err
	}
	if !equalJSON(existing, want) {
		return mismatch(file, existing)
	}
	return nil
}
