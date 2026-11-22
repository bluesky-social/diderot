package sessionship

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	// The SQLite driver OpenCode's database is read with.
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// openCodeDB is where OpenCode 2 keeps its database: $OPENCODE_DB, a name
// relative to its data directory unless absolute, or opencode.db there. The
// data directory is $XDG_DATA_HOME/opencode, or ~/.local/share/opencode, on
// every platform.
func openCodeDB(home string) string {
	db := envOr("OPENCODE_DB", "opencode.db")
	if filepath.IsAbs(db) {
		return db
	}
	return filepath.Join(envOr("XDG_DATA_HOME", filepath.Join(home, ".local", "share")), "opencode", db)
}

// openCodeStore reads OpenCode 2's sessions from its SQLite database, as the
// document `opencode session export` writes for each. OpenCode 1 kept its
// sessions in other tables, which are not read: OpenCode 2 moves them into
// its own when it first opens the database.
type openCodeStore struct{}

func (openCodeStore) File() string { return "opencode.db" }

// openCodeOpen opens the database read-only. A read-only connection never
// writes the database or its write-ahead log and never checkpoints, so
// OpenCode, which may be writing at the same time, is not held up; a reader
// of a write-ahead-logged database does take its read lock in the -shm file
// beside it, as every reader does.
func openCodeOpen(db string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", "file:"+(&url.URL{Path: db}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: db, Err: err}
	}
	conn.SetMaxOpenConns(1)
	return conn, nil
}

func (openCodeStore) Sessions(db string) ([]StoredSession, error) {
	conn, err := openCodeOpen(db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	var tables int
	err = conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table'
		AND name IN ('session_v2', 'session_message', 'session_part')`).Scan(&tables)
	if err != nil {
		return nil, openCodeErr(db, err)
	}
	if tables < 3 {
		return nil, nil // OpenCode 1's database, not yet moved by OpenCode 2
	}
	// A subagent's session has a parent; its report is in the parent's.
	rows, err := conn.Query(`SELECT s.id, s.directory,
		(SELECT count(*) FROM session_message m WHERE m.session_id = s.id)
			+ (SELECT count(*) FROM session_part p WHERE p.session_id = s.id),
		max(s.time_updated,
			coalesce((SELECT max(m.time_updated) FROM session_message m WHERE m.session_id = s.id), 0),
			coalesce((SELECT max(p.time_updated) FROM session_part p WHERE p.session_id = s.id), 0))
		FROM session_v2 s
		WHERE s.parent_id IS NULL AND EXISTS (SELECT 1 FROM session_message m WHERE m.session_id = s.id)
		ORDER BY s.time_created, s.id`)
	if err != nil {
		return nil, openCodeErr(db, err)
	}
	defer func() { _ = rows.Close() }()
	var out []StoredSession
	for rows.Next() {
		var s StoredSession
		var changed int64
		if err := rows.Scan(&s.ID, &s.Dir, &s.Rows, &changed); err != nil {
			return nil, openCodeErr(db, err)
		}
		s.Changed = time.UnixMilli(changed)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, openCodeErr(db, err)
	}
	return out, nil
}

// openCodeErr is why the database, or a session at path in it, could not be
// read: an *os.PathError, which a pass retries, when a retry may mend it (the
// database is busy, locked, or cannot be opened or read just now, or the
// session is gone since it was listed), and otherwise an error saying the
// database is not as this release reads it (a newer OpenCode changed its
// tables, say).
func openCodeErr(path string, err error) error {
	var e *sqlite.Error
	if errors.As(err, &e) {
		switch e.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_IOERR, sqlite3.SQLITE_CANTOPEN:
			return &os.PathError{Op: "read", Path: path, Err: err}
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		return &os.PathError{Op: "read", Path: path, Err: err}
	}
	return fmt.Errorf("read %s: %w", path, err)
}

// openCodeInfo is a session row as OpenCode exports it (Session.Info, with
// the location's workspace left out), fields in the export's order.
type openCodeInfo struct {
	ID          string          `json:"id"`
	ParentID    string          `json:"parentID,omitempty"`
	Fork        *openCodeFork   `json:"fork,omitempty"`
	ProjectID   string          `json:"projectID"`
	Agent       string          `json:"agent,omitempty"`
	Model       *openCodeModel  `json:"model,omitempty"`
	Cost        float64         `json:"cost"`
	Tokens      openCodeTokens  `json:"tokens"`
	Outcome     string          `json:"outcome,omitempty"`
	Time        openCodeTime    `json:"time"`
	Title       string          `json:"title,omitempty"`
	Location    openCodeLoc     `json:"location"`
	Subpath     string          `json:"subpath,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Permissions json.RawMessage `json:"permissions,omitempty"`
	Revert      json.RawMessage `json:"revert,omitempty"`
}

type openCodeFork struct {
	SessionID string          `json:"sessionID"`
	Boundary  json.RawMessage `json:"boundary"`
}

type openCodeModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant"`
}

type openCodeTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

type openCodeTime struct {
	Created  int64  `json:"created"`
	Updated  int64  `json:"updated"`
	Busy     *int64 `json:"busy,omitempty"`
	Idle     *int64 `json:"idle,omitempty"`
	Viewed   *int64 `json:"viewed,omitempty"`
	Archived *int64 `json:"archived,omitempty"`
}

type openCodeLoc struct {
	Directory string `json:"directory"`
}

// Document assembles the session's export as OpenCode does: the session
// row; then its messages in order, each the row's data with its id and
// type, an assistant message's session_part rows ahead of the content kept
// in the row itself, and a message still being written left out. One
// difference: OpenCode puts the bytes of media kept in its blob table back
// into the export, and here such a file keeps its media: reference, since
// no file's bytes are ever read.
func (openCodeStore) Document(db, id string) ([]byte, error) {
	conn, err := openCodeOpen(db)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	readErr := func(err error) error { return openCodeErr(db+"#"+id, err) }
	// One transaction, so the session is read as of one moment.
	tx, err := conn.Begin()
	if err != nil {
		return nil, readErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	// The rows are bounded before any is read into memory, as a session
	// file's read is.
	var size int64
	err = tx.QueryRow(`SELECT
		coalesce((SELECT sum(octet_length(data)) FROM session_message WHERE session_id = ?1), 0)
		+ coalesce((SELECT sum(octet_length(data)) FROM session_part WHERE session_id = ?1), 0)
		+ coalesce((SELECT ifnull(octet_length(fork_boundary), 0) + ifnull(octet_length(model), 0)
			+ ifnull(octet_length(title), 0) + ifnull(octet_length(directory), 0) + ifnull(octet_length(path), 0)
			+ ifnull(octet_length(metadata), 0) + ifnull(octet_length(permission), 0) + ifnull(octet_length(revert), 0)
			FROM session_v2 WHERE id = ?1), 0)`, id).Scan(&size)
	if err != nil {
		return nil, readErr(err)
	}
	if size > maxSourceBytes {
		return nil, fmt.Errorf("the session is larger than %d bytes", maxSourceBytes)
	}

	var info openCodeInfo
	var parentID, forkID, forkBoundary, agent, model, outcome, title, subpath, metadata, permissions, revert sql.NullString
	var busy, idle, viewed, archived sql.NullInt64
	err = tx.QueryRow(`SELECT id, parent_id, fork_session_id, fork_boundary, project_id, agent, model, cost,
		tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, idle_outcome,
		time_created, time_updated, time_busy, time_idle, time_viewed, time_archived,
		title, directory, path, metadata, permission, revert
		FROM session_v2 WHERE id = ?`, id).Scan(&info.ID, &parentID, &forkID, &forkBoundary, &info.ProjectID, &agent, &model,
		&info.Cost, &info.Tokens.Input, &info.Tokens.Output, &info.Tokens.Reasoning, &info.Tokens.Cache.Read,
		&info.Tokens.Cache.Write, &outcome, &info.Time.Created, &info.Time.Updated, &busy, &idle, &viewed, &archived,
		&title, &info.Location.Directory, &subpath, &metadata, &permissions, &revert)
	if err != nil {
		return nil, readErr(err)
	}
	info.ParentID, info.Agent, info.Outcome, info.Title, info.Subpath = parentID.String, agent.String, outcome.String, title.String, subpath.String
	if forkID.Valid && forkBoundary.Valid {
		info.Fork = &openCodeFork{SessionID: forkID.String, Boundary: json.RawMessage(forkBoundary.String)}
	}
	if model.Valid {
		info.Model = &openCodeModel{}
		if err := json.Unmarshal([]byte(model.String), info.Model); err != nil {
			return nil, fmt.Errorf("session model: %w", err)
		}
		if info.Model.Variant == "" {
			info.Model.Variant = "default"
		}
	}
	info.Metadata, info.Permissions, info.Revert = rawColumn(metadata), rawColumn(permissions), rawColumn(revert)
	info.Time.Busy, info.Time.Idle, info.Time.Viewed, info.Time.Archived = intColumn(busy), intColumn(idle), intColumn(viewed), intColumn(archived)

	parts := map[string][]json.RawMessage{}
	rows, err := tx.Query(`SELECT message_id, data FROM session_part WHERE session_id = ? ORDER BY message_id, ordinal`, id)
	if err != nil {
		return nil, readErr(err)
	}
	for rows.Next() {
		var messageID, data string
		if err := rows.Scan(&messageID, &data); err != nil {
			_ = rows.Close()
			return nil, readErr(err)
		}
		parts[messageID] = append(parts[messageID], json.RawMessage(data))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, readErr(err)
	}

	messages := []json.RawMessage{}
	rows, err = tx.Query(`SELECT id, type, data FROM session_message WHERE session_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, readErr(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var messageID, typ, data string
		if err := rows.Scan(&messageID, &typ, &data); err != nil {
			return nil, readErr(err)
		}
		message, ok, err := openCodeMessage(messageID, typ, data, parts[messageID])
		if err != nil {
			return nil, err
		}
		if ok {
			messages = append(messages, message)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, readErr(err)
	}
	return marshalText(struct {
		Info     openCodeInfo      `json:"info"`
		Messages []json.RawMessage `json:"messages"`
	}{info, messages})
}

// openCodeMessage is one exported message, or ok false for one OpenCode
// leaves out of an export because it is still being written.
func openCodeMessage(id, typ, data string, parts []json.RawMessage) (json.RawMessage, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &fields); err != nil || fields == nil {
		return nil, false, fmt.Errorf("message %s is not a JSON object", id)
	}
	switch typ {
	case "assistant":
		var t struct {
			Time struct {
				Completed json.RawMessage `json:"completed"`
			} `json:"time"`
		}
		if json.Unmarshal([]byte(data), &t) != nil || len(t.Time.Completed) == 0 {
			return nil, false, nil
		}
		if len(parts) > 0 {
			var inline []json.RawMessage
			if raw := fields["content"]; len(raw) > 0 {
				if err := json.Unmarshal(raw, &inline); err != nil {
					return nil, false, fmt.Errorf("message %s content: %w", id, err)
				}
			}
			content, err := marshalText(append(parts[:len(parts):len(parts)], inline...))
			if err != nil {
				return nil, false, err
			}
			fields["content"] = content
		}
	case "shell", "compaction":
		var s struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(data), &s) != nil || s.Status == "running" {
			return nil, false, nil
		}
	}
	fields["id"], _ = json.Marshal(id)
	fields["type"], _ = json.Marshal(typ)
	message, err := marshalText(fields)
	return message, err == nil, err
}

// marshalText encodes v without escaping <, > and & in strings, as
// OpenCode writes them, so a tool call's arguments read the same from this
// document as from OpenCode's own export.
func marshalText(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func rawColumn(s sql.NullString) json.RawMessage {
	if !s.Valid || s.String == "null" {
		return nil
	}
	return json.RawMessage(s.String)
}

func intColumn(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}
