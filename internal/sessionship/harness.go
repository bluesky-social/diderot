// Package sessionship sends the coding-agent sessions on this machine to a
// Diderot server: it finds each harness's session files, converts a session
// to ATIF here with the decoders the server uses, pushes it, and keeps a
// small state file of what it has sent. It never changes a session file.
package sessionship

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/bluesky-social/diderot/internal/client"
	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

// Harness is one coding-agent harness whose sessions can be shipped: where it
// keeps them, which files are sessions, and how to read one.
type Harness struct {
	Name string
	// Dirs are the harness's session directories under home, after any
	// environment override.
	Dirs func(home string) []string
	// File matches a session file's slash-separated path relative to its
	// source directory and captures the harness's own session ID.
	File *regexp.Regexp
	// HashID is set for a harness whose own session IDs are not UUIDs, which
	// the push route requires: the session is pushed under a name-based UUID
	// (version 5) of the harness's name and its own ID.
	HashID bool
	// Decoder reads the harness's session file.
	Decoder sessiondecode.Decoder
}

const uuidPattern = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

// hashIDNamespace is the namespace of the UUIDs HashID derives. Changing it
// gives every such session a new ID on the server.
var hashIDNamespace = uuid.MustParse("28015b4e-8083-4614-8b26-bfa81fb58087")

// sessionID is the ID a session is pushed under, from the ID File captured.
func (h Harness) sessionID(captured string) string {
	if !h.HashID {
		return captured
	}
	return uuid.NewSHA1(hashIDNamespace, []byte(h.Name+"/"+captured)).String()
}

// Harnesses is every harness sessions are shipped from. Adding one is an
// entry here; it needs a decoder in sessiondecode.
var Harnesses = []Harness{
	{
		Name: "claude-code",
		Dirs: func(home string) []string {
			return []string{filepath.Join(envOr("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude")), "projects")}
		},
		// A project's own sessions; a subagent's transcript is agent-*.jsonl.
		File:    regexp.MustCompile(`(?:^|/)(` + uuidPattern + `)\.jsonl$`),
		Decoder: sessiondecode.ClaudeCode{},
	},
	{
		Name: "codex",
		Dirs: func(home string) []string {
			return []string{filepath.Join(envOr("CODEX_HOME", filepath.Join(home, ".codex")), "sessions")}
		},
		File:    regexp.MustCompile(`(?:^|/)rollout-[^/]*-(` + uuidPattern + `)\.jsonl$`),
		Decoder: sessiondecode.Codex{},
	},
	{
		Name: "polytoken",
		Dirs: func(home string) []string {
			return []string{envOr("POLYTOKEN_DATA_PATH", filepath.Join(envOr("XDG_DATA_HOME", filepath.Join(home, ".local", "share")), "polytoken"))}
		},
		// Each session directory's own log, in the current layout
		// (sessions-v1) or the one older releases wrote (sessions) until
		// Polytoken migrates a session on resume; so the source is the data
		// directory, which holds both. A subagent's transcript
		// (subagent-*.jsonl), trace, usage and shell output are not sessions,
		// and nor is the copy a migration keeps under sessions-migration.
		File:    regexp.MustCompile(`^sessions(?:-v1)?/([^/]+)/log\.jsonl$`),
		HashID:  true,
		Decoder: sessiondecode.Polytoken{},
	},
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Lookup returns the harness called name.
func Lookup(name string) (Harness, bool) {
	for _, h := range Harnesses {
		if h.Name == name {
			return h, true
		}
	}
	return Harness{}, false
}

// Names lists the harnesses.
func Names() []string {
	names := make([]string, len(Harnesses))
	for i, h := range Harnesses {
		names[i] = h.Name
	}
	return names
}

// Source is one directory of one harness's sessions.
type Source struct {
	Harness string `json:"harness"`
	Dir     string `json:"dir"`
}

// Detect returns the session directory of every harness that has one here.
func Detect() ([]Source, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find the home directory: %w", err)
	}
	var found []Source
	for _, h := range Harnesses {
		for _, dir := range h.Dirs(home) {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				found = append(found, Source{Harness: h.Name, Dir: dir})
			}
		}
	}
	return found, nil
}

