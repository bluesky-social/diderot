package sessionship

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Names of the background service in each service manager.
const (
	LaunchdLabel = "com.github.bluesky-social.diderot.sessions"
	SystemdUnit  = "diderot-sessions.service"
)

// Unit is what the background service runs.
type Unit struct {
	// Binary is the absolute path of the diderot to run.
	Binary string
	// Log receives the service's output.
	Log string
	// Env is the environment the service needs to find the same files this
	// process does. It never holds a credential.
	Env map[string]string
}

// ServiceStatus is the background service as its manager reports it.
type ServiceStatus struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	File      string `json:"file"`
}

// Manager installs the background service for the current user: a launchd
// agent on macOS, a systemd user unit on Linux. It needs no privilege and
// writes nothing outside the home directory.
type Manager struct {
	GOOS string
	Home string
	// ConfigHome holds systemd's user units: $XDG_CONFIG_HOME, or ~/.config.
	ConfigHome string
	UID        int
	// Run runs a service-manager command and returns its output.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// LookPath finds a program, as exec.LookPath does.
	LookPath func(name string) (string, error)
	// Sleep waits between checks that a stopped service is gone.
	Sleep func(ctx context.Context, d time.Duration)
}

// NewManager is the Manager for this machine and user.
func NewManager(goos string) (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find the home directory: %w", err)
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(configHome) {
		configHome = filepath.Join(home, ".config")
	}
	return &Manager{
		GOOS: goos, Home: home, ConfigHome: configHome, UID: os.Getuid(), LookPath: exec.LookPath, Sleep: Sleep,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // launchctl or systemctl with fixed arguments
		},
	}, nil
}

// File is the service definition's path.
func (m *Manager) File() string {
	if m.GOOS == "darwin" {
		return filepath.Join(m.Home, "Library", "LaunchAgents", LaunchdLabel+".plist")
	}
	return filepath.Join(m.ConfigHome, "systemd", "user", SystemdUnit)
}

func (m *Manager) supported(binary string) error {
	switch m.GOOS {
	case "darwin":
		return nil
	case "linux":
		if _, err := m.LookPath("systemctl"); err != nil {
			return fmt.Errorf("this machine has no systemd to run the background service; keep this running under your own supervisor instead: %s sessions run", binary)
		}
		return nil
	}
	return fmt.Errorf("no background service for %s; keep this running instead: %s sessions run", m.GOOS, binary)
}

