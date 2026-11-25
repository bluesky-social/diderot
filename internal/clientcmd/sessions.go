package clientcmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/diderot/internal/client"
	"github.com/bluesky-social/diderot/internal/sessionship"
)

// sessionsNotice is the one sentence that says what shipping sessions sends
// and who can read it. Setup asks with it and the help carries it.
const sessionsNotice = "A background service converts each session on this machine and sends its full transcript to that server, " +
	"where everyone who can sign in to it can search and read it unless you choose Only me for it on the server's /sessions page."

// sessionsServerNeeds says what the server needs before a shipped session
// is searchable and the client has no way to check.
func sessionsServerNeeds(cfg *client.Config) string {
	models, _ := sessionship.ServerURL(cfg.URL, "/admin/settings/models")
	return "Not checked from here: the server summarizes sessions only once an administrator has chosen a model for both " +
		"session-summary jobs on its Models page (" + models + "); the model has to pass each job's test, " +
		"which requires it to answer a forced tool call. " +
		"Until then sessions are stored but not searchable; diderot sessions status shows what the server has indexed."
}

func sessionsCmd(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Ship this machine's coding-agent sessions to the Diderot server",
		Long: `Sends the sessions your coding agents keep on this machine (` + strings.Join(sessionship.Names(), ", ") + `) to
the Diderot server setup saved, which summarizes them and makes them
searchable. Each session is converted on this machine to ATIF, an open
trajectory format the server already reads, and sent whole: nothing is
redacted or left out, and the session files are never changed.

` + sessionsNotice + `

diderot sessions install runs this in the background; diderot setup offers
to do it.`,
		GroupID: GroupID,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError("unknown sessions command %q; see diderot sessions --help", args[0])
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(sessionsSyncCmd(opts), sessionsRunCmd(opts), sessionsInstallCmd(opts), sessionsUninstallCmd(opts), sessionsStatusCmd(opts))
	return cmd
}

func noArgs(name string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return usageError("sessions %s takes no arguments", name)
		}
		return nil
	}
}

// sessionSettings is what to ship: what install saved, or else every
// harness's session directory found here, for the server setup saved.
func sessionSettings(paths sessionship.Paths, cfg *client.Config) (sessionship.Settings, error) {
	settings, ok, err := sessionship.LoadSettings(paths.Settings)
	if err != nil || ok {
		return settings, err
	}
	if cfg != nil {
		settings.Server = cfg.URL
	}
	settings.Sources, err = sessionship.Detect()
	return settings, err
}

// errOtherServer says the installed shipping was agreed for another server
// than the one setup has saved since. Agreeing to send transcripts to one
// server is not agreeing to send them to the next.
func errOtherServer(settings sessionship.Settings, cfg *client.Config) error {
	return fmt.Errorf("session shipping was installed for %s, and setup has since saved %s: nothing is sent; "+
		"run diderot sessions install to ship to the new server, or diderot sessions uninstall", settings.Server, cfg.URL)
}

func sessionPusher(path string, cfg *client.Config) *sessionship.Pusher {
	return &sessionship.Pusher{
		MCPURL: cfg.URL, HTTP: client.NewHTTPClient(),
		Authorization: func(ctx context.Context) (string, error) { return client.Authorization(ctx, path, cfg) },
	}
}

