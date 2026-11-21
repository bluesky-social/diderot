package sessionship

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite the golden service definitions")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o600))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}

var testUnit = Unit{
	Binary: `/Users/ada/My Tools/diderot`,
	Log:    "/Users/ada/.local/state/diderot/sessions.log",
	Env:    map[string]string{"XDG_STATE_HOME": "/Users/ada/state & more", "XDG_CONFIG_HOME": "/Users/ada/50%"},
}

func TestRenderServiceDefinitions(t *testing.T) {
	golden(t, "launchd.plist", RenderLaunchd(testUnit))
	golden(t, "systemd.service", RenderSystemd(testUnit))
	golden(t, "launchd-plain.plist", RenderLaunchd(Unit{Binary: "/opt/homebrew/bin/diderot", Log: "/Users/ada/.local/state/diderot/sessions.log"}))
	golden(t, "systemd-plain.service", RenderSystemd(Unit{Binary: "/usr/local/bin/diderot", Log: "/home/ada/.local/state/diderot/sessions.log"}))
}

// fakeManager records the service-manager commands and plays a launchd or
// systemd that loads what it is told to.
type fakeManager struct {
	*Manager
	calls  []string
	loaded bool
}

func newFakeManager(t *testing.T, goos string) *fakeManager {
	f := &fakeManager{}
	f.Manager = &Manager{
		GOOS: goos, UID: 501,
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		Sleep:    func(context.Context, time.Duration) {},
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			call := name + " " + strings.Join(args, " ")
			f.calls = append(f.calls, call)
			switch {
			case strings.HasPrefix(call, "launchctl print"):
				if !f.loaded {
					return []byte("Could not find service"), errors.New("exit status 113")
				}
				return []byte("gui/501/" + LaunchdLabel + " = {\n\tstate = running\n\tpid = 4242\n}\n"), nil
			case strings.HasPrefix(call, "launchctl bootstrap"), strings.Contains(call, " restart "):
				f.loaded = true
			case strings.HasPrefix(call, "launchctl bootout"), strings.Contains(call, " disable "):
				f.loaded = false
			case strings.Contains(call, " show "):
				if f.loaded {
					return []byte("MainPID=4242\nActiveState=active\n"), nil
				}
				return []byte("MainPID=0\nActiveState=inactive\n"), nil
			}
			return nil, nil
		},
	}
	f.Home = t.TempDir()
	f.ConfigHome = filepath.Join(f.Home, "xdg-config")
	return f
}

func TestInstallAndUninstallLaunchd(t *testing.T) {
	ctx := context.Background()
	f := newFakeManager(t, "darwin")
	unit := Unit{Binary: "/opt/homebrew/bin/diderot", Log: filepath.Join(f.Home, ".local", "state", "diderot", "sessions.log")}
	file := filepath.Join(f.Home, "Library", "LaunchAgents", LaunchdLabel+".plist")
	assert.Equal(t, ServiceStatus{File: file}, f.Status(ctx))

	require.NoError(t, f.Install(ctx, unit))
	assert.Equal(t, []string{
		"launchctl print gui/501/" + LaunchdLabel,
		"launchctl bootstrap gui/501 " + file,
		"launchctl print gui/501/" + LaunchdLabel, // started?
	}, f.calls)
	written, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, string(RenderLaunchd(unit)), string(written))
	assert.Equal(t, ServiceStatus{Installed: true, Running: true, PID: 4242, File: file}, f.Status(ctx))

	// Again: the loaded agent is unloaded first, and the result is the same.
	f.calls = nil
	require.NoError(t, f.Install(ctx, unit))
	assert.Equal(t, []string{
		"launchctl print gui/501/" + LaunchdLabel,
		"launchctl bootout gui/501/" + LaunchdLabel,
		"launchctl print gui/501/" + LaunchdLabel,
		"launchctl bootstrap gui/501 " + file,
		"launchctl print gui/501/" + LaunchdLabel, // started?
	}, f.calls)
	assert.True(t, f.Status(ctx).Running)

	require.NoError(t, f.Uninstall(ctx))
	assert.NoFileExists(t, file)
	assert.Equal(t, ServiceStatus{File: file}, f.Status(ctx))
	// Everything install made is gone: only the directories remain.
	entries, err := os.ReadDir(filepath.Dir(file))
	require.NoError(t, err)
	assert.Empty(t, entries)
	require.NoError(t, f.Uninstall(ctx), "uninstalling nothing is not an error")
}

