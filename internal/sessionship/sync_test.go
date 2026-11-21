package sessionship

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/client"
	"github.com/bluesky-social/diderot/internal/sessiondecode"
)

const (
	claudeSession = "0f6b8a8e-1d47-4f6c-9a0e-3f0c4f1b2a10"
	codexSession  = "5c1d2e3f-4a5b-4c6d-8e7f-90a1b2c3d4e5"
)

// claudeTurn is one synthetic Claude Code user or assistant line; each line
// answers the one before it.
func claudeTurn(i int) string {
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
	parent := "null"
	if i > 0 {
		parent = fmt.Sprintf("%q", fmt.Sprintf("00000000-0000-4000-8000-%012d", i-1))
	}
	ts := time.Date(2026, 11, 1, 10, 0, i, 0, time.UTC).Format(time.RFC3339)
	if i%2 == 0 {
		return fmt.Sprintf(`{"type":"user","uuid":%q,"parentUuid":%s,"timestamp":%q,"version":"2.1.323","message":{"role":"user","content":"question %d"}}`, id, parent, ts, i) + "\n"
	}
	return fmt.Sprintf(`{"type":"assistant","uuid":%q,"parentUuid":%s,"timestamp":%q,"message":{"role":"assistant","model":"synthetic-model","content":[{"type":"text","text":"answer %d"}]}}`, id, parent, ts, i) + "\n"
}

func claudeTurns(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(claudeTurn(i))
	}
	return b.String()
}

// fakeServer is the push route as the server serves it: the precondition
// table, the ATIF decoder as its validator, and a strong revision per body.
type fakeServer struct {
	t  *testing.T
	mu sync.Mutex
	// sessions maps a session ID to its current document.
	sessions map[string][]byte
	puts     int
	// fail, when set, answers every request.
	fail func(w http.ResponseWriter)
}

func revisionOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "hmac-sha256:" + hex.EncodeToString(sum[:8])
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		f.fail(w)
		return
	}
	id, ok := strings.CutPrefix(r.URL.Path, "/v1/ingest/agent-sessions/")
	if !ok || r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return
	}
	writeJSON := func(status int, v any) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	current, exists := f.sessions[id]
	if r.Method == http.MethodGet {
		if !exists {
			writeJSON(http.StatusNotFound, map[string]any{"error": "not_found"})
			return
		}
		_, _ = w.Write(current)
		return
	}
	f.puts++
	assert.Equal(f.t, sessiondecode.FormatATIF, r.Header.Get("Diderot-Session-Format"))
	assert.Equal(f.t, "gzip", r.Header.Get("Content-Encoding"))
	zr, err := gzip.NewReader(r.Body)
	require.NoError(f.t, err)
	body, err := io.ReadAll(zr)
	require.NoError(f.t, err)
	if _, _, err := sessiondecode.DecodeAll(sessiondecode.ATIF{}, body); err != nil {
		writeJSON(http.StatusUnprocessableEntity, map[string]any{"error": "undecodable", "detail": err.Error()})
		return
	}
	matches := exists && string(current) == string(body)
	refuse := func() {
		writeJSON(http.StatusPreconditionFailed, map[string]any{
			"error": "precondition_failed", "current_revision": revisionOf(current), "body_matches_current": matches,
		})
	}
	switch ifMatch := strings.Trim(r.Header.Get("If-Match"), `"`); {
	case r.Header.Get("If-None-Match") == "*":
		if exists {
			refuse()
			return
		}
		f.sessions[id] = body
		writeJSON(http.StatusCreated, map[string]string{"revision": revisionOf(body)})
	case exists && (ifMatch == revisionOf(current) || matches):
		f.sessions[id] = body
		writeJSON(http.StatusOK, map[string]string{"revision": revisionOf(body)})
	case exists:
		refuse()
	default:
		writeJSON(http.StatusPreconditionRequired, map[string]any{"error": "precondition_required"})
	}
}

// records decodes what the server holds for a session.
func (f *fakeServer) records(id string) []sessiondecode.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, records, err := sessiondecode.DecodeAll(sessiondecode.ATIF{}, f.sessions[id])
	require.NoError(f.t, err)
	return records
}

