package clientcmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/diderot/internal/client"
	"github.com/bluesky-social/diderot/internal/harness"
)

type setupReport struct {
	URL       string          `json:"url"`
	Auth      string          `json:"auth"`
	Account   string          `json:"account,omitempty"`
	Config    string          `json:"config"`
	SignIn    string          `json:"sign_in"` // new, or kept
	Harnesses []harnessReport `json:"harnesses"`
	Skills    []harnessReport `json:"skills"`
	// Sessions is what setup did about shipping sessions; absent when it
	// was neither asked nor had anyone to ask.
	Sessions *sessionsSetupReport `json:"sessions,omitempty"`
}

type harnessReport struct {
	Name string `json:"name,omitempty"`
	harness.Change
	Error string `json:"error,omitempty"`
}

func setupCmd(opts Options) *cobra.Command {
	var jsonOut, tokenStdin, sessions bool
	var harnesses, clientID string
	cmd := &cobra.Command{
		Use:   "setup <server URL>",
		Short: "Sign in to a Diderot server and connect your coding agents to it",
		Long: `Signs in to a Diderot server, or a gateway in front of one, registers its MCP
server with each harness you name, and installs the diderot agent skill for
them. The URL is the MCP endpoint; a URL without a path gets /mcp.

How to sign in is discovered from the URL: a server that takes an API token
asks for it (on a terminal; otherwise pass it on stdin with --token-stdin),
and one behind OAuth opens your browser. Run setup again at any time: entries
that already match are left alone.

Setup can also ship the sessions your coding agents keep on this machine to
the server, which summarizes them and makes them searchable. On a terminal
it shows the session directories it found and asks; off one, pass --sessions
to do it or nothing to leave it. ` + sessionsNotice + `
Setup ships from every session directory it finds; to leave a project out,
answer no and run diderot sessions install --exclude <directory> instead.

Harnesses: ` + strings.Join(harness.Names(), ", ") + `. Use --harness none to only sign in.`,
		Example: `  diderot setup https://diderot.example.com --harness claude-code,codex
  printf %s "$TOKEN" | diderot setup https://diderot.example.com --harness cursor --token-stdin`,
		GroupID: GroupID,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError("setup takes one argument, the server URL")
			}
			return nil
		},
	}
	cmd.RunE = clientRunE(opts, &jsonOut, func(cmd *cobra.Command, args []string, out *cliIO) error {
		return runSetup(cmd.Context(), out, setupOptions{
			url: args[0], harnesses: harnesses, harnessesSet: cmd.Flags().Changed("harness"),
			clientID: strings.TrimSpace(clientID), tokenStdin: tokenStdin,
			sessions: sessions, sessionsSet: cmd.Flags().Changed("sessions"),
		})
	})
	cmd.Flags().StringVar(&harnesses, "harness", "", "comma-separated harnesses to configure ("+strings.Join(harness.Names(), ", ")+", or none)")
	cmd.Flags().StringVar(&clientID, "client-id", "", "OAuth client ID from your administrator, for a sign-in server that does not register clients itself")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the API token from stdin")
	cmd.Flags().BoolVar(&sessions, "sessions", false, "ship this machine's coding-agent sessions to the server from a background service (--sessions=false declines without asking)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the result as JSON (the default when stdout is not a terminal)")
	return cmd
}

type setupOptions struct {
	url          string
	harnesses    string
	harnessesSet bool
	clientID     string
	tokenStdin   bool
	sessions     bool
	sessionsSet  bool
}

