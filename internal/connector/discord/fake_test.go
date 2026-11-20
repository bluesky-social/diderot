package discord

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Fixture IDs: obviously invented snowflakes.
const (
	fakeGuild    = "100000000000000001"
	fakeBot      = "100000000000000009"
	fakeEveryone = fakeGuild
	fakeText     = "200000000000000001"
	fakeForum    = "200000000000000002"
	fakePrivate  = "200000000000000003"
	fakeVoice    = "200000000000000004"
	fakeCategory = "200000000000000005"
	fakeAlice    = "300000000000000001"
	fakeBob      = "300000000000000002"
	fakeRole     = "400000000000000001"
)

// fakeID is the snowflake of a message created at t, with n to tell
// messages of one millisecond apart.
func fakeID(t time.Time, n int) string {
	return strconv.FormatUint(uint64(t.UnixMilli()-discordEpoch)<<22|uint64(n), 10)
}

// fakeDiscord is a Discord REST API and gateway in memory.
type fakeDiscord struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	channels []channel
	threads  []channel
	messages map[string][]message // by channel, any order
	// failures answers the next requests of a path prefix with a status.
	failures map[string][]int
	// historyFailures answers the next history reads (newest first, no
	// after cursor) of a channel with a status, and nothing else: not a
	// tail, not one message.
	historyFailures map[string][]int
	// limited answers the next request of a path with a 429 first.
	limited  map[string]int
	requests []string
	intentOK bool
	// ask is what Ask posted.
	ask fakeAsk

	// Gateway. As Discord does, the session numbers its dispatches and
	// keeps them, and a resume replays those after the client's sequence,
	// so a dispatch sent while the client reconnects is not lost.
	conns      chan *fakeConn
	identifies int
	resumes    []int64
	upgrader   websocket.Upgrader
	seq        int64
	sent       []payload
	current    *fakeConn
	// handoff orders dispatch against a resume's replay and its move to the
	// new connection, so no dispatch falls between the two.
	handoff sync.Mutex
}

type fakeConn struct {
	f  *fakeDiscord
	ws *websocket.Conn
	mu sync.Mutex
	// in receives what the client sends after identifying.
	in chan payload
}

// send dispatches an event of the session on this connection.
func (c *fakeConn) send(t string, d any) {
	c.write(c.f.record(t, d))
}

func (c *fakeConn) write(p payload) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteJSON(p)
}

// record numbers a dispatch of the session and keeps it for resumes.
func (f *fakeDiscord) record(t string, d any) payload {
	raw, _ := json.Marshal(d)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	seq := f.seq
	p := payload{Op: opDispatch, T: t, D: raw, S: &seq}
	f.sent = append(f.sent, p)
	return p
}

// dispatch sends an event to the session's current connection; a client
// that is reconnecting gets it when it resumes.
func (f *fakeDiscord) dispatch(t string, d any) {
	f.handoff.Lock()
	defer f.handoff.Unlock()
	p := f.record(t, d)
	f.mu.Lock()
	conn := f.current
	f.mu.Unlock()
	if conn != nil {
		conn.write(p)
	}
}

func (c *fakeConn) op(op int, d any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, _ := json.Marshal(d)
	_ = c.ws.WriteJSON(payload{Op: op, D: raw})
}