type fixture struct {
	server *fakeServer
	syncer *Syncer
	claude string // the Claude Code session file
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	fake := &fakeServer{t: t, sessions: map[string][]byte{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	pusher := &Pusher{
		MCPURL: srv.URL + "/mcp", HTTP: srv.Client(),
		Authorization: func(context.Context) (string, error) { return "Bearer test-token", nil },
	}
	f := &fixture{server: fake, now: time.Now(), claude: filepath.Join(root, "claude", "-work-demo", claudeSession+".jsonl")}
	f.syncer = &Syncer{
		Settings: Settings{Sources: []Source{
			{Harness: "claude-code", Dir: filepath.Join(root, "claude")},
			{Harness: "codex", Dir: filepath.Join(root, "codex")},
		}},
		StatePath: filepath.Join(root, "state", "sessions-state.json"), ServerURL: pusher.MCPURL,
		Put: pusher.Put, Now: func() time.Time { return f.now },
	}
	write(t, f.claude, claudeTurns(2))
	return f
}

func write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func (f *fixture) pass(t *testing.T) Result {
	t.Helper()
	result, err := f.syncer.Pass(context.Background())
	require.NoError(t, err)
	return result
}

func TestPassSendsBothHarnessesAsATIF(t *testing.T) {
	f := newFixture(t)
	codex, err := os.ReadFile("../sessiondecode/testdata/codex/basic.jsonl")
	require.NoError(t, err)
	codexDir := f.syncer.Settings.Sources[1].Dir
	write(t, filepath.Join(codexDir, "2026", "11", "01", "rollout-2026-11-01T11-00-00-"+codexSession+".jsonl"), string(codex))
	// Files that are not sessions are never read: a subagent's transcript,
	// even one whose name ends in a session ID, a file whose name matches
	// only together with the directory above it, and anything else a
	// harness keeps beside its sessions.
	write(t, filepath.Join(filepath.Dir(f.claude), claudeSession, "subagents", "agent-a1b2c3.jsonl"), "not a session\n")
	write(t, filepath.Join(filepath.Dir(f.claude), claudeSession, "subagents", "agent-"+codexSession+".jsonl"), "not a session\n")
	write(t, filepath.Join(codexDir, "rollout-old", "notes-"+codexSession+".jsonl"), "not a session\n")
	write(t, filepath.Join(filepath.Dir(f.claude), "notes.txt"), "not a session\n")

	result := f.pass(t)
	require.Len(t, result.Sent, 2)
	assert.Equal(t, Outcome{Harness: "claude-code", SessionID: claudeSession, Path: f.claude, Action: "created"}, result.Sent[0])
	assert.Equal(t, "codex", result.Sent[1].Harness)
	assert.Equal(t, codexSession, result.Sent[1].SessionID)
	assert.Empty(t, result.Skipped)

	// The server's decoder gets back what the harness's own decoder reads.
	_, want, err := sessiondecode.DecodeAll(sessiondecode.Codex{}, codex)
	require.NoError(t, err)
	got := f.server.records(codexSession)
	require.Len(t, got, len(want))
	assert.Len(t, f.server.records(claudeSession), 2)

	info, err := os.Stat(f.syncer.StatePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestPassSendsPolytokenSessions: a Polytoken session is its directory's
// log, in the current layout or the older one, pushed under a UUID derived
// from its own ID. Nothing else under the data directory is read, and a
// session Polytoken migrates to the current layout stays one session.
func TestPassSendsPolytokenSessions(t *testing.T) {
	f := newFixture(t)
	data := t.TempDir()
	f.syncer.Settings.Sources = append(f.syncer.Settings.Sources, Source{Harness: "polytoken", Dir: data})
	basic, err := os.ReadFile("../sessiondecode/testdata/polytoken/basic.jsonl")
	require.NoError(t, err)
	legacy, err := os.ReadFile("../sessiondecode/testdata/polytoken/legacy.jsonl")
	require.NoError(t, err)
	// Only the older layout exists yet.
	older := filepath.Join(data, "sessions", "0abc34-older", "log.jsonl")
	write(t, older, string(legacy))
	result := f.pass(t)
	require.Len(t, result.Sent, 2)
	olderID := uuid.NewSHA1(hashIDNamespace, []byte("polytoken/0abc34-older")).String()
	assert.Equal(t, Outcome{Harness: "polytoken", SessionID: olderID, Path: older, Action: "created"}, result.Sent[1])

	session := filepath.Join(data, "sessions-v1", "0abc12-demo")
	write(t, filepath.Join(session, "log.jsonl"), string(basic))
	for _, name := range []string{"subagent-general-purpose_describe-swatch.jsonl", "trace.jsonl", "log.jsonl.usage",
		"log.jsonl.corrupt-20261101T100000Z", "shell/job-1.stdout", "attachments/artifact-1.bin"} {
		write(t, filepath.Join(session, name), "not a session\n")
	}
	for _, name := range []string{"public-spool/0abc12-demo/checkpoint-1.jsonl", "logs/2026-11-01T10-00-00Z-100.log"} {
		write(t, filepath.Join(data, name), "not a session\n")
	}
	// The empty directory a newer release leaves in the older layout.
	require.NoError(t, os.MkdirAll(filepath.Join(data, "sessions", "0abc12-demo"), 0o755))
	result = f.pass(t)
	require.Len(t, result.Sent, 1)
	assert.Empty(t, result.Skipped)
	polytoken, _ := Lookup("polytoken")
	id := polytoken.sessionID("0abc12-demo")
	assert.Equal(t, uuid.NewSHA1(hashIDNamespace, []byte("polytoken/0abc12-demo")).String(), id)
	assert.Equal(t, id, result.Sent[0].SessionID)
	_, want, err := sessiondecode.DecodeAll(sessiondecode.Polytoken{}, basic)
	require.NoError(t, err)
	assert.Len(t, f.server.records(id), len(want))

	// Resuming the older session migrates it: the log moves to the current
	// layout and the original is kept aside, which is never read.
	require.NoError(t, os.MkdirAll(filepath.Join(data, "sessions-migration", "0abc34-older"), 0o755))
	require.NoError(t, os.Rename(filepath.Dir(older), filepath.Join(data, "sessions-migration", "0abc34-older", "legacy-backup")))
	write(t, filepath.Join(data, "sessions-v1", "0abc34-older", "log.jsonl"), string(basic))
	result = f.pass(t)
	require.Len(t, result.Sent, 1)
	assert.Equal(t, olderID, result.Sent[0].SessionID)
	assert.Len(t, f.server.sessions, 3, "a migrated session is still one session")
	assert.Len(t, f.server.records(olderID), len(want))
}

func TestPassIsIncremental(t *testing.T) {
	f := newFixture(t)
	require.Len(t, f.pass(t).Sent, 1)
	// Unchanged: nothing is read or sent.
	result := f.pass(t)
	assert.Empty(t, result.Sent)
	assert.Equal(t, 1, result.Unchanged)
	assert.Equal(t, 1, f.server.puts)

	// The session grew: the whole session replaces the server's revision.
	write(t, f.claude, claudeTurns(4))
	result = f.pass(t)
	require.Len(t, result.Sent, 1)
	assert.Equal(t, "updated", result.Sent[0].Action)
	assert.Len(t, f.server.records(claudeSession), 4)
	assert.Len(t, f.server.sessions, 1, "a session that grew is still one session")

	// A last line still being written is left out, and sent once whole.
	write(t, f.claude, claudeTurns(4)+strings.TrimSuffix(claudeTurn(4), "\n")[:40])
	result = f.pass(t)
	assert.Empty(t, result.Sent, "only a partial line was added, so the session did not change")
	assert.Equal(t, 2, f.server.puts)
	write(t, f.claude, claudeTurns(5))
	require.Len(t, f.pass(t).Sent, 1)
	assert.Len(t, f.server.records(claudeSession), 5)

	// The source is only ever read.
	after, err := os.ReadFile(f.claude)
	require.NoError(t, err)
	assert.Equal(t, claudeTurns(5), string(after))
	entries, err := os.ReadDir(filepath.Dir(f.claude))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "nothing is written beside a session file")
}

func TestPassWaitsForAQuietFile(t *testing.T) {
	f := newFixture(t)
	f.syncer.Quiet = 5 * time.Minute
	result := f.pass(t)
	assert.Empty(t, result.Sent)
	assert.Equal(t, 1, result.Waiting)

	f.now = f.now.Add(6 * time.Minute)
	require.Len(t, f.pass(t).Sent, 1)
}

func TestPassRetriesAfterAServerError(t *testing.T) {
	f := newFixture(t)
	second := filepath.Join(filepath.Dir(f.claude), "1a2b3c4d-0000-4000-8000-000000000001.jsonl")
	write(t, second, claudeTurns(2))

	f.server.fail = func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, `{"error":"busy"}`, http.StatusServiceUnavailable)
	}
	_, err := f.syncer.Pass(context.Background())
	var busy *BusyError
	require.ErrorAs(t, err, &busy)
	assert.Equal(t, 7*time.Second, busy.RetryAfter)
	state, err := LoadState(f.syncer.StatePath)
	require.NoError(t, err)
	assert.Empty(t, state.Files, "nothing is recorded as sent")
	assert.NotEmpty(t, state.LastError)
	assert.True(t, state.LastSuccess.IsZero())

	f.server.fail = nil
	require.Len(t, f.pass(t).Sent, 2)
	state, err = LoadState(f.syncer.StatePath)
	require.NoError(t, err)
	sent, skipped := state.Counts()
	assert.Equal(t, 2, sent)
	assert.Zero(t, skipped)
	assert.False(t, state.LastSuccess.Before(state.LastErrorAt))

	// A refused sign-in is the client's sign-in error, for the exit code.
	f.server.fail = func(w http.ResponseWriter) { http.Error(w, "invalid token", http.StatusUnauthorized) }
	write(t, second, claudeTurns(4))
	_, err = f.syncer.Pass(context.Background())
	var signIn *client.SignInError
	require.ErrorAs(t, err, &signIn)

	// A request a proxy timed out is tried again, not recorded as refused.
	f.server.fail = func(w http.ResponseWriter) { http.Error(w, "timed out", http.StatusRequestTimeout) }
	result, err := f.syncer.Pass(context.Background())
	require.Error(t, err)
	assert.Empty(t, result.Skipped)

	// A server with push off, or a gateway that forwards only MCP.
	f.server.fail = func(w http.ResponseWriter) { http.Error(w, "push ingest is not enabled", http.StatusNotFound) }
	_, err = f.syncer.Pass(context.Background())
	require.ErrorContains(t, err, "does not accept pushed sessions")
}

// A lost state file, or a push whose answer was lost, never makes a second
// session: the server's copy is found by the session's ID and replaced.
func TestPassRecoversFromLostState(t *testing.T) {
	f := newFixture(t)
	require.Len(t, f.pass(t).Sent, 1)

	require.NoError(t, os.Remove(f.syncer.StatePath))
	result := f.pass(t)
	require.Len(t, result.Sent, 1, "the same bytes are recognized by the server")
	assert.Len(t, f.server.sessions, 1)

	require.NoError(t, os.Remove(f.syncer.StatePath))
	write(t, f.claude, claudeTurns(4))
	require.Len(t, f.pass(t).Sent, 1)
	assert.Len(t, f.server.sessions, 1)
	assert.Len(t, f.server.records(claudeSession), 4)

	// Another server starts from nothing.
	f.syncer.ServerURL = "https://other.example.com/mcp"
	f.server.sessions = map[string][]byte{}
	require.Len(t, f.pass(t).Sent, 1)
}

func TestPassSkipsWhatCannotBeSent(t *testing.T) {
	f := newFixture(t)
	broken := filepath.Join(filepath.Dir(f.claude), "1a2b3c4d-0000-4000-8000-000000000002.jsonl")
	write(t, broken, "this is not JSON\n")
	private := filepath.Join(filepath.Dir(filepath.Dir(f.claude)), "-work-private", "1a2b3c4d-0000-4000-8000-000000000003.jsonl")
	write(t, private, claudeTurns(2))
	f.syncer.Settings.Exclude = []string{filepath.Dir(private)}

	result := f.pass(t)
	require.Len(t, result.Sent, 1)
	require.Len(t, result.Skipped, 1)
	assert.Equal(t, broken, result.Skipped[0].Path)
	assert.Contains(t, result.Skipped[0].Reason, "line 1 is not a JSON object")
	assert.NotContains(t, f.server.sessions, "1a2b3c4d-0000-4000-8000-000000000003", "an excluded directory is never sent")

	// A skipped file is not tried again until it changes.
	result = f.pass(t)
	assert.Empty(t, result.Skipped)
	assert.Equal(t, 2, result.Unchanged)
	write(t, broken, claudeTurns(2))
	require.Len(t, f.pass(t).Sent, 1)
}

// A session whose real location is under an excluded directory is never
// sent, whatever name reached it: a link to it from outside the excluded
// directory, or a source or an exclude named through a link.
func TestExcludeFollowsLinks(t *testing.T) {
	f := newFixture(t)
	root := filepath.Dir(filepath.Dir(f.claude))
	real := filepath.Join(filepath.Dir(root), "elsewhere", "private")
	secret := filepath.Join(real, "1a2b3c4d-0000-4000-8000-000000000004.jsonl")
	write(t, secret, claudeTurns(2))
	// A link in a shipped project to a session that lives in the excluded one.
	require.NoError(t, os.Symlink(secret, filepath.Join(filepath.Dir(f.claude), filepath.Base(secret))))
	// A link to a session that is not excluded is still followed.
	public := filepath.Join(filepath.Dir(root), "elsewhere", "public", "1a2b3c4d-0000-4000-8000-000000000005.jsonl")
	write(t, public, claudeTurns(2))
	require.NoError(t, os.Symlink(public, filepath.Join(filepath.Dir(f.claude), filepath.Base(public))))

	f.syncer.Settings.Exclude = []string{real}
	result := f.pass(t)
	assert.Len(t, result.Sent, 2)
	assert.Nil(t, f.server.sessions["1a2b3c4d-0000-4000-8000-000000000004"], "the excluded session was sent through a link")
	assert.NotNil(t, f.server.sessions["1a2b3c4d-0000-4000-8000-000000000005"])

	// The source named through a link, the exclude by the real directory
	// beneath it; then the other way round.
	alias := filepath.Join(filepath.Dir(root), "alias")
	require.NoError(t, os.Symlink(root, alias))
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	for _, tc := range []struct{ dir, exclude string }{
		{alias, filepath.Join(realRoot, "-work-demo")},
		{realRoot, filepath.Join(alias, "-work-demo")},
	} {
		files, err := Source{Harness: "claude-code", Dir: tc.dir}.files([]string{tc.exclude})
		require.NoError(t, err)
		assert.Empty(t, files, "source %s, exclude %s", tc.dir, tc.exclude)
	}
}

// Only the server's own refusal of a document is recorded against its file.
// A 404 or any other answer from something in front of the server ends the
// pass, to be tried again.
func TestOnlyTheServersOwnRefusalsSkipAFile(t *testing.T) {
	f := newFixture(t)
	for _, answer := range []struct {
		status int
		body   string
	}{
		{http.StatusNotFound, `{"error":"route_not_found"}`},
		{http.StatusBadRequest, `{"error":"bad_gateway_request"}`},
		{http.StatusForbidden, `{"error":"blocked_by_policy"}`},
		{http.StatusBadRequest, `{"error":"invalid_gzip"}`},
	} {
		f.server.fail = func(w http.ResponseWriter) { http.Error(w, answer.body, answer.status) }
		result, err := f.syncer.Pass(context.Background())
		require.Error(t, err, answer.body)
		assert.Empty(t, result.Skipped, answer.body)
	}
	state, err := LoadState(f.syncer.StatePath)
	require.NoError(t, err)
	assert.Empty(t, state.Files, "nothing was recorded as skipped")
	pusher := &Pusher{MCPURL: f.syncer.ServerURL, HTTP: http.DefaultClient}
	require.Error(t, pusher.Check(context.Background()), "a 404 that is not the push route's own is not a ready server")

	for _, answer := range []struct {
		status int
		body   string
	}{
		{http.StatusUnprocessableEntity, `{"error":"undecodable","detail":"step 1"}`},
		{http.StatusRequestEntityTooLarge, `{"error":"too_large","limit":10}`},
		{http.StatusConflict, `{"error":"conflict","detail":"session is bound to format claude-code-jsonl"}`},
		{http.StatusForbidden, `{"error":"forbidden"}`},
	} {
		f.server.fail = func(w http.ResponseWriter) { http.Error(w, answer.body, answer.status) }
		write(t, f.claude, claudeTurns(2)+"\n") // changed, so it is tried again
		write(t, f.claude, claudeTurns(2))
		require.NoError(t, os.Remove(f.syncer.StatePath))
		result := f.pass(t)
		assert.Len(t, result.Skipped, 1, answer.body)
	}
}

// The limit holds for what is read, not for the size the listing saw.
func TestPassBoundsWhatItReads(t *testing.T) {
	f := newFixture(t)
	original := maxSourceBytes
	t.Cleanup(func() { maxSourceBytes = original })
	maxSourceBytes = int64(len(claudeTurns(2))) + 10

	files, err := f.syncer.Settings.Sources[0].files(nil)
	require.NoError(t, err)
	require.Len(t, files, 1)
	write(t, f.claude, claudeTurns(6)) // grew after it was listed
	_, err = convert(Harnesses[0], files[0])
	require.ErrorContains(t, err, "larger than")

	result := f.pass(t)
	require.Len(t, result.Skipped, 1)
	assert.Contains(t, result.Skipped[0].Reason, "larger than")
	assert.Zero(t, f.server.puts)
}

func TestPassRefusesToRunTwice(t *testing.T) {
	f := newFixture(t)
	unlock, err := lockState(f.syncer.StatePath)
	require.NoError(t, err)
	_, err = f.syncer.Pass(context.Background())
	require.ErrorIs(t, err, ErrBusy)
	unlock()
	require.Len(t, f.pass(t).Sent, 1)
}

func TestPusherCheck(t *testing.T) {
	f := newFixture(t)
	pusher := &Pusher{
		MCPURL: f.syncer.ServerURL, HTTP: http.DefaultClient,
		Authorization: func(context.Context) (string, error) { return "Bearer test-token", nil },
	}
	require.NoError(t, pusher.Check(context.Background()))
	f.server.fail = func(w http.ResponseWriter) { http.Error(w, "team member access required", http.StatusForbidden) }
	require.ErrorContains(t, pusher.Check(context.Background()), "does not let this account push sessions")

	got, err := ServerURL("https://diderot.example.com/team/mcp", pushRoute+claudeSession)
	require.NoError(t, err)
	assert.Equal(t, "https://diderot.example.com/team/v1/ingest/agent-sessions/"+claudeSession, got)

	// What the server did with the sessions it stored.
	f.server.fail = func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"owner":"ada@example.com","sessions":[{"indexed":true},{"indexed":true},{},{"summary_failed":true},{"deleted":true}]}`))
	}
	indexed, err := pusher.Indexed(context.Background())
	require.NoError(t, err)
	assert.Equal(t, Indexed{Listed: 4, Indexed: 2, Waiting: 1, Failed: 1}, indexed)
}

func TestRunBacksOffAndRecovers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	outcomes := []error{nil, errors.New("down"), errors.New("down"), &BusyError{RetryAfter: time.Hour}, nil, ErrBusy}
	var waits []time.Duration
	pass := func(context.Context) (Result, error) {
		err := outcomes[0]
		outcomes = outcomes[1:]
		if len(outcomes) == 0 {
			defer cancel()
		}
		return Result{}, err
	}
	for range 8 {
		outcomes = append(outcomes[:3], append([]error{errors.New("down")}, outcomes[3:]...)...)
	}
	Run(ctx, pass, func(Result, error, time.Duration) {}, func(_ context.Context, d time.Duration) { waits = append(waits, d) })
	assert.Equal(t, []time.Duration{
		time.Minute,                      // a pass that worked
		2 * time.Minute, 4 * time.Minute, // failures double the wait
		8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute,
		30 * time.Minute, 30 * time.Minute, 30 * time.Minute, // up to half an hour
		time.Hour,   // the server's Retry-After, when longer
		time.Minute, // recovered
	}, waits)
}
