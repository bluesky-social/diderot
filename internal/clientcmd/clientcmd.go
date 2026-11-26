// Package clientcmd is the diderot client commands: setup, doctor, and ask,
// which reach a Diderot server over MCP alone, and sessions, which pushes
// this machine's coding-agent sessions to the server's session route. Both binaries mount them: the
// server's, beside serve and its maintenance commands, and the client-only
// binary the releases carry, which must stay free of the server's packages
// (the root package's test checks).
package clientcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/diderot/internal/client"
)

// Exit codes of the client commands. Agents branch on them.
const (
	exitFailure   = 1 // a failed check, a failed Ask, anything else
	exitUsage     = 2 // bad flags or arguments
	exitSignIn    = 3 // sign-in needed, expired, or refused
	exitPartial   = 4 // setup signed in, but a harness or skill failed
	exitCancelled = 5 // interrupted
)

// exitError carries a client command's exit code to main. reported is set
// when the command's JSON result already says what went wrong, so no second
// JSON document follows it on stdout.
type exitError struct {
	code     int
	err      error
	reported bool
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageError(format string, args ...any) error {
	return &exitError{code: exitUsage, err: fmt.Errorf(format, args...)}
}

// Options adapt the client commands to the program that mounts them.
type Options struct {
	// Version is the program's version, which the client gives servers.
	Version string
	// OpenBrowser shows the sign-in page to the person; nil opens the system
	// browser.
	OpenBrowser func(url string) error
	// Interactive reports whether a person is at stdin; nil checks for a
	// terminal.
	Interactive func(stdin io.Reader) bool
}

// ResolveVersion is the version a binary reports: stamped, the release's tag
// set at link time, or else the module version the go command recorded,
// which `go install ...@vX.Y.Z` sets, or else "dev".
func ResolveVersion(stamped string) string {
	if stamped != "" && stamped != "dev" {
		return stamped
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// GroupID is the help group the client commands are listed under.
const GroupID = "client"

// ServerCommands are the server binary's other commands, which the
// client-only binary names to say where they are. cmd/diderot's test keeps
// the list in step with that binary.
var ServerCommands = []string{"serve", "probe", "qdrant-audit", "reset-admin", "codex-provider", "ui-dev", "session-dev"}

// AddCommands adds the client commands to root, in their own help group,
// with flag errors as usage errors (exit 2).
func AddCommands(root *cobra.Command, opts Options) {
	root.AddGroup(&cobra.Group{ID: GroupID, Title: "Use a Diderot server:"})
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &exitError{code: exitUsage, err: err}
	})
	root.AddCommand(setupCmd(opts), doctorCmd(opts), askCmd(opts), sessionsCmd(opts))
}

// ExitCode is the process exit code for an error Execute returned: 0 for
// nil, else one of the codes the client commands document.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return exitCode(err)
}

// exitCode is the process exit code for err.
func exitCode(err error) int {
	var coded *exitError
	var signIn *client.SignInError
	switch {
	case errors.As(err, &coded):
		return coded.code
	case errors.As(err, &signIn):
		return exitSignIn
	case errors.Is(err, context.Canceled):
		return exitCancelled
	default:
		return exitFailure
	}
}

// io is one client command's view of the terminal: JSON results on stdout
// when stdout is not a terminal or --json is set, people's text otherwise,
// and progress and prompts on stderr.
type cliIO struct {
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	json        bool
	interactive bool // a person is at stdin
	version     string
	openBrowser func(url string) error
}

func newCLIIO(opts Options, cmd *cobra.Command, jsonFlag bool) *cliIO {
	c := &cliIO{
		stdin: cmd.InOrStdin(), stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr(), json: jsonFlag,
		version: opts.Version, openBrowser: opts.OpenBrowser,
	}
	if f, ok := c.stdout.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) { //nolint:gosec // a descriptor fits an int
		c.json = true
	}
	if opts.Interactive != nil {
		c.interactive = opts.Interactive(c.stdin)
	} else if f, ok := c.stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) { //nolint:gosec // as above
		c.interactive = true
	}
	return c
}

func (c *cliIO) result(v any) error {
	enc := json.NewEncoder(c.stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (c *cliIO) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.stdout, format, args...)
}

func (c *cliIO) progressf(format string, args ...any) {
	_, _ = fmt.Fprintf(c.stderr, format, args...)
}

// clientRunE runs a client command with its view of the terminal.
func clientRunE(opts Options, jsonFlag *bool, run func(cmd *cobra.Command, args []string, out *cliIO) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return run(cmd, args, newCLIIO(opts, cmd, *jsonFlag))
	}
}

// Execute runs the command line. When a client command fails in JSON mode,
// its error is also a JSON object on stdout, so a caller parsing stdout sees
// it; argument and flag errors arise before the command runs, so this is
// done here rather than in the command.
func Execute(ctx context.Context, root *cobra.Command) error {
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil || cmd == nil || (cmd.GroupID != GroupID && (cmd.Parent() == nil || cmd.Parent().GroupID != GroupID)) {
		return err
	}
	var coded *exitError
	if errors.As(err, &coded) && coded.reported {
		return err
	}
	jsonFlag, _ := cmd.Flags().GetBool("json")
	if out := newCLIIO(Options{}, cmd, jsonFlag); out.json {
		_ = out.result(map[string]any{"error": err.Error(), "exit_code": exitCode(err)})
	}
	return err
}

// loadClientConfig reads the saved configuration, or says to run setup.
func loadClientConfig() (string, *client.Config, error) {
	path, err := client.ConfigPath()
	if err != nil {
		return "", nil, err
	}
	cfg, err := client.LoadConfig(path)
	if errors.Is(err, client.ErrNoConfig) {
		return path, nil, &client.SignInError{Reason: "diderot is not set up here; run diderot setup <server URL>"}
	}
	return path, cfg, err
}

// mcp is an MCP client signed in as cfg says.
func (c *cliIO) mcp(path string, cfg *client.Config) *client.MCP {
	return &client.MCP{
		URL: cfg.URL, HTTP: client.NewHTTPClient(), Version: c.version,
		Authorization: func(ctx context.Context) (string, error) {
			return client.Authorization(ctx, path, cfg)
		},
	}
}