// sessionPass makes one pass with the configuration and settings as they
// are saved now, so the background service follows a later install without
// a restart. installed is the background service's pass, which ships only
// what install saved: with no saved settings it sends nothing, rather than
// every directory it can find.
func sessionPass(ctx context.Context, quiet time.Duration, installed bool) (sessionship.Result, error) {
	path, cfg, err := loadClientConfig()
	if err != nil {
		return sessionship.Result{}, err
	}
	paths, err := sessionship.DefaultPaths()
	if err != nil {
		return sessionship.Result{}, err
	}
	settings, err := sessionSettings(paths, cfg)
	if err != nil {
		return sessionship.Result{}, err
	}
	if installed {
		if _, saved, err := sessionship.LoadSettings(paths.Settings); err != nil || !saved {
			return sessionship.Result{}, errors.Join(err, fmt.Errorf("%s is missing, so nothing is sent; run diderot sessions install", paths.Settings))
		}
	}
	if settings.Server != cfg.URL {
		return sessionship.Result{}, errOtherServer(settings, cfg)
	}
	syncer := &sessionship.Syncer{
		Settings: settings, StatePath: paths.State, ServerURL: cfg.URL,
		Put: sessionPusher(path, cfg).Put, Quiet: quiet, Now: time.Now,
	}
	return syncer.Pass(ctx)
}

func sessionsSyncCmd(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Send every new or changed session now, then exit",
		Long: `Makes one pass over the session directories and sends each session that is
new or has changed since it was last sent, including one still in progress.
It prints what it sent. A session the server refuses is reported as skipped
and tried again when its file changes.`,
		Args: noArgs("sync"),
	}
	cmd.RunE = clientRunE(opts, &jsonOut, func(cmd *cobra.Command, _ []string, out *cliIO) error {
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		result, err := sessionPass(ctx, 0, false)
		if errors.Is(err, sessionship.ErrBusy) {
			return errors.New("the background service is sending sessions right now; try again in a moment")
		}
		if out.json {
			if encodeErr := out.result(struct {
				sessionship.Result
				Error string `json:"error,omitempty"`
			}{result, errorText(err)}); encodeErr != nil {
				return encodeErr
			}
		} else {
			for _, o := range result.Sent {
				out.printf("%-8s %-12s %s\n", o.Action, o.Harness, o.SessionID)
			}
			for _, o := range result.Skipped {
				out.printf("%-8s %-12s %s: %s\n", o.Action, o.Harness, o.Path, o.Reason)
			}
			for _, u := range result.Unlisted {
				out.printf("%-8s %-12s %s: %s\n", "unlisted", u.Harness, u.Dir, u.Error)
			}
			out.printf("Sent %d sessions; %d unchanged, %d skipped.\n", len(result.Sent), result.Unchanged, len(result.Skipped))
		}
		if err != nil {
			return &exitError{code: exitCode(err), err: err, reported: out.json}
		}
		return nil
	})
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the result as JSON (the default when stdout is not a terminal)")
	return cmd
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func sessionsRunCmd(_ Options) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Keep sending sessions until stopped (what the background service runs)",
		Long: fmt.Sprintf(`Makes a pass every %s and sends each session that changed and has not been
written to for %s, so a session in progress is sent when its agent pauses.
When the server cannot be reached it waits longer between tries, up to half
an hour, and carries on when the server is back. It logs to stderr and stops
on SIGINT or SIGTERM.`, sessionship.PollInterval, sessionship.QuietPeriod),
		Args: noArgs("run"),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			logf := func(format string, args ...any) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), time.Now().Format(time.RFC3339)+" "+format+"\n", args...)
			}
			logf("started; a pass every %s", sessionship.PollInterval)
			failing := ""
			sessionship.Run(ctx,
				func(ctx context.Context) (sessionship.Result, error) {
					return sessionPass(ctx, sessionship.QuietPeriod, true)
				},
				func(result sessionship.Result, err error, wait time.Duration) {
					for _, o := range result.Sent {
						logf("%s %s %s", o.Action, o.Harness, o.SessionID)
					}
					for _, o := range result.Skipped {
						logf("skipped %s %s: %s", o.Harness, o.Path, o.Reason)
					}
					for _, u := range result.Unlisted {
						logf("could not list %s %s: %s", u.Harness, u.Dir, u.Error)
					}
					switch {
					case err != nil && !errors.Is(err, sessionship.ErrBusy):
						failing = err.Error()
						logf("failed: %s; next try in %s", failing, wait)
					case err == nil && failing != "":
						failing = ""
						logf("recovered")
					}
				}, sessionship.Sleep)
			logf("stopped")
			return nil
		},
	}
}

