package sessionship

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bluesky-social/diderot/internal/client"
	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

// maxSourceBytes bounds the session file one pass reads into memory. It is
// four times the server's default document limit; the server refuses what is
// over its own. A variable so a test can lower it.
var maxSourceBytes int64 = 256 << 20

// State is what has been sent, by session file. It is rewritten after every
// accepted push, so a pass that is killed loses at most the push in flight,
// which the server recognizes when it is sent again.
type State struct {
	// URL is the server the revisions belong to; another server starts over.
	URL   string                `json:"url"`
	Files map[string]*FileState `json:"files"`

	LastPass    time.Time `json:"last_pass,omitzero"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	// Unlisted is each source the last pass could not list.
	Unlisted []SourceError `json:"unlisted,omitempty"`
}

// FileState is one session file as the last pass left it.
type FileState struct {
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	// Size and ModTime are the file's when it was last read; a file with
	// both unchanged is not read again. For a session in a SessionStore they
	// are its row count and last write.
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	// Revision is the server's revision of the session, and SHA256 the hash
	// of the document that became it. Empty when nothing was accepted yet.
	Revision string `json:"revision,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	// Skipped says why the file's current contents were not sent.
	Skipped string `json:"skipped,omitempty"`
}

// LoadState reads the state file; a missing one is an empty state.
func LoadState(path string) (*State, error) {
	state := &State{Files: map[string]*FileState{}}
	b, err := os.ReadFile(path) //nolint:gosec // the path is DefaultPaths'
	if errors.Is(err, fs.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, state); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if state.Files == nil {
		state.Files = map[string]*FileState{}
	}
	return state, nil
}

func (s *State) save(path string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return client.WriteFileAtomic(path, b, 0o600)
}

// Counts is how many sessions the state holds as sent and as skipped.
func (s *State) Counts() (sent, skipped int) {
	for _, f := range s.Files {
		if f.Revision != "" {
			sent++
		}
		if f.Skipped != "" {
			skipped++
		}
	}
	return sent, skipped
}

// ErrBusy means another pass (the background service's, usually) holds the
// state file.
var ErrBusy = errors.New("another sync is running")

func lockState(path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // beside the state file
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // a descriptor fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // as above
		_ = f.Close()
	}, nil
}