// sessionFile is one session file found under a source.
type sessionFile struct {
	path string
	// real is path with every link resolved: the file that was checked
	// against the exclusions, and so the one that is read.
	real      string
	sessionID string
	info      fs.FileInfo
}

// files walks a source for its harness's session files, leaving out
// everything under an excluded directory. A source that does not exist has
// none. Symbolic links to files are followed; links to directories are not.
func (s Source) files(exclude []string) ([]sessionFile, error) {
	h, ok := Lookup(s.Harness)
	if !ok {
		return nil, fmt.Errorf("unknown harness %q (known: %v)", s.Harness, Names())
	}
	// WalkDir does not follow a root that is itself a link.
	root, err := filepath.EvalSymlinks(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// An exclusion holds for a directory under every name it has: as it
	// was given, and as its links resolve.
	for _, dir := range exclude {
		if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
			exclude = append(exclude[:len(exclude):len(exclude)], real)
		}
	}
	var found []sessionFile
	err = filepath.WalkDir(root, func(resolved string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Paths are reported and recorded under the source as it was named,
		// not under what its link resolves to.
		path := filepath.Join(s.Dir, strings.TrimPrefix(resolved, root))
		// resolved is a directory's real path: the root's links are resolved
		// and the walk follows none.
		if excluded(path, exclude) || excluded(resolved, exclude) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil {
			return nil
		}
		m := h.File.FindStringSubmatch(filepath.ToSlash(rel))
		if m == nil {
			return nil
		}
		// A session file may be a link, which is followed; what decides
		// whether it is excluded is where the file it leads to really is.
		real, err := filepath.EvalSymlinks(resolved)
		if err != nil || excluded(real, exclude) {
			return nil // a link to nothing, or into an excluded directory
		}
		info, err := os.Stat(real)
		if err != nil || !info.Mode().IsRegular() {
			return nil // gone since the listing
		}
		found = append(found, sessionFile{path: path, real: real, sessionID: h.sessionID(m[1]), info: info})
		return nil
	})
	return found, err
}

// Count is how many session files a source holds.
func (s Source) Count(exclude []string) (int, error) {
	files, err := s.files(exclude)
	return len(files), err
}

func excluded(path string, exclude []string) bool {
	for _, dir := range exclude {
		if rel, err := filepath.Rel(dir, path); err == nil && filepath.IsLocal(rel) {
			return true
		}
	}
	return false
}

// Settings is what `sessions install` saved: the server the person agreed
// to ship to, the directories to ship from, and the directories to leave
// out.
type Settings struct {
	// Server is the server URL install was run for. The background service
	// sends to no other, whatever setup saves afterwards.
	Server  string   `json:"server"`
	Sources []Source `json:"sources"`
	Exclude []string `json:"exclude,omitempty"`
}

// Paths are the files session shipping keeps, beside the client's own.
type Paths struct {
	Settings string // what to ship; written by install
	State    string // what has been sent
	Log      string // the background service's output
}

// DefaultPaths puts the settings beside the saved sign-in and the state and
// log under $XDG_STATE_HOME/diderot, or ~/.local/state/diderot.
func DefaultPaths() (Paths, error) {
	config, err := client.ConfigPath()
	if err != nil {
		return Paths{}, err
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" || !filepath.IsAbs(state) {
		home, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("find the home directory: %w", err)
		}
		state = filepath.Join(home, ".local", "state")
	}
	return Paths{
		Settings: filepath.Join(filepath.Dir(config), "sessions.json"),
		State:    filepath.Join(state, "diderot", "sessions-state.json"),
		Log:      filepath.Join(state, "diderot", "sessions.log"),
	}, nil
}

// LoadSettings reads the saved settings; ok is false when there are none.
func LoadSettings(path string) (settings Settings, ok bool, err error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path is DefaultPaths'
	if errors.Is(err, fs.ErrNotExist) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		return Settings{}, false, fmt.Errorf("read %s: %w", path, err)
	}
	return settings, true, nil
}

// SaveSettings writes the settings, readable only by their owner.
func SaveSettings(path string, settings Settings) error {
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return client.WriteFileAtomic(path, append(b, '\n'), 0o600)
}
