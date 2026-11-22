package sessionship

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

// openCodeDDL is OpenCode 2.3.2's schema for the tables the store reads.
const openCodeDDL = "CREATE TABLE `session_v2` (`id` text PRIMARY KEY, `project_id` text NOT NULL, `workspace_id` text, " +
	"`parent_id` text, `fork_session_id` text, `fork_boundary` text, `slug` text NOT NULL, `directory` text NOT NULL, " +
	"`path` text, `title` text, `version` text NOT NULL, `share_url` text, `summary_additions` integer, " +
	"`summary_deletions` integer, `summary_files` integer, `summary_diffs` text, `metadata` text, " +
	"`cost` real DEFAULT 0 NOT NULL, `tokens_input` integer DEFAULT 0 NOT NULL, `tokens_output` integer DEFAULT 0 NOT NULL, " +
	"`tokens_reasoning` integer DEFAULT 0 NOT NULL, `tokens_cache_read` integer DEFAULT 0 NOT NULL, " +
	"`tokens_cache_write` integer DEFAULT 0 NOT NULL, `revert` text, `permission` text, `agent` text, `model` text, " +
	"`time_created` integer NOT NULL, `time_updated` integer NOT NULL, `time_busy` integer, `time_idle` integer, " +
	"`time_viewed` integer, `idle_outcome` text, `time_compacting` integer, `time_archived` integer, " +
	"`time_suspended` integer, `resume_attempts` integer DEFAULT 0 NOT NULL);" +
	"CREATE TABLE `session_message` (`id` text PRIMARY KEY, `session_id` text NOT NULL, `type` text NOT NULL, " +
	"`seq` integer NOT NULL, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, `data` text NOT NULL);" +
	"CREATE UNIQUE INDEX `session_message_session_seq_idx` ON `session_message` (`session_id`,`seq`);" +
	"CREATE TABLE `session_part` (`message_id` text NOT NULL, `session_id` text NOT NULL, `ordinal` integer NOT NULL, " +
	"`type` text NOT NULL, `call_id` text, `time_created` integer NOT NULL, `time_updated` integer NOT NULL, " +
	"`data` text NOT NULL, CONSTRAINT `session_part_pk` PRIMARY KEY(`message_id`, `ordinal`));"

type openCodeFixture struct {
	db, sessionID, dir string
	export             []byte
	conn               *sql.DB
	seq                int
}