// serviceBinary is the absolute path the background service runs: this
// program, by the name it has on PATH when that is the same file, so that a
// package manager's stable link outlives an upgrade.
func serviceBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	onPath, err := exec.LookPath(filepath.Base(exe))
	if err != nil {
		return exe, nil
	}
	if onPath, err = filepath.Abs(onPath); err != nil {
		return exe, nil
	}
	a, errA := os.Stat(onPath)
	b, errB := os.Stat(exe)
	if errA == nil && errB == nil && os.SameFile(a, b) {
		return onPath, nil
	}
	return exe, nil
}

// serviceEnv carries the directory overrides this process runs under to the
// service, which starts with the service manager's environment.
func serviceEnv() map[string]string {
	env := map[string]string{}
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME"} {
		if v := os.Getenv(key); filepath.IsAbs(v) {
			env[key] = v
		}
	}
	return env
}

type sessionsInstallReport struct {
	Service  sessionship.ServiceStatus `json:"service"`
	Sources  []sessionship.Source      `json:"sources"`
	Exclude  []string                  `json:"exclude,omitempty"`
	Settings string                    `json:"settings"`
	State    string                    `json:"state"`
	Log      string                    `json:"log"`
	// Note is what the server still needs that install could not check.
	Note string `json:"note"`
}

// installSessions checks that the server takes this account's sessions,
// saves what to ship, and installs and starts the background service.
func installSessions(ctx context.Context, cfgPath string, cfg *client.Config, settings sessionship.Settings) (*sessionsInstallReport, error) {
	if len(settings.Sources) == 0 {
		return nil, fmt.Errorf("no session directories found for %s; name one with diderot sessions install --path <harness>=<directory>",
			strings.Join(sessionship.Names(), ", "))
	}
	paths, err := sessionship.DefaultPaths()
	if err != nil {
		return nil, err
	}
	manager, err := sessionship.NewManager(runtime.GOOS)
	if err != nil {
		return nil, err
	}
	binary, err := serviceBinary()
	if err != nil {
		return nil, err
	}
	if err := sessionPusher(cfgPath, cfg).Check(ctx); err != nil {
		return nil, err
	}
	settings.Server = cfg.URL
	_, had, err := sessionship.LoadSettings(paths.Settings)
	if err != nil {
		return nil, err
	}
	if err := sessionship.SaveSettings(paths.Settings, settings); err != nil {
		return nil, err
	}
	if err := manager.Install(ctx, sessionship.Unit{Binary: binary, Log: paths.Log, Env: serviceEnv()}); err != nil {
		// Install may have loaded the service before it failed. Take it out
		// before the settings, so nothing is left running; a service that
		// cannot be removed keeps its settings, which hold what to leave out.
		if uninstallErr := manager.Uninstall(ctx); uninstallErr != nil {
			return nil, errors.Join(err, uninstallErr)
		}
		if !had {
			_ = os.Remove(paths.Settings)
		}
		return nil, err
	}
	return &sessionsInstallReport{
		Service: manager.Status(ctx), Sources: settings.Sources, Exclude: settings.Exclude,
		Settings: paths.Settings, State: paths.State, Log: paths.Log, Note: sessionsServerNeeds(cfg),
	}, nil
}

func printSessionsInstall(out *cliIO, r *sessionsInstallReport) {
	out.printf("Shipping sessions in the background: %s\n", r.Service.File)
	for _, s := range r.Sources {
		out.printf("  %-12s %s\n", s.Harness, s.Dir)
	}
	out.printf("%s\n", r.Note)
}