// Outcome is what a pass did with one session file.
type Outcome struct {
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	// Action is created, updated, or skipped.
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// Result is one pass over every source.
type Result struct {
	Sent      []Outcome `json:"sent"`
	Skipped   []Outcome `json:"skipped"`
	Unchanged int       `json:"unchanged"`
	// Waiting counts files changed too recently to send yet.
	Waiting int `json:"waiting"`
	// Unlisted is each source whose sessions could not be listed; the pass
	// went on to the others.
	Unlisted []SourceError `json:"unlisted,omitempty"`
}

// SourceError is why a source's sessions could not be listed.
type SourceError struct {
	Harness string `json:"harness"`
	Dir     string `json:"dir"`
	Error   string `json:"error"`
}

// Syncer makes passes over the sources, sending what changed.
type Syncer struct {
	Settings  Settings
	StatePath string
	// ServerURL identifies the server in the state file.
	ServerURL string
	// Put sends one session's document (Pusher.Put).
	Put func(ctx context.Context, sessionID string, document []byte, revision string) (string, error)
	// Quiet is how long a file must have gone unwritten before it is sent,
	// so a session in progress becomes a new revision on the server when
	// its harness pauses, not every pass. Zero sends every changed file.
	Quiet time.Duration
	Now   func() time.Time
}

// Pass sends every session file that changed since it was last sent, one at
// a time. A file the server or the converter refuses is recorded as skipped
// and left until it changes. A source that can be read but not listed (a
// session database laid out as this release does not read) is reported, and
// the pass goes on to the next. An error that is not about one file or
// source (the server is down, the sign-in was refused, a directory cannot be
// read) ends the pass, and the next pass resumes from what the state file
// holds.
func (s *Syncer) Pass(ctx context.Context) (Result, error) {
	result := Result{Sent: []Outcome{}, Skipped: []Outcome{}}
	unlock, err := lockState(s.StatePath)
	if err != nil {
		return result, err
	}
	defer unlock()
	state, err := LoadState(s.StatePath)
	if err != nil {
		return result, err
	}
	if state.URL != s.ServerURL {
		state = &State{URL: s.ServerURL, Files: map[string]*FileState{}}
	}
	passErr := s.pass(ctx, state, &result)
	state.LastPass, state.Unlisted = s.Now(), result.Unlisted
	if passErr == nil {
		state.LastSuccess = state.LastPass
	} else if !errors.Is(passErr, context.Canceled) {
		state.LastError, state.LastErrorAt = passErr.Error(), state.LastPass
	}
	if err := state.save(s.StatePath); err != nil {
		return result, errors.Join(passErr, err)
	}
	return result, passErr
}

func (s *Syncer) pass(ctx context.Context, state *State, result *Result) error {
	for _, source := range s.Settings.Sources {
		harness, ok := Lookup(source.Harness)
		if !ok {
			return fmt.Errorf("unknown harness %q (known: %v)", source.Harness, Names())
		}
		files, err := source.files(s.Settings.Exclude)
		var unreadable *os.PathError
		switch {
		case err != nil && !errors.As(err, &unreadable):
			// Retrying does not mend it, and it must not hold up the
			// sources after it.
			result.Unlisted = append(result.Unlisted, SourceError{Harness: harness.Name, Dir: source.Dir, Error: err.Error()})
			continue
		case err != nil:
			return fmt.Errorf("read %s: %w", source.Dir, err)
		}
		for _, file := range files {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.one(ctx, state, result, harness, file); err != nil {
				return err
			}
		}
	}
	return nil
}

// one handles one session file. It returns an error only for a failure that
// should end the pass.
func (s *Syncer) one(ctx context.Context, state *State, result *Result, harness Harness, file sessionFile) error {
	previous := state.Files[file.path]
	if previous != nil && previous.Size == file.info.Size() && previous.ModTime.Equal(file.info.ModTime()) {
		result.Unchanged++
		return nil
	}
	if s.Quiet > 0 && s.Now().Sub(file.info.ModTime()) < s.Quiet {
		result.Waiting++
		return nil
	}
	next := &FileState{Harness: harness.Name, SessionID: file.sessionID, Size: file.info.Size(), ModTime: file.info.ModTime()}
	if previous != nil {
		next.Revision, next.SHA256 = previous.Revision, previous.SHA256
	}
	outcome := Outcome{Harness: harness.Name, SessionID: file.sessionID, Path: file.path}
	skip := func(reason string) error {
		next.Skipped = reason
		state.Files[file.path] = next
		outcome.Action, outcome.Reason = "skipped", reason
		result.Skipped = append(result.Skipped, outcome)
		return state.save(s.StatePath)
	}

	sessionID, document, err := convert(harness, file)
	var unreadable *os.PathError
	switch {
	case errors.As(err, &unreadable):
		// Not recorded: the next pass reads it again.
		outcome.Action, outcome.Reason = "skipped", err.Error()
		result.Skipped = append(result.Skipped, outcome)
		return nil
	case err != nil:
		return skip(err.Error())
	}
	next.SessionID, outcome.SessionID = sessionID, sessionID
	sum := sha256.Sum256(document)
	next.SHA256 = hex.EncodeToString(sum[:])
	if previous != nil && previous.Revision != "" && previous.SHA256 == next.SHA256 {
		// The file changed in a way its session did not.
		state.Files[file.path] = next
		result.Unchanged++
		return nil
	}

	revision, err := s.Put(ctx, sessionID, document, next.Revision)
	var rejected *RejectedError
	switch {
	case errors.As(err, &rejected):
		next.SHA256 = ""
		if previous != nil {
			next.SHA256 = previous.SHA256
		}
		return skip(rejected.Reason)
	case err != nil:
		return err
	}
	outcome.Action = "updated"
	if next.Revision == "" {
		outcome.Action = "created"
	}
	next.Revision = revision
	state.Files[file.path] = next
	result.Sent = append(result.Sent, outcome)
	return state.save(s.StatePath)
}

// convert reads a session file and converts it to the ATIF document that is
// sent, and returns the ID the session is pushed under with it
// (Harness.sessionID). A last line still being written (one with no newline
// yet) is left for the pass that sees it whole.
func convert(harness Harness, file sessionFile) (string, []byte, error) {
	if harness.Store != nil {
		data, err := harness.Store.Document(file.real, file.sessionID)
		if err != nil {
			return "", nil, err
		}
		if int64(len(data)) > maxSourceBytes {
			return "", nil, fmt.Errorf("the session is larger than %d bytes", maxSourceBytes)
		}
		return encode(harness, file, data)
	}
	f, err := os.Open(file.real)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = f.Close() }()
	// The read is bounded, not the size the listing saw: the harness may
	// have written a great deal since.
	data, err := io.ReadAll(io.LimitReader(f, maxSourceBytes+1))
	if err != nil {
		return "", nil, err
	}
	if int64(len(data)) > maxSourceBytes {
		return "", nil, fmt.Errorf("the file is larger than %d bytes", maxSourceBytes)
	}
	data = data[:bytes.LastIndexByte(data, '\n')+1]
	return encode(harness, file, data)
}

// encode decodes a session document with its harness's decoder and writes
// the result as ATIF, with the ID the session is pushed under.
func encode(harness Harness, file sessionFile, data []byte) (string, []byte, error) {
	doc, records, err := sessiondecode.DecodeAll(harness.Decoder, data)
	if err != nil {
		return "", nil, err
	}
	sessionID, err := harness.sessionID(file.sessionID, doc.SessionID)
	if err != nil {
		return "", nil, err
	}
	document, err := sessiondecode.EncodeATIF(sessionID, doc, records)
	return sessionID, document, err
}

// Poll intervals of the background service.
const (
	// PollInterval is the time between passes while they succeed.
	PollInterval = time.Minute
	// QuietPeriod is the service's Syncer.Quiet.
	QuietPeriod = 5 * time.Minute
	// maxBackoff caps the wait after repeated failures.
	maxBackoff = 30 * time.Minute
)

// Run makes a pass, waits, and repeats until ctx ends. After a failed pass
// the wait doubles, up to half an hour, and a server's Retry-After is
// honored; a pass that succeeds restores the usual interval. report is
// called after every pass.
func Run(ctx context.Context, pass func(context.Context) (Result, error), report func(Result, error, time.Duration), sleep func(context.Context, time.Duration)) {
	wait := PollInterval
	for ctx.Err() == nil {
		result, err := pass(ctx)
		if ctx.Err() != nil {
			return
		}
		var busy *BusyError
		switch {
		case err == nil, errors.Is(err, ErrBusy):
			wait = PollInterval
		default:
			wait = min(wait*2, maxBackoff)
			if errors.As(err, &busy) {
				wait = max(wait, busy.RetryAfter)
			}
		}
		report(result, err, wait)
		sleep(ctx, wait)
	}
}

// Sleep waits for d or until ctx ends.
func Sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
