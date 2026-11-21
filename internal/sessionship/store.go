package sessionship

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// SessionStore reads a harness that keeps every session in one database
// file rather than a file per session.
type SessionStore interface {
	// File is the database's name in a source that names its directory.
	File() string
	// Sessions lists the database's sessions, leaving out subagents'.
	Sessions(db string) ([]StoredSession, error)
	// Document is one session as the document the harness's decoder reads.
	// An error reading the database is an *os.PathError, which a pass
	// retries; any other error is about the session's own contents.
	Document(db, id string) ([]byte, error)
}

// StoredSession is one session a SessionStore lists.
type StoredSession struct {
	ID string // the harness's own ID
	// Dir is the directory the session ran in, which an exclusion covers.
	Dir string
	// Rows and Changed are how many rows the session has and when the last
	// was written; with both unchanged, the session has not changed.
	Rows    int64
	Changed time.Time
}

// storedInfo stands in for a session file's metadata: a stored session's
// row count and last write are its size and modification time, which is
// all a pass reads of either.
type storedInfo struct{ s StoredSession }

func (i storedInfo) Name() string       { return i.s.ID }
func (i storedInfo) Size() int64        { return i.s.Rows }
func (i storedInfo) Mode() fs.FileMode  { return 0 }
func (i storedInfo) ModTime() time.Time { return i.s.Changed }
func (i storedInfo) IsDir() bool        { return false }
func (i storedInfo) Sys() any           { return nil }

// stored lists the sessions in a database-backed source, which names the
// database file or the directory holding it. Each is reported as the
// database's path and the session's ID, joined by #. A session is left out
// when the database, or the directory the session ran in, is under an
// excluded directory. root is the source with its links resolved.
func (s Source) stored(h Harness, root string, exclude []string) ([]sessionFile, error) {
	path, db := s.Dir, root
	info, err := os.Stat(db)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		path = filepath.Join(s.Dir, h.Store.File())
		db, err = filepath.EvalSymlinks(filepath.Join(root, h.Store.File()))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	if excluded(path, exclude) || excluded(db, exclude) {
		return nil, nil
	}
	sessions, err := h.Store.Sessions(db)
	if err != nil {
		return nil, err
	}
	// An exclusion also holds under its real name when it names a directory
	// since removed, as the directory a session ran in may be.
	for _, dir := range exclude {
		if real, err := realPath(dir); err == nil && filepath.IsAbs(dir) && real != dir {
			exclude = append(exclude[:len(exclude):len(exclude)], real)
		}
	}
	var found []sessionFile
	for _, session := range sessions {
		if session.Dir != "" && excluded(session.Dir, exclude) {
			continue
		}
		// A directory that cannot be resolved is left out, as a session
		// file whose link cannot be is. Without exclusions there is nothing
		// to resolve it for.
		if len(exclude) > 0 && filepath.IsAbs(session.Dir) {
			if real, err := realPath(session.Dir); err != nil || excluded(real, exclude) {
				continue
			}
		}
		found = append(found, sessionFile{path: path + "#" + session.ID, real: db, sessionID: session.ID, info: storedInfo{session}})
	}
	return found, nil
}

// realPath resolves the links in the longest part of an absolute path that
// still exists and keeps the rest as written: the directory a session ran in
// may have been removed since.
func realPath(path string) (string, error) {
	path, rest := filepath.Clean(path), ""
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return "", err
		}
		path, rest = parent, filepath.Join(filepath.Base(path), rest)
	}
}