func sessionsInstallCmd(opts Options) *cobra.Command {
	var jsonOut bool
	var pathFlags, excludeFlags []string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the background service that ships sessions",
		Long: `Installs a per-user background service that runs diderot sessions run: a
launchd agent in ~/Library/LaunchAgents on macOS, a systemd user unit in
~/.config/systemd/user on Linux. It needs no administrator rights, starts
now and at every login, and restarts if it fails. The sign-in stays in the
file setup saved; the service definition holds no credential.

Without --path it ships from the directories a previous install saved, or
else from every harness's session directory found here. Running it again
replaces the service and restarts it.

` + sessionsNotice,
		Example: `  diderot sessions install
  diderot sessions install --path claude-code=$HOME/.claude/projects --exclude $HOME/.claude/projects/-Users-me-private`,
		Args: noArgs("install"),
	}
	cmd.RunE = clientRunE(opts, &jsonOut, func(cmd *cobra.Command, _ []string, out *cliIO) error {
		cfgPath, cfg, err := loadClientConfig()
		if err != nil {
			return err
		}
		paths, err := sessionship.DefaultPaths()
		if err != nil {
			return err
		}
		settings, err := sessionSettings(paths, cfg)
		if err != nil {
			return err
		}
		if len(pathFlags) > 0 {
			settings.Sources = nil
		}
		for _, flag := range pathFlags {
			name, dir, ok := strings.Cut(flag, "=")
			if _, known := sessionship.Lookup(name); !ok || !known || dir == "" {
				return usageError("--path %q: want <harness>=<directory>, with a harness from %s", flag, strings.Join(sessionship.Names(), ", "))
			}
			if dir, err = filepath.Abs(dir); err != nil {
				return err
			}
			settings.Sources = append(settings.Sources, sessionship.Source{Harness: name, Dir: dir})
		}
		if cmd.Flags().Changed("exclude") {
			settings.Exclude = nil
		}
		for _, dir := range excludeFlags {
			if dir, err = filepath.Abs(dir); err != nil {
				return err
			}
			settings.Exclude = append(settings.Exclude, dir)
		}
		report, err := installSessions(cmd.Context(), cfgPath, cfg, settings)
		if err != nil {
			return err
		}
		if out.json {
			return out.result(report)
		}
		printSessionsInstall(out, report)
		return nil
	})
	cmd.Flags().StringArrayVar(&pathFlags, "path", nil, "ship from this directory, as <harness>=<directory> (repeatable; harnesses: "+strings.Join(sessionship.Names(), ", ")+")")
	cmd.Flags().StringArrayVar(&excludeFlags, "exclude", nil, "leave out every session under this directory (repeatable)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the result as JSON (the default when stdout is not a terminal)")
	return cmd
}

func sessionsUninstallCmd(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the background service",
		Long: `Stops the background service and removes what install created: the service
definition, the saved list of directories, and the service's log. The record
of what was sent is kept, so installing again does not send every session a
second time; the command prints where it is. Sessions already on the server
stay there.`,
		Args: noArgs("uninstall"),
	}
	cmd.RunE = clientRunE(opts, &jsonOut, func(cmd *cobra.Command, _ []string, out *cliIO) error {
		paths, err := sessionship.DefaultPaths()
		if err != nil {
			return err
		}
		manager, err := sessionship.NewManager(runtime.GOOS)
		if err != nil {
			return err
		}
		if err := manager.Uninstall(cmd.Context()); err != nil {
			return err
		}
		removed, err := removeInstalledFiles(paths)
		if err != nil {
			return err
		}
		if out.json {
			return out.result(map[string]any{"removed": append([]string{manager.File()}, removed...), "kept": paths.State})
		}
		out.printf("Removed the background service (%s).\nKept the record of what was sent: %s\n", manager.File(), paths.State)
		return nil
	})
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the result as JSON (the default when stdout is not a terminal)")
	return cmd
}