func runSetup(ctx context.Context, out *cliIO, opts setupOptions) error {
	mcpURL, err := client.NormalizeURL(opts.url)
	if err != nil {
		return usageError("%v", err)
	}
	stdin := bufio.NewReader(out.stdin)
	chosen, err := chooseHarnesses(out, stdin, opts)
	if err != nil {
		return err
	}
	path, err := client.ConfigPath()
	if err != nil {
		return err
	}
	previous, err := client.LoadConfig(path)
	if err != nil && !errors.Is(err, client.ErrNoConfig) {
		return err
	}
	if previous != nil && previous.URL != mcpURL {
		previous = nil
	}

	hc := client.NewHTTPClient()
	disc, err := client.Discover(ctx, hc, mcpURL)
	if err != nil {
		return err
	}
	cfg, signIn, err := signInFor(ctx, out, stdin, path, mcpURL, disc, previous, opts)
	if err != nil {
		return err
	}
	if previous != nil {
		for _, name := range previous.Harnesses {
			if !slices.Contains(chosen, name) {
				chosen = append(chosen, name)
			}
		}
	}
	slices.Sort(chosen)
	cfg.Harnesses = chosen
	if err := client.SaveConfig(path, cfg); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}

	report := setupReport{URL: mcpURL, Auth: cfg.Auth, Account: cfg.Account, Config: path, SignIn: signIn}
	entry := harness.Entry{URL: mcpURL, Token: cfg.Token, OAuth: cfg.Auth == client.AuthOAuth}
	if cfg.OAuth != nil {
		entry.ClientID = cfg.OAuth.HarnessClientID
	}
	env := harness.Env{Interactive: out.interactive, Output: out.stderr}
	failed := false
	var skillDirs []string
	for _, name := range chosen {
		h, ok := harness.Lookup(name)
		if !ok {
			failed = true
			report.Harnesses = append(report.Harnesses, harnessReport{Name: name, Error: fmt.Sprintf("unknown harness %q in %s", name, path)})
			continue
		}
		r := harnessReport{Name: name}
		change, err := h.Configure(ctx, env, entry)
		if err != nil {
			failed = true
			r.Error = err.Error()
		} else {
			r.Change = change
		}
		report.Harnesses = append(report.Harnesses, r)
		dir, err := h.SkillsDir()
		if err != nil {
			failed = true
			report.Skills = append(report.Skills, harnessReport{Name: name, Error: err.Error()})
			continue
		}
		if !slices.Contains(skillDirs, dir) {
			skillDirs = append(skillDirs, dir)
		}
	}
	for _, dir := range skillDirs {
		change, err := harness.InstallSkill(dir)
		r := harnessReport{Change: change}
		if err != nil {
			failed = true
			r.Error = err.Error()
			r.Where = dir
		}
		report.Skills = append(report.Skills, r)
	}
	report.Sessions = offerSessions(ctx, out, stdin, path, cfg, opts)
	if report.Sessions != nil && report.Sessions.Error != "" {
		failed = true
	}

	if out.json {
		if err := out.result(report); err != nil {
			return err
		}
	} else {
		printSetup(out, report)
	}
	if failed {
		return &exitError{code: exitPartial, err: errors.New("signed in, but not everything else is in place (see above)"), reported: true}
	}
	return nil
}

// signInFor returns the configuration to save for the server, signing in
// when the saved one does not work.
func signInFor(ctx context.Context, out *cliIO, stdin *bufio.Reader, path, mcpURL string, disc *client.Discovery, previous *client.Config, opts setupOptions) (*client.Config, string, error) {
	cfg := &client.Config{URL: mcpURL, Auth: disc.Auth}
	switch disc.Auth {
	case client.AuthNone:
		return cfg, "none needed", verify(ctx, out, path, cfg)
	case client.AuthToken:
		if opts.tokenStdin {
			token, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
			if err != nil {
				return nil, "", err
			}
			cfg.Token = strings.TrimSpace(string(token))
			if cfg.Token == "" {
				return nil, "", usageError("--token-stdin read no token from stdin")
			}
			return cfg, "new", verify(ctx, out, path, cfg)
		}
		if previous != nil && previous.Auth == client.AuthToken && verify(ctx, out, path, previous) == nil {
			cfg.Token = previous.Token
			return cfg, "kept", nil
		}
		if !out.interactive {
			return nil, "", &client.SignInError{Reason: mcpURL + " takes an API token; pass it on stdin with --token-stdin"}
		}
		token, err := promptSecret(out, "API token for "+hostOf(mcpURL)+": ")
		if err != nil {
			return nil, "", err
		}
		cfg.Token = token
		return cfg, "new", verify(ctx, out, path, cfg)
	case client.AuthOAuth:
		if opts.tokenStdin {
			return nil, "", usageError("%s signs in with OAuth, not an API token; drop --token-stdin", mcpURL)
		}
		if previous != nil && previous.Auth == client.AuthOAuth && previous.OAuth != nil &&
			(opts.clientID == "" || opts.clientID == previous.OAuth.ClientID) && verify(ctx, out, path, previous) == nil {
			return previous, "kept", nil
		}
		if !out.interactive {
			return nil, "", &client.SignInError{Reason: "signing in to " + mcpURL + " needs a browser; run the same diderot setup command in a terminal"}
		}
		oauth, account, err := client.SignInOAuth(ctx, client.SignInOptions{
			MCPURL: mcpURL, ClientID: opts.clientID, HTTP: client.NewHTTPClient(), Status: out.stderr,
			Open: out.openBrowser,
		})
		if err != nil {
			return nil, "", &client.SignInError{Reason: err.Error()}
		}
		cfg.OAuth, cfg.Account = oauth, account
		// Checked with the new access token as it is: a token near its expiry
		// would otherwise be refreshed through the saved file, which does not
		// hold this sign-in yet.
		check := &client.Config{URL: mcpURL, Auth: client.AuthToken, Token: oauth.Token.AccessToken}
		return cfg, "new", verify(ctx, out, path, check)
	}
	return nil, "", fmt.Errorf("unknown sign-in kind %q", disc.Auth)
}

