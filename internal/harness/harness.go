// Package harness registers a Diderot MCP server with coding-agent harnesses
// and installs the agent skill for them. Each harness is registered through
// its own CLI where it has one that can, and otherwise by patching its
// configuration file in place.
package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

// ServerName is the name the MCP server is registered under everywhere.
const ServerName = "diderot"

// TokenEnv is the environment variable a harness that takes a bearer only
// from the environment (Codex) reads the API token from.
const TokenEnv = "DIDEROT_TOKEN"

// Entry is what a harness should be told about the server.
type Entry struct {
	URL string
	// Token is the API token of a token server, empty otherwise.
	Token string
	// OAuth is set when the server signs in through OAuth; the harness then
	// signs itself in.
	OAuth bool
	// ClientID is the public client the harness signs in with, empty when the
	// authorization server registers each harness itself.
	ClientID string
}

func (e Entry) authorization() string {
	if e.Token == "" {
		return ""
	}
	return "Bearer " + e.Token
}

// Change says what Configure did.
type Change struct {
	Action string `json:"action,omitempty"` // added, updated, or unchanged
	Where  string `json:"where,omitempty"`  // the file or the command, with any token as <token>
	// Next is what the person still has to do, such as signing the harness in.
	Next string `json:"next,omitempty"`
}

// Env is how a harness reaches the machine.
type Env struct {
	// Interactive is set when a person is at a terminal; registration that
	// would open a browser runs only then.
	Interactive bool
	// Output receives what a harness CLI prints.
	Output io.Writer
}

// ErrNeedsTerminal is a registration that would start a browser sign-in,
// refused because no one is at a terminal.
var ErrNeedsTerminal = errors.New("registering would start this harness's browser sign-in; run setup again in a terminal")

// Harness is one coding-agent harness.
type Harness interface {
	Name() string
	// Detected reports whether the harness looks installed here.
	Detected() bool
	// Configure makes the harness's diderot entry match e.
	Configure(ctx context.Context, env Env, e Entry) (Change, error)
	// Check reports why the harness's diderot entry does not match e, or nil.
	Check(ctx context.Context, e Entry) error
	// SkillsDir is the directory the harness reads user skills from.
	SkillsDir() (string, error)
}

// All is every supported harness, in the order they are listed.
func All() []Harness {
	return []Harness{claudeCode{}, codex{}, cursor{}, openCode{}, pi{}}
}

// Names lists the supported harness names.
func Names() []string {
	var names []string
	for _, h := range All() {
		names = append(names, h.Name())
	}
	return names
}

// Lookup finds a harness by name.
func Lookup(name string) (Harness, bool) {
	for _, h := range All() {
		if h.Name() == strings.TrimSpace(strings.ToLower(name)) {
			return h, true
		}
	}
	return nil, false
}

func home() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	return h, nil
}

// agentsSkillsDir is ~/.agents/skills, the user skill directory of the Agent
// Skills convention that Codex, Cursor, OpenCode and Pi all read.
func agentsSkillsDir() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".agents", "skills"), nil
}

func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// run runs a harness CLI, its output going to env.Output and kept for the
// error. Its stdin is input when given, else the terminal when env is
// interactive, else nothing.
func run(ctx context.Context, env Env, input io.Reader, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var captured strings.Builder
	out := io.Writer(&captured)
	if env.Output != nil {
		out = io.MultiWriter(&captured, env.Output)
	}
	cmd.Stdout, cmd.Stderr = out, out
	switch {
	case input != nil:
		cmd.Stdin = input
	case env.Interactive:
		cmd.Stdin = os.Stdin
	}
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(captured.String())
		if detail != "" {
			return fmt.Errorf("%s: %w: %s", name, err, lastLine(detail))
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// shown is a command line for people to read, with the token hidden.
func shown(e Entry, name string, args ...string) string {
	parts := append([]string{name}, args...)
	line := strings.Join(parts, " ")
	if e.Token != "" {
		line = strings.ReplaceAll(line, e.Token, "<token>")
	}
	return line
}

// equalJSON compares two decoded JSON values, treating numbers alike.
func equalJSON(a, b any) bool {
	return reflect.DeepEqual(normalize(a), normalize(b))
}

func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalize(val)
		}
		return out
	case int:
		return float64(t)
	case int64:
		return float64(t)
	default:
		return v
	}
}

// mismatch describes how an entry differs, for doctor.
func mismatch(where string, existing any) error {
	if existing == nil {
		return fmt.Errorf("no %s entry in %s; run diderot setup", ServerName, where)
	}
	return fmt.Errorf("the %s entry in %s differs from what setup configures; run diderot setup", ServerName, where)
}
