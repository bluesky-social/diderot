package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// codex registers through `codex mcp add`, which replaces an entry of the
// same name, and reads it back from $CODEX_HOME/config.toml: `codex mcp get
// --json` leaves out the OAuth client. Codex takes a bearer token only from
// an environment variable.
type codex struct{}

func (codex) Name() string   { return "codex" }
func (codex) Detected() bool { return onPath("codex") }

func (codex) home() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir, nil
	}
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".codex"), nil
}

func (codex) SkillsDir() (string, error) { return agentsSkillsDir() }

// codexEntry is the part of an mcp_servers entry setup decides.
type codexEntry struct {
	URL               string `toml:"url"`
	Command           string `toml:"command"`
	BearerTokenEnvVar string `toml:"bearer_token_env_var"`
	OAuth             struct {
		ClientID string `toml:"client_id"`
	} `toml:"oauth"`
}

func (c codex) existing() (string, *codexEntry, error) {
	dir, err := c.home()
	if err != nil {
		return "", nil, err
	}
	file := filepath.Join(dir, "config.toml")
	data, err := os.ReadFile(file) //nolint:gosec // Codex's own configuration
	if errors.Is(err, fs.ErrNotExist) {
		return file, nil, nil
	}
	if err != nil {
		return file, nil, err
	}
	var doc struct {
		MCPServers map[string]*codexEntry `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return file, nil, fmt.Errorf("read %s: %w", file, err)
	}
	return file, doc.MCPServers[ServerName], nil
}

func matches(existing *codexEntry, e Entry) bool {
	if existing == nil || existing.Command != "" || existing.URL != e.URL || existing.OAuth.ClientID != e.ClientID {
		return false
	}
	wantEnv := ""
	if e.Token != "" {
		wantEnv = TokenEnv
	}
	return existing.BearerTokenEnvVar == wantEnv
}

func (c codex) args(e Entry) []string {
	args := []string{"mcp", "add", ServerName, "--url", e.URL}
	if e.Token != "" {
		args = append(args, "--bearer-token-env-var", TokenEnv)
	}
	if e.ClientID != "" {
		args = append(args, "--oauth-client-id", e.ClientID)
	}
	return args
}

func (c codex) Configure(ctx context.Context, env Env, e Entry) (Change, error) {
	_, existing, err := c.existing()
	if err != nil {
		return Change{}, err
	}
	args := c.args(e)
	change := Change{Action: "added", Where: shown(e, "codex", args...)}
	switch {
	case e.Token != "":
		change.Next = "export " + TokenEnv + "=<your API token> where Codex runs: Codex reads the token only from the environment"
	case e.OAuth:
		change.Next = "codex mcp login " + ServerName + " to sign Codex in again later"
	}
	if matches(existing, e) {
		change.Action = "unchanged"
		return change, nil
	}
	if existing != nil {
		change.Action = "updated"
	}
	// codex mcp add starts its own browser sign-in when it finds OAuth.
	if e.OAuth && !env.Interactive {
		return Change{}, ErrNeedsTerminal
	}
	return change, run(ctx, env, nil, "codex", args...)
}

func (c codex) Check(_ context.Context, e Entry) error {
	file, existing, err := c.existing()
	if err != nil {
		return err
	}
	if !matches(existing, e) {
		if existing == nil {
			return mismatch(file, nil)
		}
		return mismatch(file, existing)
	}
	if e.Token != "" && os.Getenv(TokenEnv) == "" {
		return fmt.Errorf("%s is not set in this shell; Codex reads the API token from it", TokenEnv)
	}
	return nil
}