// verify checks that cfg's credential lets it speak to the server and list
// its tools.
func verify(ctx context.Context, out *cliIO, path string, cfg *client.Config) error {
	m := out.mcp(path, cfg)
	if _, err := m.Discover(ctx); err != nil {
		return err
	}
	_, err := m.Tools(ctx)
	return err
}

func chooseHarnesses(out *cliIO, stdin *bufio.Reader, opts setupOptions) ([]string, error) {
	answer := opts.harnesses
	if !opts.harnessesSet {
		if !out.interactive {
			return nil, usageError("say which harnesses to configure with --harness (%s, or none)", strings.Join(harness.Names(), ", "))
		}
		var found []string
		out.progressf("Harnesses this can configure:\n")
		for _, h := range harness.All() {
			mark := ""
			if h.Detected() {
				mark = " (found)"
				found = append(found, h.Name())
			}
			out.progressf("  %s%s\n", h.Name(), mark)
		}
		out.progressf("Which should use Diderot? Comma-separated, none for none [%s]: ", strings.Join(found, ","))
		line, err := stdin.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		answer = strings.TrimSpace(line)
		if answer == "" {
			answer = strings.Join(found, ",")
		}
	}
	var chosen []string
	for _, name := range strings.Split(answer, ",") {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" || name == "none" {
			continue
		}
		if _, ok := harness.Lookup(name); !ok {
			return nil, usageError("unknown harness %q; choose from %s", name, strings.Join(harness.Names(), ", "))
		}
		if !slices.Contains(chosen, name) {
			chosen = append(chosen, name)
		}
	}
	return chosen, nil
}

func promptSecret(out *cliIO, prompt string) (string, error) {
	f, ok := out.stdin.(*os.File)
	if !ok {
		return "", errors.New("no terminal to read the token from")
	}
	out.progressf("%s", prompt)
	b, err := term.ReadPassword(int(f.Fd())) //nolint:gosec // a descriptor fits an int
	out.progressf("\n")
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", usageError("no token entered")
	}
	return token, nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return raw
}

func printSetup(out *cliIO, r setupReport) {
	who := ""
	if r.Account != "" {
		who = " as " + r.Account
	}
	switch r.SignIn {
	case "kept":
		out.printf("Signed in to %s%s (saved sign-in still works).\n", r.URL, who)
	case "none needed":
		out.printf("Connected to %s (it needs no sign-in).\n", r.URL)
	default:
		out.printf("Signed in to %s%s. Saved in %s.\n", r.URL, who, r.Config)
	}
	var next []string
	for _, h := range r.Harnesses {
		if h.Error != "" {
			out.printf("  %-12s failed: %s\n", h.Name, h.Error)
			continue
		}
		out.printf("  %-12s %s: %s\n", h.Name, h.Action, h.Where)
		if h.Next != "" {
			next = append(next, h.Next)
		}
	}
	for _, s := range r.Skills {
		if s.Error != "" {
			out.printf("  %-12s failed: %s: %s\n", "skill", s.Where, s.Error)
			continue
		}
		out.printf("  %-12s %s: %s\n", "skill", s.Action, s.Where)
	}
	if s := r.Sessions; s != nil {
		switch {
		case s.Error != "":
			out.printf("  %-12s failed: %s\n", "sessions", s.Error)
		case s.Install != nil:
			out.printf("  %-12s shipping in the background: %s\n", "sessions", s.Install.Service.File)
			out.printf("  %-12s %s\n", "", s.Install.Note)
		default:
			out.printf("  %-12s %s\n", "sessions", s.Action)
		}
	}
	if len(next) > 0 {
		out.printf("Next:\n")
		for _, n := range next {
			out.printf("  %s\n", n)
		}
	}
	out.printf("Check it any time with: diderot doctor\n")
}