// removeInstalledFiles removes what install and the service wrote beside the
// service definition: the settings and the log. The state file stays, and
// so does its lock file: a sync still running holds that lock, and a pass
// started after it was removed would lock a new file and write the same
// state.
func removeInstalledFiles(paths sessionship.Paths) ([]string, error) {
	removed := []string{paths.Settings, paths.Log}
	for _, file := range removed {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return removed, nil
}

type sessionsStatus struct {
	Service     sessionship.ServiceStatus `json:"service"`
	URL         string                    `json:"url,omitempty"`
	Sources     []sessionship.Source      `json:"sources"`
	Exclude     []string                  `json:"exclude,omitempty"`
	Sent        int                       `json:"sent"`
	Skipped     []sessionsSkipped         `json:"skipped"`
	LastPass    *time.Time                `json:"last_pass,omitempty"`
	LastSuccess *time.Time                `json:"last_success,omitempty"`
	// LastError is set while the most recent pass failed.
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	// Unlisted is each source the most recent pass could not list.
	Unlisted []sessionship.SourceError `json:"unlisted,omitempty"`
	// OnServer is what the server has done with this account's sessions,
	// asked for once something was sent; OnServerError is why it could not
	// be asked.
	OnServer      *sessionship.Indexed `json:"on_server,omitempty"`
	OnServerError string               `json:"on_server_error,omitempty"`
	// Models is the server page where the session-summary models are chosen.
	Models string `json:"models,omitempty"`
	State  string `json:"state"`
	Log    string `json:"log"`
}

// onServer is the status line about what the server indexed.
func (s *sessionsStatus) onServer() string {
	switch {
	case s.OnServerError != "":
		return "could not ask: " + s.OnServerError
	case s.OnServer == nil:
		return ""
	}
	line := fmt.Sprintf("%d indexed, %d waiting, %d could not be summarized (of your %d most recently changed sessions there)",
		s.OnServer.Indexed, s.OnServer.Waiting, s.OnServer.Failed, s.OnServer.Listed)
	if s.OnServer.Waiting > 0 && s.OnServer.Indexed == 0 {
		line += "; the server summarizes a session about ten minutes after it stops changing, and never if no model is chosen for both session-summary jobs: " + s.Models
	}
	return line
}

type sessionsSkipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func readSessionsStatus(ctx context.Context) (*sessionsStatus, error) {
	paths, err := sessionship.DefaultPaths()
	if err != nil {
		return nil, err
	}
	manager, err := sessionship.NewManager(runtime.GOOS)
	if err != nil {
		return nil, err
	}
	cfgPath, cfg, err := loadClientConfig()
	var signIn *client.SignInError
	if err != nil && !errors.As(err, &signIn) {
		return nil, err
	}
	settings, err := sessionSettings(paths, cfg)
	if err != nil {
		return nil, err
	}
	state, err := sessionship.LoadState(paths.State)
	if err != nil {
		return nil, err
	}
	// The state file is one server's. Until a pass for this server rewrites
	// it, there is nothing to report: not another server's counts and times.
	if state.URL != settings.Server {
		state = &sessionship.State{}
	}
	status := &sessionsStatus{
		Service: manager.Status(ctx), URL: settings.Server, Sources: settings.Sources, Exclude: settings.Exclude,
		Skipped: []sessionsSkipped{}, LastPass: timePtr(state.LastPass), LastSuccess: timePtr(state.LastSuccess),
		Unlisted: state.Unlisted, State: paths.State, Log: paths.Log,
	}
	status.Sent, _ = state.Counts()
	for path, f := range state.Files {
		if f.Skipped != "" {
			status.Skipped = append(status.Skipped, sessionsSkipped{Path: path, Reason: f.Skipped})
		}
	}
	sort.Slice(status.Skipped, func(i, j int) bool { return status.Skipped[i].Path < status.Skipped[j].Path })
	if state.LastErrorAt.After(state.LastSuccess) {
		status.LastError, status.LastErrorAt = state.LastError, timePtr(state.LastErrorAt)
	}
	// Said here as well as in the service's log: a pass the service refuses
	// for one of these reasons never reaches the state file.
	_, saved, err := sessionship.LoadSettings(paths.Settings)
	if err != nil {
		return nil, err
	}
	installed := status.Service.Installed
	switch {
	case installed && cfg == nil:
		status.LastError = "the saved sign-in is missing, so nothing is sent; run diderot setup <server URL>"
	case installed && !saved:
		status.LastError = paths.Settings + " is missing, so nothing is sent; run diderot sessions install"
	case cfg != nil && settings.Server != cfg.URL:
		status.LastError = errOtherServer(settings, cfg).Error()
	case cfg != nil && status.Sent > 0:
		// Pushing only stores a session; ask what the server did with them.
		status.Models, _ = sessionship.ServerURL(cfg.URL, "/admin/settings/models")
		askCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		indexed, err := sessionPusher(cfgPath, cfg).Indexed(askCtx)
		if err != nil {
			status.OnServerError = err.Error()
		} else {
			status.OnServer = &indexed
		}
	}
	return status, nil
}

func ago(t *time.Time) string {
	if t == nil {
		return "no pass yet for this server"
	}
	return fmt.Sprintf("%s (%s ago)", t.Local().Format(time.RFC3339), time.Since(*t).Round(time.Second))
}

func (s *sessionsStatus) service() string {
	switch {
	case s.Service.Running && s.Service.PID != 0:
		return fmt.Sprintf("running (pid %d)", s.Service.PID)
	case s.Service.Running:
		return "running"
	case s.Service.Installed:
		return "installed, not running"
	}
	return "not installed"
}

func sessionsStatusCmd(opts Options) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the background service and what has been sent",
		Long: `Shows whether the background service is installed and running, the
directories it ships from, how many sessions have been sent, what the server
has indexed of them, when a pass last succeeded, the error of the last pass
if it failed, and each source it could not list. It changes nothing. With --json it also lists each skipped session file and why.`,
		Args: noArgs("status"),
	}
	cmd.RunE = clientRunE(opts, &jsonOut, func(cmd *cobra.Command, _ []string, out *cliIO) error {
		status, err := readSessionsStatus(cmd.Context())
		if err != nil {
			return err
		}
		if out.json {
			return out.result(status)
		}
		out.printf("Service:      %s\n", status.service())
		if status.Service.Installed {
			out.printf("              %s\n", status.Service.File)
		}
		if status.URL != "" {
			out.printf("Server:       %s\n", status.URL)
		}
		label := "Ships from:"
		for _, s := range status.Sources {
			out.printf("%-13s %-12s %s\n", label, s.Harness, s.Dir)
			label = ""
		}
		for _, dir := range status.Exclude {
			out.printf("%-13s %s\n", "Leaves out:", dir)
		}
		out.printf("Sent:         %d sessions (%d skipped; --json lists them)\n", status.Sent, len(status.Skipped))
		if line := status.onServer(); line != "" {
			out.printf("On server:    %s\n", line)
		}
		out.printf("Last pass:    %s\n", ago(status.LastPass))
		out.printf("Last success: %s\n", ago(status.LastSuccess))
		if status.LastError != "" {
			out.printf("Last error:   %s\n", status.LastError)
		}
		for _, u := range status.Unlisted {
			out.printf("%-13s %-12s %s: %s\n", "Not listed:", u.Harness, u.Dir, u.Error)
		}
		out.printf("State:        %s\nLog:          %s\n", status.State, status.Log)
		return nil
	})
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the result as JSON (the default when stdout is not a terminal)")
	return cmd
}