func TestInstallAndUninstallSystemd(t *testing.T) {
	ctx := context.Background()
	f := newFakeManager(t, "linux")
	unit := Unit{Binary: "/usr/local/bin/diderot", Log: filepath.Join(f.Home, ".local", "state", "diderot", "sessions.log")}
	file := filepath.Join(f.ConfigHome, "systemd", "user", SystemdUnit)

	require.NoError(t, f.Install(ctx, unit))
	require.NoError(t, f.Install(ctx, unit))
	assert.Equal(t, []string{
		"systemctl --user daemon-reload", "systemctl --user enable " + SystemdUnit, "systemctl --user restart " + SystemdUnit,
		"systemctl --user daemon-reload", "systemctl --user enable " + SystemdUnit, "systemctl --user restart " + SystemdUnit,
	}, f.calls)
	written, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, string(RenderSystemd(unit)), string(written))
	assert.Equal(t, ServiceStatus{Installed: true, Running: true, PID: 4242, File: file}, f.Status(ctx))

	f.calls = nil
	require.NoError(t, f.Uninstall(ctx))
	assert.Equal(t, []string{"systemctl --user disable --now " + SystemdUnit, "systemctl --user daemon-reload"}, f.calls)
	assert.NoFileExists(t, file)
	assert.Equal(t, ServiceStatus{File: file}, f.Status(ctx))
	require.NoError(t, f.Uninstall(ctx))

	// Without systemd, install says what to run instead and writes nothing.
	f.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	err = f.Install(ctx, unit)
	require.ErrorContains(t, err, "/usr/local/bin/diderot sessions run")
	assert.NoFileExists(t, file)
}

func TestDetect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("POLYTOKEN_DATA_PATH", "")
	t.Setenv("XDG_DATA_HOME", "")
	found, err := Detect()
	require.NoError(t, err)
	assert.Empty(t, found, "a harness with no session directory is not offered")

	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".copilot", "session-state"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".local", "share", "polytoken", "sessions"), 0o755))
	found, err = Detect()
	require.NoError(t, err)
	assert.Equal(t, []Source{
		{Harness: "claude-code", Dir: filepath.Join(home, ".claude", "projects")},
		{Harness: "codex", Dir: filepath.Join(home, ".codex", "sessions")},
		{Harness: "copilot-cli", Dir: filepath.Join(home, ".copilot", "session-state")},
		{Harness: "polytoken", Dir: filepath.Join(home, ".local", "share", "polytoken")},
	}, found)

	// Each harness's own override moves its directory.
	elsewhere := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(elsewhere, "sessions"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(elsewhere, "session-state"), 0o755))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(elsewhere, "no-claude-here"))
	t.Setenv("CODEX_HOME", elsewhere)
	t.Setenv("COPILOT_HOME", elsewhere)
	t.Setenv("XDG_DATA_HOME", filepath.Join(elsewhere, "no-data-here"))
	found, err = Detect()
	require.NoError(t, err)
	assert.Equal(t, []Source{
		{Harness: "codex", Dir: filepath.Join(elsewhere, "sessions")},
		{Harness: "copilot-cli", Dir: filepath.Join(elsewhere, "session-state")},
	}, found)

	// Polytoken's own override names its data directory itself.
	data := t.TempDir()
	t.Setenv("POLYTOKEN_DATA_PATH", data)
	found, err = Detect()
	require.NoError(t, err)
	assert.Equal(t, []Source{
		{Harness: "codex", Dir: filepath.Join(elsewhere, "sessions")},
		{Harness: "copilot-cli", Dir: filepath.Join(elsewhere, "session-state")},
		{Harness: "polytoken", Dir: data},
	}, found)

	// Every harness in the table has a decoder and names its session files.
	for _, h := range Harnesses {
		assert.NotNil(t, h.Decoder, h.Name)
		assert.Equal(t, 1, h.File.NumSubexp(), h.Name)
	}
	n, err := Source{Harness: "codex", Dir: filepath.Join(elsewhere, "missing")}.Count(nil)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestSettingsAndPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	paths, err := DefaultPaths()
	require.NoError(t, err)
	assert.Equal(t, Paths{
		Settings: filepath.Join(home, ".config", "diderot", "sessions.json"),
		State:    filepath.Join(home, ".local", "state", "diderot", "sessions-state.json"),
		Log:      filepath.Join(home, ".local", "state", "diderot", "sessions.log"),
	}, paths)

	_, ok, err := LoadSettings(paths.Settings)
	require.NoError(t, err)
	assert.False(t, ok)
	want := Settings{Server: "https://diderot.example.com/mcp", Sources: []Source{{Harness: "codex", Dir: "/somewhere"}}, Exclude: []string{"/somewhere/private"}}
	require.NoError(t, SaveSettings(paths.Settings, want))
	got, ok, err := LoadSettings(paths.Settings)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, want, got)

	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	paths, err = DefaultPaths()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "state", "diderot", "sessions.log"), paths.Log)
}