// newOpenCodeDB stores the synthetic export as OpenCode 2 does: the session
// row; one row per message holding its data less its id and type; and, as
// with OpenCode's part rows, the first assistant message's content in
// session_part rows. Beside it are what is never shipped: a subagent's
// session, a session with no messages, and a message still being written.
func newOpenCodeDB(t *testing.T, dir string) *openCodeFixture {
	t.Helper()
	export, err := os.ReadFile("../sessiondecode/testdata/opencode/basic.json")
	require.NoError(t, err)
	var doc struct {
		Info struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectID"`
			Model     struct {
				ID         string `json:"id"`
				ProviderID string `json:"providerID"`
			} `json:"model"`
			Cost   float64 `json:"cost"`
			Tokens struct {
				Input, Output, Reasoning int64
				Cache                    struct{ Read, Write int64 }
			} `json:"tokens"`
			Outcome string `json:"outcome"`
			Time    struct {
				Created, Updated, Busy, Idle int64
			} `json:"time"`
			Title    string                     `json:"title"`
			Location struct{ Directory string } `json:"location"`
			Revert   json.RawMessage            `json:"revert"`
		} `json:"info"`
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(export, &doc))
	require.NoError(t, os.MkdirAll(dir, 0o755))
	f := &openCodeFixture{db: filepath.Join(dir, "opencode.db"), sessionID: doc.Info.ID, dir: doc.Info.Location.Directory, export: export}
	f.conn, err = sql.Open("sqlite", f.db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.conn.Close() })
	f.exec(t, "PRAGMA journal_mode=WAL")
	f.exec(t, openCodeDDL)
	info := doc.Info
	model, _ := json.Marshal(map[string]string{"id": info.Model.ID, "providerID": info.Model.ProviderID})
	f.exec(t, `INSERT INTO session_v2 (id, project_id, slug, directory, title, version, cost, tokens_input, tokens_output,
		tokens_reasoning, tokens_cache_read, tokens_cache_write, revert, model, time_created, time_updated, time_busy,
		time_idle, idle_outcome) VALUES (?, ?, 'synthetic-slug', ?, ?, '2.3.2', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		info.ID, info.ProjectID, info.Location.Directory, info.Title, info.Cost, info.Tokens.Input, info.Tokens.Output,
		info.Tokens.Reasoning, info.Tokens.Cache.Read, info.Tokens.Cache.Write, string(info.Revert), string(model),
		info.Time.Created, info.Time.Updated, info.Time.Busy, info.Time.Idle, info.Outcome)
	split := false
	for _, m := range doc.Messages {
		var id, typ string
		require.NoError(t, json.Unmarshal(m["id"], &id))
		require.NoError(t, json.Unmarshal(m["type"], &typ))
		delete(m, "id")
		delete(m, "type")
		if typ == "assistant" && !split {
			split = true
			var parts []json.RawMessage
			require.NoError(t, json.Unmarshal(m["content"], &parts))
			for i, p := range parts {
				var head struct{ Type, ID string }
				require.NoError(t, json.Unmarshal(p, &head))
				f.exec(t, `INSERT INTO session_part (message_id, session_id, ordinal, type, call_id, time_created, time_updated, data)
					VALUES (?, ?, ?, ?, nullif(?, ''), ?, ?, ?)`, id, info.ID, i, head.Type, head.ID, info.Time.Created, info.Time.Created, string(p))
			}
			m["content"] = json.RawMessage(`[]`)
		}
		f.message(t, info.ID, id, typ, m)
	}
	// A message still being written is not in an export.
	f.message(t, info.ID, "msg_1f0000000099SyntheticMsg99", "assistant", map[string]json.RawMessage{
		"time": json.RawMessage(`{"created":1793527299000}`), "agent": json.RawMessage(`"build"`),
		"model":   json.RawMessage(`{"id":"synthetic-haiku","providerID":"synthetic","variant":"default"}`),
		"content": json.RawMessage(`[{"type":"text","text":"STREAMING-SYNTHETIC"}]`),
	})
	f.exec(t, `INSERT INTO session_v2 (id, project_id, parent_id, slug, directory, version, time_created, time_updated)
		VALUES ('ses_0000000000ffSyntheticChild1', ?, ?, 'child', ?, '2.3.2', 1, 1),
		       ('ses_0000000000ffSyntheticEmpty1', ?, NULL, 'empty', ?, '2.3.2', 1, 1)`,
		info.ProjectID, info.ID, info.Location.Directory, info.ProjectID, info.Location.Directory)
	f.message(t, "ses_0000000000ffSyntheticChild1", "msg_1f0000000098SyntheticMsg98", "user", map[string]json.RawMessage{
		"time": json.RawMessage(`{"created":1793527222000}`), "text": json.RawMessage(`"CHILD-SYNTHETIC"`),
	})
	return f
}

func (f *openCodeFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	_, err := f.conn.Exec(query, args...)
	require.NoError(t, err)
}

func (f *openCodeFixture) message(t *testing.T, session, id, typ string, data map[string]json.RawMessage) {
	t.Helper()
	var at struct {
		Time struct{ Created int64 } `json:"time"`
	}
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &at))
	f.seq += 3
	f.exec(t, `INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, session, typ, f.seq, at.Time.Created, at.Time.Created, string(raw))
}

func jsonValue(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v))
	return v
}