// sessionsSetupReport is what setup did about shipping sessions.
type sessionsSetupReport struct {
	// Action is installed, already installed, declined (with what that
	// leaves, when a service for another server is installed), not offered
	// (with what the server lacks), or failed.
	Action  string                 `json:"action"`
	Install *sessionsInstallReport `json:"install,omitempty"`
	Error   string                 `json:"error,omitempty"`
}

// offerSessions is setup's last step: install the background service when
// the person says yes (--sessions, or the prompt on a terminal). It returns
// nil when there is nothing to say: no flag and nobody to ask, or nothing
// to ship.
func offerSessions(ctx context.Context, out *cliIO, stdin *bufio.Reader, cfgPath string, cfg *client.Config, opts setupOptions) *sessionsSetupReport {
	fail := func(err error) *sessionsSetupReport {
		return &sessionsSetupReport{Action: "failed", Error: err.Error()}
	}
	paths, err := sessionship.DefaultPaths()
	if err != nil {
		return fail(err)
	}
	manager, err := sessionship.NewManager(runtime.GOOS)
	if err != nil {
		return fail(err)
	}
	saved, _, err := sessionship.LoadSettings(paths.Settings)
	if err != nil {
		return fail(err)
	}
	installed := manager.Status(ctx).Installed
	if installed && saved.Server == cfg.URL {
		return &sessionsSetupReport{Action: "already installed"}
	}
	// Installed for another server: it sends nothing to this one unless the
	// person agrees again, here or with diderot sessions install.
	declined := &sessionsSetupReport{Action: "declined"}
	if installed {
		declined.Action = "declined; the service installed for " + saved.Server + " sends nothing now (diderot sessions uninstall removes it)"
	}
	sources, err := sessionship.Detect()
	if err != nil {
		return fail(err)
	}
	if len(saved.Sources) > 0 {
		sources = saved.Sources
	}
	if opts.sessionsSet && !opts.sessions {
		return declined
	}
	if !opts.sessionsSet {
		if !out.interactive || len(sources) == 0 {
			if installed {
				return declined
			}
			return nil
		}
		// Asked only of a server that would take the sessions: otherwise say
		// what it lacks, and install nothing.
		if err := sessionPusher(cfgPath, cfg).Check(ctx); err != nil {
			return &sessionsSetupReport{Action: "not offered: " + err.Error() + ". Then run diderot sessions install"}
		}
		out.progressf("Coding-agent sessions on this machine:\n")
		for _, s := range sources {
			n, _ := s.Count(saved.Exclude)
			out.progressf("  %-12s %s (%d sessions)\n", s.Harness, s.Dir, n)
		}
		out.progressf("%s\n", sessionsServerNeeds(cfg))
		out.progressf("Ship them to %s? %s [y/N]: ", hostOf(cfg.URL), sessionsNotice)
		line, err := stdin.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fail(err)
		}
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			return declined
		}
	}
	install, err := installSessions(ctx, cfgPath, cfg, sessionship.Settings{Sources: sources, Exclude: saved.Exclude})
	if err != nil {
		return fail(err)
	}
	return &sessionsSetupReport{Action: "installed", Install: install}
}

// sessionsCheck is doctor's line about the background service.
func sessionsCheck(ctx context.Context) (detail string, err error) {
	status, err := readSessionsStatus(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case !status.Service.Installed:
		return "not installed; diderot sessions install ships this machine's coding-agent sessions", nil
	case !status.Service.Running:
		return "", fmt.Errorf("the background service is installed but not running; see %s, or run diderot sessions install", status.Log)
	case status.LastError != "":
		return "", fmt.Errorf("the last pass failed: %s", status.LastError)
	case len(status.Unlisted) > 0:
		return "", fmt.Errorf("the last pass could not list %s: %s", status.Unlisted[0].Dir, status.Unlisted[0].Error)
	}
	detail = fmt.Sprintf("%s; %d sessions sent; last pass %s", status.service(), status.Sent, ago(status.LastSuccess))
	if line := status.onServer(); line != "" {
		detail += "; on the server: " + line
	}
	return detail, nil
}