func (c *fakeConn) close(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
	_ = c.ws.Close()
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{
		t: t, messages: map[string][]message{}, failures: map[string][]int{}, historyFailures: map[string][]int{}, limited: map[string]int{},
		intentOK: true, conns: make(chan *fakeConn, 8),
	}
	f.channels = []channel{
		{ID: fakeCategory, Type: channelCategory, Name: "Projects"},
		{ID: fakeText, Type: channelText, Name: "general", ParentID: fakeCategory, Position: 1},
		{ID: fakeForum, Type: channelForum, Name: "help", Position: 2},
		{ID: fakePrivate, Type: channelText, Name: "staff", Position: 3, PermissionOverwrites: []overwrite{
			{ID: fakeEveryone, Type: 0, Deny: strconv.FormatInt(permViewChannel, 10), Allow: "0"},
			{ID: fakeBot, Type: 1, Allow: strconv.FormatInt(permViewChannel|permReadMessageHistory, 10), Deny: "0"},
		}},
		{ID: fakeVoice, Type: 2, Name: "voice"},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeDiscord) apiBase() string { return f.server.URL + "/api/v10" }

func (f *fakeDiscord) addMessage(channelID string, m message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m.ChannelID = channelID
	f.messages[channelID] = append(f.messages[channelID], m)
}

func (f *fakeDiscord) addThread(t channel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threads = append(f.threads, t)
}

func (f *fakeDiscord) deleteMessage(channelID, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[channelID] = slices.DeleteFunc(f.messages[channelID], func(m message) bool { return m.ID == id })
}

func (f *fakeDiscord) editMessage(channelID, id string, edit func(*message)) message {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.messages[channelID] {
		if f.messages[channelID][i].ID == id {
			edit(&f.messages[channelID][i])
			return f.messages[channelID][i]
		}
	}
	f.t.Fatalf("no message %s in %s", id, channelID)
	return message{}
}

func (f *fakeDiscord) fail(prefix string, statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[prefix] = append(f.failures[prefix], statuses...)
}

// failHistory answers the channel's next history reads with statuses.
func (f *fakeDiscord) failHistory(channelID string, statuses ...int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyFailures[channelID] = append(f.historyFailures[channelID], statuses...)
}

func (f *fakeDiscord) requestCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeDiscord) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeDiscord) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/gateway" {
		f.serveGateway(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/files/") {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG fake " + r.URL.Path))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v10")
	if r.Header.Get("Authorization") != "Bot test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		f.writeJSON(w, map[string]any{"code": 0, "message": "401: Unauthorized"})
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, path)
	for prefix, statuses := range f.failures {
		if strings.HasPrefix(path, prefix) && len(statuses) > 0 {
			f.failures[prefix] = statuses[1:]
			f.mu.Unlock()
			w.WriteHeader(statuses[0])
			f.writeJSON(w, map[string]any{"code": 50001, "message": "Missing Access"})
			return
		}
	}
	if channelID, ok := strings.CutSuffix(strings.TrimPrefix(path, "/channels/"), "/messages"); ok && r.URL.Query().Get("after") == "" {
		if statuses := f.historyFailures[channelID]; len(statuses) > 0 {
			f.historyFailures[channelID] = statuses[1:]
			f.mu.Unlock()
			w.WriteHeader(statuses[0])
			f.writeJSON(w, map[string]any{"code": 50001, "message": "Missing Access"})
			return
		}
	}
	if n := f.limited[path]; n > 0 {
		f.limited[path] = n - 1
		f.mu.Unlock()
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset-After", "0.05")
		w.Header().Set("X-RateLimit-Bucket", "fakebucket")
		w.Header().Set("X-RateLimit-Scope", "user")
		w.WriteHeader(http.StatusTooManyRequests)
		f.writeJSON(w, map[string]any{"message": "You are being rate limited.", "retry_after": 0.05, "global": false})
		return
	}
	defer f.mu.Unlock()
	w.Header().Set("X-RateLimit-Limit", "5")
	w.Header().Set("X-RateLimit-Remaining", "4")
	w.Header().Set("X-RateLimit-Reset-After", "1")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if r.Method != http.MethodGet {
		f.serveWrite(w, r, path, parts)
		return
	}
	switch {
	case path == "/users/@me":
		f.writeJSON(w, user{ID: fakeBot, Username: "diderot-test", Bot: true})
	case path == "/applications/@me":
		flags := 0
		if f.intentOK {
			flags = appFlagMessageContentLimited
		}
		f.writeJSON(w, application{ID: "500000000000000001", Name: "test app", Flags: int64(flags)})
	case path == "/users/@me/guilds":
		if r.URL.Query().Get("after") != "" {
			f.writeJSON(w, []guild{})
			return
		}
		f.writeJSON(w, []guild{{ID: fakeGuild, Name: "Example Server"}})
	case len(parts) == 2 && parts[0] == "users":
		for _, u := range []user{alice, bob} {
			if u.ID == parts[1] {
				f.writeJSON(w, u)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		f.writeJSON(w, map[string]any{"code": 10013})
	case path == "/gateway/bot":
		f.writeJSON(w, map[string]any{"url": "ws" + strings.TrimPrefix(f.server.URL, "http") + "/gateway"})
	case len(parts) == 3 && parts[0] == "guilds" && parts[2] == "roles":
		f.writeJSON(w, []role{
			{ID: fakeEveryone, Name: "@everyone", Permissions: strconv.FormatInt(permViewChannel|permReadMessageHistory, 10)},
			{ID: fakeRole, Name: "maintainers", Permissions: "0"},
		})
	case len(parts) == 3 && parts[0] == "guilds" && parts[2] == "channels":
		f.writeJSON(w, f.channels)
	case len(parts) == 4 && parts[0] == "guilds" && parts[2] == "members":
		if m, ok := f.ask.members[parts[3]]; ok {
			f.writeJSON(w, m)
			return
		}
		f.writeJSON(w, member{Roles: []string{}})
	case len(parts) == 2 && parts[0] == "guilds":
		f.writeJSON(w, guild{ID: parts[1], Name: "Example Server", Features: f.ask.guildFeatures})
	case len(parts) == 4 && parts[0] == "guilds" && parts[2] == "threads" && parts[3] == "active":
		var active []channel
		for _, t := range f.threads {
			if t.ThreadMetadata == nil || !t.ThreadMetadata.Archived {
				active = append(active, t)
			}
		}
		f.writeJSON(w, activeThreads{Threads: active})
	case len(parts) == 5 && parts[0] == "channels" && parts[2] == "threads" && parts[3] == "archived":
		var archived []channel
		for _, t := range f.threads {
			if t.ParentID == parts[1] && t.ThreadMetadata != nil && t.ThreadMetadata.Archived && t.Type != channelPrivateThread {
				archived = append(archived, t)
			}
		}
		f.writeJSON(w, archivedThreads{Threads: archived})
	case len(parts) == 2 && parts[0] == "channels":
		for _, c := range append(slices.Clone(f.channels), f.threads...) {
			if c.ID == parts[1] {
				f.writeJSON(w, c)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		f.writeJSON(w, map[string]any{"code": 10003})
	case len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages":
		f.writeJSON(w, f.page(parts[1], r))
	case len(parts) == 4 && parts[0] == "channels" && parts[2] == "messages":
		for _, m := range f.messages[parts[1]] {
			if m.ID == parts[3] {
				f.writeJSON(w, f.signed(m))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		f.writeJSON(w, map[string]any{"code": 10008})
	default:
		w.WriteHeader(http.StatusNotFound)
		f.writeJSON(w, map[string]any{"code": 0})
	}
}

// signed gives a message's attachments fresh links, as Discord's signed
// URLs are.
func (f *fakeDiscord) signed(m message) message {
	m.Attachments = slices.Clone(m.Attachments)
	for i := range m.Attachments {
		m.Attachments[i].URL = f.server.URL + "/files/" + m.Attachments[i].ID + "?ex=" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return m
}

// page answers a message history request: newest first, with before,
// after and limit.
func (f *fakeDiscord) page(channelID string, r *http.Request) []message {
	all := slices.Clone(f.messages[channelID])
	slices.SortFunc(all, func(a, b message) int {
		if laterID(a.ID, b.ID) {
			return -1
		}
		return 1
	})
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	var out []message
	if after := r.URL.Query().Get("after"); after != "" {
		// The oldest limit messages after the cursor, newest first.
		var newer []message
		for i := len(all) - 1; i >= 0; i-- {
			if laterID(all[i].ID, after) {
				newer = append(newer, all[i])
				if len(newer) == limit {
					break
				}
			}
		}
		slices.Reverse(newer)
		out = newer
	} else {
		before := r.URL.Query().Get("before")
		for _, m := range all {
			if before == "" || laterID(before, m.ID) {
				out = append(out, m)
				if len(out) == limit {
					break
				}
			}
		}
	}
	for i := range out {
		out[i] = f.signed(out[i])
	}
	if out == nil {
		out = []message{}
	}
	return out
}

func (f *fakeDiscord) serveGateway(w http.ResponseWriter, r *http.Request) {
	ws, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	conn := &fakeConn{f: f, ws: ws, in: make(chan payload, 64)}
	conn.op(opHello, map[string]any{"heartbeat_interval": 200})
	var first payload
	if err := ws.ReadJSON(&first); err != nil {
		return
	}
	f.handoff.Lock()
	f.mu.Lock()
	switch first.Op {
	case opIdentify:
		f.identifies++
		// A new session numbers from the start.
		f.seq, f.sent = 0, nil
		var identify struct {
			Token   string `json:"token"`
			Intents int    `json:"intents"`
		}
		_ = json.Unmarshal(first.D, &identify)
		f.mu.Unlock()
		if identify.Token != "test-token" {
			f.handoff.Unlock()
			conn.close(4004)
			return
		}
		if identify.Intents != gatewayIntents {
			f.handoff.Unlock()
			conn.close(4013)
			return
		}
		conn.send("READY", map[string]any{"session_id": "fake-session", "resume_gateway_url": "ws" + strings.TrimPrefix(f.server.URL, "http") + "/gateway"})
	case opResume:
		var resume struct {
			Seq int64 `json:"seq"`
		}
		_ = json.Unmarshal(first.D, &resume)
		f.resumes = append(f.resumes, resume.Seq)
		var missed []payload
		for _, p := range f.sent {
			if *p.S > resume.Seq {
				missed = append(missed, p)
			}
		}
		f.mu.Unlock()
		for _, p := range missed {
			conn.write(p)
		}
		conn.send("RESUMED", map[string]any{})
	default:
		f.mu.Unlock()
		f.handoff.Unlock()
		conn.close(4002)
		return
	}
	f.mu.Lock()
	f.current = conn
	f.mu.Unlock()
	f.handoff.Unlock()
	f.conns <- conn
	for {
		var p payload
		if err := ws.ReadJSON(&p); err != nil {
			return
		}
		if p.Op == opHeartbeat {
			conn.op(opHeartbeatACK, nil)
			continue
		}
		select {
		case conn.in <- p:
		default:
		}
	}
}

// nextConn waits for the client's next gateway session.
func (f *fakeDiscord) nextConn(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("no gateway connection")
		return nil
	}
}

func (f *fakeDiscord) stats() (identifies int, resumes []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identifies, slices.Clone(f.resumes)
}

// msg is a fixture message by author at t.
func msg(id string, author user, t time.Time, content string) message {
	return message{ID: id, Type: 0, Author: author, Content: content, Timestamp: t}
}

var (
	alice = user{ID: fakeAlice, Username: "alice", GlobalName: "Alice Example"}
	bob   = user{ID: fakeBob, Username: "bob"}
)

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