// TestOpenCodeStoreBuildsTheExport: the document built from the database is
// the export OpenCode writes for the same session, and reading it changes
// nothing in the database file.
func TestOpenCodeStoreBuildsTheExport(t *testing.T) {
	f := newOpenCodeDB(t, t.TempDir())
	require.NoError(t, f.conn.Close())
	before, err := os.ReadFile(f.db)
	require.NoError(t, err)

	sessions, err := openCodeStore{}.Sessions(f.db)
	require.NoError(t, err)
	require.Len(t, sessions, 1, "a subagent's session and an empty one are not listed")
	assert.Equal(t, f.sessionID, sessions[0].ID)
	assert.Equal(t, f.dir, sessions[0].Dir)
	assert.EqualValues(t, 27+1+5, sessions[0].Rows, "messages and part rows")
	assert.Equal(t, time.UnixMilli(1793527299000), sessions[0].Changed)

	document, err := openCodeStore{}.Document(f.db, f.sessionID)
	require.NoError(t, err)
	assert.Equal(t, jsonValue(t, f.export), jsonValue(t, document))
	_, want, err := sessiondecode.DecodeAll(sessiondecode.OpenCode{}, f.export)
	require.NoError(t, err)
	_, got, err := sessiondecode.DecodeAll(sessiondecode.OpenCode{}, document)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	after, err := os.ReadFile(f.db)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(before, after), "reading wrote to the database")
	_, err = openCodeStore{}.Document(f.db, "ses_missing")
	var notRead *os.PathError
	assert.ErrorAs(t, err, &notRead, "a session that cannot be read is retried, not skipped")

	// A session over the size limit is refused before its rows are read,
	// whether its messages or its own row are what is large.
	defer func(limit int64) { maxSourceBytes = limit }(maxSourceBytes)
	maxSourceBytes = int64(len(f.export)) / 4
	_, err = openCodeStore{}.Document(f.db, f.sessionID)
	require.ErrorContains(t, err, "larger than")
	assert.False(t, errors.As(err, &notRead), "an oversized session is skipped until it changes")
	maxSourceBytes = int64(len(f.export)) * 2
	_, err = openCodeStore{}.Document(f.db, f.sessionID)
	require.NoError(t, err)
	conn, err := sql.Open("sqlite", f.db)
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE session_v2 SET metadata = json_object('note', ?) WHERE id = ?`, strings.Repeat("x", len(f.export)*2), f.sessionID)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	_, err = openCodeStore{}.Document(f.db, f.sessionID)
	require.ErrorContains(t, err, "larger than")
}

// TestOpenCodeStoreRefusesMalformedContent: a message whose own content is
// not a list is refused, as the decoder refuses it, even when its part rows
// would stand in for it.
func TestOpenCodeStoreRefusesMalformedContent(t *testing.T) {
	f := newOpenCodeDB(t, t.TempDir())
	f.exec(t, `UPDATE session_message SET data = json_set(data, '$.content', json('{"x":1}'))
		WHERE id = (SELECT message_id FROM session_part LIMIT 1)`)
	_, err := openCodeStore{}.Document(f.db, f.sessionID)
	require.ErrorContains(t, err, "content")
	var notRead *os.PathError
	assert.False(t, errors.As(err, &notRead), "a malformed session is skipped until it changes")
}

// TestPassGoesOnPastAnUnlistableStore: a database this release cannot list
// is reported, and the sources after it still ship; one that cannot be read
// just now ends the pass, for a later one to retry, as before.
func TestPassGoesOnPastAnUnlistableStore(t *testing.T) {
	f := newFixture(t)
	oc := newOpenCodeDB(t, filepath.Join(t.TempDir(), "opencode"))
	f.syncer.Settings.Sources = append([]Source{{Harness: "opencode", Dir: oc.db}}, f.syncer.Settings.Sources...)
	// A newer OpenCode renamed a column the listing reads.
	oc.exec(t, "ALTER TABLE session_v2 RENAME COLUMN directory TO location")
	result := f.pass(t)
	require.Len(t, result.Sent, 1)
	assert.Equal(t, "claude-code", result.Sent[0].Harness)
	require.Len(t, result.Unlisted, 1)
	assert.Equal(t, "opencode", result.Unlisted[0].Harness)
	assert.Equal(t, oc.db, result.Unlisted[0].Dir)
	assert.Contains(t, result.Unlisted[0].Error, "no such column")
	state, err := LoadState(f.syncer.StatePath)
	require.NoError(t, err)
	assert.Equal(t, result.Unlisted, state.Unlisted)
	assert.Equal(t, state.LastPass, state.LastSuccess)

	// Listed again, it ships and is no longer reported.
	oc.exec(t, "ALTER TABLE session_v2 RENAME COLUMN location TO directory")
	result = f.pass(t)
	require.Len(t, result.Sent, 1)
	assert.Equal(t, "opencode", result.Sent[0].Harness)
	assert.Empty(t, result.Unlisted)
	state, err = LoadState(f.syncer.StatePath)
	require.NoError(t, err)
	assert.Empty(t, state.Unlisted)

	// A database that cannot be opened ends the pass before the sources
	// after it. (SQLite would reuse the fixture's own open file.)
	require.NoError(t, oc.conn.Close())
	require.NoError(t, os.Chmod(oc.db, 0))
	t.Cleanup(func() { _ = os.Chmod(oc.db, 0o600) })
	write(t, f.claude, claudeTurns(4))
	puts := f.server.puts
	result, err = f.syncer.Pass(context.Background())
	var unreadable *os.PathError
	require.ErrorAs(t, err, &unreadable)
	assert.Empty(t, result.Sent)
	assert.Empty(t, result.Unlisted)
	assert.Equal(t, puts, f.server.puts)
}

// TestPassRecordsAnUnreadableOpenCodeSession: a session the database cannot
// give as this release reads it is recorded as skipped and not read again
// until it changes; one it cannot read just now is retried.
func TestPassRecordsAnUnreadableOpenCodeSession(t *testing.T) {
	f := newFixture(t)
	oc := newOpenCodeDB(t, filepath.Join(t.TempDir(), "opencode"))
	f.syncer.Settings.Sources = append(f.syncer.Settings.Sources, Source{Harness: "opencode", Dir: oc.db})
	// A newer OpenCode renamed a column only a session's own read uses.
	oc.exec(t, "ALTER TABLE session_v2 RENAME COLUMN project_id TO project")
	result := f.pass(t)
	require.Len(t, result.Skipped, 1)
	assert.Contains(t, result.Skipped[0].Reason, "no such column")
	state, err := LoadState(f.syncer.StatePath)
	require.NoError(t, err)
	assert.Contains(t, state.Files[oc.db+"#"+oc.sessionID].Skipped, "no such column")
	result = f.pass(t)
	assert.Empty(t, result.Skipped, "an unchanged session is not read again")

	oc.exec(t, "ALTER TABLE session_v2 RENAME COLUMN project TO project_id")
	oc.exec(t, `UPDATE session_v2 SET time_updated = 1793527500000 WHERE id = ?`, oc.sessionID)
	result = f.pass(t)
	require.Len(t, result.Sent, 1)
	assert.Equal(t, "opencode", result.Sent[0].Harness)

	// SQLite would reuse the fixture's own open file.
	require.NoError(t, oc.conn.Close())
	require.NoError(t, os.Chmod(oc.db, 0))
	t.Cleanup(func() { _ = os.Chmod(oc.db, 0o600) })
	_, err = openCodeStore{}.Document(oc.db, oc.sessionID)
	var unreadable *os.PathError
	require.ErrorAs(t, err, &unreadable, "a database that cannot be opened just now is retried")
}

// TestOpenCodeStoreSkipsOpenCode1: OpenCode 1 kept its sessions in other
// tables; until OpenCode 2 moves them, the database has none to ship.
func TestOpenCodeStoreSkipsOpenCode1(t *testing.T) {
	db := filepath.Join(t.TempDir(), "opencode.db")
	conn, err := sql.Open("sqlite", db)
	require.NoError(t, err)
	_, err = conn.Exec("CREATE TABLE `session` (`id` text PRIMARY KEY); CREATE TABLE `message` (`id` text PRIMARY KEY)")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	sessions, err := openCodeStore{}.Sessions(db)
	require.NoError(t, err)
	assert.Empty(t, sessions)
	_, err = openCodeStore{}.Sessions(filepath.Join(t.TempDir(), "missing.db"))
	assert.Error(t, err, "a database that is not there is never created")
}

// TestPassSendsOpenCodeSessions: an OpenCode session is a row of its
// database, named by the database and its ID, pushed under a UUID derived
// from that ID, and sent again when a message is added.
func TestPassSendsOpenCodeSessions(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "opencode")
	oc := newOpenCodeDB(t, dir)
	// A source may name the database's directory, as here, or the file.
	f.syncer.Settings.Sources = append(f.syncer.Settings.Sources, Source{Harness: "opencode", Dir: dir})
	result := f.pass(t)
	require.Len(t, result.Sent, 2)
	id := uuid.NewSHA1(hashIDNamespace, []byte("opencode/"+oc.sessionID)).String()
	assert.Equal(t, Outcome{Harness: "opencode", SessionID: id, Path: oc.db + "#" + oc.sessionID, Action: "created"}, result.Sent[1])
	_, want, err := sessiondecode.DecodeAll(sessiondecode.OpenCode{}, oc.export)
	require.NoError(t, err)
	assert.Len(t, f.server.records(id), len(want))

	assert.Empty(t, f.pass(t).Sent, "an unchanged session is not sent again")
	// A message after the staged revert's boundary is hidden as the rest
	// are: the session is read again, and nothing new is sent.
	oc.message(t, oc.sessionID, "msg_1f0000000097SyntheticMsg97", "system", map[string]json.RawMessage{
		"time": json.RawMessage(`{"created":1793527300000}`), "text": json.RawMessage(`"A later notice."`),
	})
	result = f.pass(t)
	assert.Empty(t, result.Sent)
	assert.Equal(t, 2, result.Unchanged)
	// Undoing the revert changes only the session's own row, and brings
	// back what it hid.
	oc.exec(t, `UPDATE session_v2 SET revert = NULL, time_updated = 1793527400000 WHERE id = ?`, oc.sessionID)
	result = f.pass(t)
	require.Len(t, result.Sent, 1)
	assert.Equal(t, "updated", result.Sent[0].Action)
	assert.Len(t, f.server.records(id), len(want)+3)

	file := Source{Harness: "opencode", Dir: oc.db}
	n, err := file.Count(nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = file.Count([]string{filepath.Dir(oc.dir)})
	require.NoError(t, err)
	assert.Zero(t, n, "a session run under an excluded directory is left out")
	n, err = file.Count([]string{dir})
	require.NoError(t, err)
	assert.Zero(t, n, "a database under an excluded directory is not read")
	n, err = Source{Harness: "opencode", Dir: filepath.Join(dir, "missing")}.Count(nil)
	require.NoError(t, err)
	assert.Zero(t, n)

	// A directory recorded through a link and since removed is excluded
	// under the link's real name.
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(real, link))
	oc.exec(t, `UPDATE session_v2 SET directory = ? WHERE id = ?`, filepath.Join(link, "removed", "project"), oc.sessionID)
	n, err = file.Count([]string{real})
	require.NoError(t, err)
	assert.Zero(t, n)
	n, err = file.Count(nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	// So is one recorded under its real name, and excluded through the
	// link after it was removed.
	oc.exec(t, `UPDATE session_v2 SET directory = ? WHERE id = ?`, filepath.Join(real, "removed", "project"), oc.sessionID)
	n, err = file.Count([]string{filepath.Join(link, "removed")})
	require.NoError(t, err)
	assert.Zero(t, n)
}