func (m *Manager) command(ctx context.Context, name string, args ...string) error {
	out, err := m.Run(ctx, name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

func (m *Manager) launchdTarget() string {
	return "gui/" + strconv.Itoa(m.UID) + "/" + LaunchdLabel
}

// Install writes the service definition and starts the service, replacing
// and restarting one that is already there.
func (m *Manager) Install(ctx context.Context, unit Unit) error {
	if err := m.supported(unit.Binary); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(unit.Log), 0o700); err != nil {
		return err
	}
	file := m.File()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil { //nolint:gosec // the service manager's own directory
		return err
	}
	if m.GOOS == "darwin" {
		if err := m.launchdStop(ctx); err != nil {
			return err
		}
		if err := os.WriteFile(file, RenderLaunchd(unit), 0o644); err != nil { //nolint:gosec // launchd reads it; it holds no secret
			return err
		}
		if err := m.command(ctx, "launchctl", "bootstrap", "gui/"+strconv.Itoa(m.UID), file); err != nil {
			return err
		}
		// launchd starts the agent a moment after it loads it.
		for range 25 {
			if m.Status(ctx).Running {
				return nil
			}
			m.Sleep(ctx, 200*time.Millisecond)
		}
		return fmt.Errorf("launchd loaded the session service but has not started it; see %s", unit.Log)
	}
	if err := os.WriteFile(file, RenderSystemd(unit), 0o644); err != nil { //nolint:gosec // systemd reads it; it holds no secret
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", SystemdUnit}, {"restart", SystemdUnit}} {
		if err := m.command(ctx, "systemctl", append([]string{"--user"}, args...)...); err != nil {
			return err
		}
	}
	return nil
}

// launchdStop unloads the agent when it is loaded and waits until launchd
// has let go of it, since a bootstrap right behind a bootout fails.
func (m *Manager) launchdStop(ctx context.Context) error {
	if _, err := m.Run(ctx, "launchctl", "print", m.launchdTarget()); err != nil {
		return nil // not loaded
	}
	if err := m.command(ctx, "launchctl", "bootout", m.launchdTarget()); err != nil {
		return err
	}
	for range 50 {
		if _, err := m.Run(ctx, "launchctl", "print", m.launchdTarget()); err != nil {
			return nil
		}
		m.Sleep(ctx, 200*time.Millisecond)
	}
	return errors.New("launchd did not stop the session service within 10 seconds")
}

// Uninstall stops the service and removes its definition. It is not an
// error when there is none.
func (m *Manager) Uninstall(ctx context.Context) error {
	file := m.File()
	if m.GOOS == "darwin" {
		if err := m.launchdStop(ctx); err != nil {
			return err
		}
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := m.command(ctx, "systemctl", "--user", "disable", "--now", SystemdUnit); err != nil {
		return err
	}
	if err := os.Remove(file); err != nil {
		return err
	}
	return m.command(ctx, "systemctl", "--user", "daemon-reload")
}

var (
	launchdRunning = regexp.MustCompile(`(?m)^\s*state = running\s*$`)
	launchdPID     = regexp.MustCompile(`(?m)^\s*pid = (\d+)\s*$`)
	systemdPID     = regexp.MustCompile(`(?m)^MainPID=(\d+)$`)
)

// Status reports whether the service is installed and running.
func (m *Manager) Status(ctx context.Context) ServiceStatus {
	status := ServiceStatus{File: m.File()}
	// Without the definition there is no service of this home directory's to
	// ask the service manager about.
	if _, err := os.Stat(status.File); err != nil {
		return status
	}
	status.Installed = true
	var pid [][]byte
	if m.GOOS == "darwin" {
		out, err := m.Run(ctx, "launchctl", "print", m.launchdTarget())
		if err != nil {
			return status
		}
		status.Running = launchdRunning.Match(out)
		pid = launchdPID.FindSubmatch(out)
	} else {
		out, err := m.Run(ctx, "systemctl", "--user", "show", "-p", "ActiveState,MainPID", SystemdUnit)
		if err != nil {
			return status
		}
		status.Running = bytes.Contains(out, []byte("ActiveState=active\n"))
		pid = systemdPID.FindSubmatch(out)
	}
	if status.Running && pid != nil {
		status.PID, _ = strconv.Atoi(string(pid[1]))
	}
	return status
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// RenderLaunchd is the launchd agent: started at login and now, restarted
// when it exits with an error, at background priority.
func RenderLaunchd(unit Unit) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + LaunchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlText(unit.Binary) + `</string>
		<string>sessions</string>
		<string>run</string>
	</array>
`)
	if len(unit.Env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, k := range sortedKeys(unit.Env) {
			b.WriteString("\t\t<key>" + xmlText(k) + "</key>\n\t\t<string>" + xmlText(unit.Env[k]) + "</string>\n")
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString(`	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + xmlText(unit.Log) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlText(unit.Log) + `</string>
</dict>
</plist>
`)
	return []byte(b.String())
}

// systemdQuote quotes a value for a unit file: C-style escapes inside
// double quotes, and % doubled so it is not read as a specifier.
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s) + `"`
}

// RenderSystemd is the systemd user unit: started at login and now,
// restarted when it exits with an error.
func RenderSystemd(unit Unit) []byte {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Ship coding-agent sessions to Diderot\n\n[Service]\n")
	// $ is doubled on the command line alone, where systemd expands variables.
	b.WriteString("ExecStart=" + strings.ReplaceAll(systemdQuote(unit.Binary), `$`, `$$`) + " sessions run\n")
	for _, k := range sortedKeys(unit.Env) {
		b.WriteString("Environment=" + systemdQuote(k+"="+unit.Env[k]) + "\n")
	}
	log := strings.ReplaceAll(unit.Log, `%`, `%%`)
	b.WriteString("StandardOutput=append:" + log + "\nStandardError=append:" + log + "\n")
	b.WriteString("Restart=on-failure\nRestartSec=30\n\n[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}
