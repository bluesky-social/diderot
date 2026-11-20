package slack

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/askchat"
)

// sessionThread is a Slack thread.
type sessionThread struct{ channelID, threadTS string }

// fakeWorkingThreads is slack_working_threads in memory. A row's expiry is a
// flag the test sets, rather than a time.
type fakeWorkingThreads struct {
	mu       sync.Mutex
	rows     map[sessionThread]*fakeWorkingRow
	finishes []fakeFinish
}

type fakeWorkingRow struct {
	holder                  uuid.UUID
	expired, finished, stop bool
}

type fakeFinish struct {
	cleared, finished bool
}

func newFakeWorkingThreads() *fakeWorkingThreads {
	return &fakeWorkingThreads{rows: map[sessionThread]*fakeWorkingRow{}}
}

func (f *fakeWorkingThreads) Begin(_ context.Context, channelID, threadTS string, holder uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[sessionThread{channelID, threadTS}] = &fakeWorkingRow{holder: holder}
	return nil
}

func (f *fakeWorkingThreads) Renew(_ context.Context, channelID, threadTS string, holder uuid.UUID) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.rows[sessionThread{channelID, threadTS}]
	if row == nil || row.holder != holder {
		return false, false, nil
	}
	return row.stop, true, nil
}

func (f *fakeWorkingThreads) Finish(_ context.Context, channelID, threadTS string, holder uuid.UUID, cleared, finished bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes = append(f.finishes, fakeFinish{cleared: cleared, finished: finished})
	key := sessionThread{channelID, threadTS}
	row := f.rows[key]
	if row == nil || row.holder != holder {
		return nil
	}
	if cleared {
		delete(f.rows, key)
		return nil
	}
	row.finished = finished
	row.expired = !finished
	return nil
}

func (f *fakeWorkingThreads) RequestStop(_ context.Context, channelID, threadTS string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row := f.rows[sessionThread{channelID, threadTS}]; row != nil {
		row.stop = true
	}
	return nil
}

func (f *fakeWorkingThreads) Expired(context.Context) ([]workingThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []workingThread
	for key, row := range f.rows {
		if row.expired {
			out = append(out, workingThread{ChannelID: key.channelID, ThreadTS: key.threadTS, Holder: row.holder})
		}
	}
	return out, nil
}

func (f *fakeWorkingThreads) Claim(_ context.Context, w workingThread) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := f.rows[sessionThread{w.ChannelID, w.ThreadTS}]
	if row == nil || row.holder != w.Holder || !row.expired {
		return false, false, nil
	}
	notify := !row.finished && !row.stop
	row.finished, row.expired = true, false
	return true, notify, nil
}

func (f *fakeWorkingThreads) Release(_ context.Context, w workingThread) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := sessionThread{w.ChannelID, w.ThreadTS}
	if row := f.rows[key]; row != nil && row.holder == w.Holder {
		delete(f.rows, key)
	}
	return nil
}

func (f *fakeWorkingThreads) snapshot() (map[sessionThread]fakeWorkingRow, []fakeFinish) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rows := map[sessionThread]fakeWorkingRow{}
	for key, row := range f.rows {
		rows[key] = *row
	}
	return rows, append([]fakeFinish(nil), f.finishes...)
}

// blockingRouter runs a turn until its context ends, as a model call does.
type blockingRouter struct {
	started chan struct{}
	turnCtx chan context.Context
}

func newBlockingRouter() *blockingRouter {
	return &blockingRouter{started: make(chan struct{}), turnCtx: make(chan context.Context, 1)}
}

func (r *blockingRouter) LookupOrCreateSlackSession(context.Context, string, string, string) (uuid.UUID, error) {
	return uuid.MustParse("00000000-0000-0000-0000-000000000001"), nil
}

func (r *blockingRouter) SlackSessionState(context.Context, string, string) (bool, time.Time, error) {
	return true, time.Time{}, nil
}

func (r *blockingRouter) ExecuteTurn(ctx context.Context, _ ask.TurnRequest) (ask.TurnResult, error) {
	r.turnCtx <- ctx
	close(r.started)
	<-ctx.Done()
	return ask.TurnResult{}, ctx.Err()
}

// slackRecorder records the Web API methods a test's fake Slack served.
type slackRecorder struct {
	mu      sync.Mutex
	methods []string
	texts   []string
}

func (s *slackRecorder) handler(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.mu.Lock()
	s.methods = append(s.methods, r.URL.Path)
	s.texts = append(s.texts, r.Form.Get("text"))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":""}`))
}

func (s *slackRecorder) calls() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.methods...), append([]string(nil), s.texts...)
}

func runBlockingTurn(consumer *Consumer, parent context.Context, done chan<- struct{}) {
	go func() {
		defer close(done)
		consumer.runAskTurn(parent, "C_TEST", "100.000", "100.000", "101.000", "claim-key",
			"U_TEST", "what changed?", "what changed?", "", time.Now(), false, nil, nil,
			[]string{"public", "internal", "slack-channel:C_TEST"}, false)
	}()
}

// Pressing stop ends the turn the way its asker meant: the model's context
// ends with ask.ErrTurnStopped, nothing is posted to the thread, the 👀 comes
// off, and the session is back to active.
func TestStopButtonStopsTheTurnAndPostsNothing(t *testing.T) {
	transport := &fakeProgressTransport{}
	router := newBlockingRouter()
	slackAPI := &slackRecorder{}
	consumer := newProgressIntegrationConsumer(t, transport, router, slackAPI.handler)
	store := consumer.working.(*fakeWorkingThreads)

	done := make(chan struct{})
	runBlockingTurn(consumer, context.Background(), done)
	<-router.started
	waitForLen(t, func() int { return len(transport.statusValues()) }, 1, "status set")

	consumer.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: "agent_session_stopped",
			Data: &slackevents.AgentSessionStoppedEvent{Channel: "C_TEST", ThreadTimestamp: "100.000", User: "U_TEST"}},
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stopped turn did not end")
	}

	assert.True(t, ask.TurnStopped(<-router.turnCtx), "the model's context names the stop as its cause")
	methods, _ := slackAPI.calls()
	assert.NotContains(t, methods, "/chat.postMessage", "a stopped turn posts nothing")
	assert.Contains(t, methods, "/reactions.remove", "the 👀 comes off")
	statuses := transport.statusValues()
	assert.Equal(t, slackgo.AgentSessionStatusProcessing, statuses[0])
	assert.Equal(t, slackgo.AgentSessionStatusActive, statuses[len(statuses)-1])
	rows, finishes := store.snapshot()
	assert.Empty(t, rows, "the cleared thread's record is gone")
	assert.Equal(t, []fakeFinish{{cleared: true, finished: true}}, finishes)
}

// A turn cut off before it said anything (its process lost leadership) is
// left for the next leader, which tells the thread and sets it back to
// active.
func TestTurnCutOffIsClearedByTheNextSweep(t *testing.T) {
	transport := &fakeProgressTransport{}
	router := newBlockingRouter()
	slackAPI := &slackRecorder{}
	consumer := newProgressIntegrationConsumer(t, transport, router, slackAPI.handler)
	store := consumer.working.(*fakeWorkingThreads)

	parent, loseLeadership := context.WithCancel(context.Background())
	done := make(chan struct{})
	runBlockingTurn(consumer, parent, done)
	<-router.started
	waitForLen(t, func() int { return len(transport.statusValues()) }, 1, "status set")
	loseLeadership()
	<-done

	methods, _ := slackAPI.calls()
	assert.NotContains(t, methods, "/chat.postMessage", "a cut-off turn cannot post")
	_, finishes := store.snapshot()
	assert.Equal(t, []fakeFinish{{cleared: false, finished: false}}, finishes)

	next := newProgressIntegrationConsumer(t, transport, nil, slackAPI.handler)
	next.working = store
	require.NoError(t, next.sweepWorkingThreads(context.Background()))
	require.NoError(t, next.sweepWorkingThreads(context.Background()))
	methods, texts := slackAPI.calls()
	var notices []string
	for i, method := range methods {
		if method == "/chat.postMessage" {
			notices = append(notices, texts[i])
		}
	}
	assert.Equal(t, []string{interruptedNotice}, notices, "the thread is told once")
	statuses := transport.statusValues()
	assert.Equal(t, slackgo.AgentSessionStatusActive, statuses[len(statuses)-1])
	rows, _ := store.snapshot()
	assert.Empty(t, rows)
}

// A turn that answered but could not confirm the clear is set back to active
// without a notice.
func TestSweepClearsAFinishedTurnQuietly(t *testing.T) {
	transport := &fakeProgressTransport{}
	slackAPI := &slackRecorder{}
	consumer := newProgressIntegrationConsumer(t, transport, nil, slackAPI.handler)
	store := consumer.working.(*fakeWorkingThreads)
	holder := uuid.New()
	require.NoError(t, store.Begin(context.Background(), "C_TEST", "100.000", holder))
	require.NoError(t, store.Finish(context.Background(), "C_TEST", "100.000", holder, false, true))
	store.rows[sessionThread{"C_TEST", "100.000"}].expired = true

	require.NoError(t, consumer.sweepWorkingThreads(context.Background()))
	methods, _ := slackAPI.calls()
	assert.NotContains(t, methods, "/chat.postMessage")
	assert.Equal(t, []string{slackgo.AgentSessionStatusActive}, transport.statusValues())
	rows, _ := store.snapshot()
	assert.Empty(t, rows)
}

// Without a record of the thread, a turn shows no status: a crash would
// otherwise leave the thread working for Slack's hour.
func TestNoStatusWithoutAWorkingRecord(t *testing.T) {
	transport := &fakeProgressTransport{}
	router := &progressIntegrationRouter{execute: func(ask.TurnRequest) (ask.TurnResult, error) {
		return ask.TurnResult{Reply: "the answer", AssistantTurn: 1}, nil
	}}
	slackAPI := &slackRecorder{}
	consumer := newProgressIntegrationConsumer(t, transport, router, slackAPI.handler)
	consumer.working = nil
	runProgressIntegrationTurn(consumer)
	assert.Empty(t, transport.statusValues())
	methods, _ := slackAPI.calls()
	assert.Contains(t, methods, "/chat.postMessage", "the answer is still posted")
}

func getWorkingThreadsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), slackTestDBURL(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(context.Background(), `TRUNCATE slack_working_threads`)
	require.NoError(t, err)
	return pool
}

// A socket that fails ends the run while the leader context lives: the
// working-thread sweep stops with it rather than holding the run open.
func TestRunReturnsWhenTheSocketFailsDespiteTheSweep(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user_id":"U_DIDEROT","team_id":"T1"}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		}
	}))
	t.Cleanup(server.Close)
	c := NewConsumer(nil, nil, "xapp-test", "xoxb-test", "example.slack.com", slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	c.api = slackgo.New("xoxb-test", slackgo.OptionAppLevelToken("xapp-test"), slackgo.OptionAPIURL(server.URL+"/"))
	c.working = newFakeWorkingThreads()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case err := <-done:
		assert.Error(t, err)
		assert.NoError(t, ctx.Err(), "the run ended on the socket's error, not the context")
	case <-time.After(20 * time.Second):
		t.Fatal("the run did not return after the socket failed")
	}
}

// expireNow ends a working thread's row, as time would.
func expireNow(t *testing.T, pool *pgxpool.Pool, threadTS string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE slack_working_threads SET expires_at = now() WHERE thread_ts = $1`, threadTS)
	require.NoError(t, err)
}

func TestPgWorkingThreads(t *testing.T) {
	pool := getWorkingThreadsPool(t)
	ctx := context.Background()
	store := askchat.PGWorkingThreads{Pool: pool, Surface: ask.AskRunSurfaceSlack}
	first, second := uuid.New(), uuid.New()

	require.NoError(t, store.Begin(ctx, "C_TEST", "100.000", first))
	stop, held, err := store.Renew(ctx, "C_TEST", "100.000", first)
	require.NoError(t, err)
	assert.True(t, held)
	assert.False(t, stop)
	require.NoError(t, store.RequestStop(ctx, "C_TEST", "100.000"))
	stop, _, err = store.Renew(ctx, "C_TEST", "100.000", first)
	require.NoError(t, err)
	assert.True(t, stop, "the stop request reaches the holder")

	// A later turn of the same thread takes the record over.
	require.NoError(t, store.Begin(ctx, "C_TEST", "100.000", second))
	_, held, err = store.Renew(ctx, "C_TEST", "100.000", first)
	require.NoError(t, err)
	assert.False(t, held)
	expired, err := store.Expired(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired, "a running turn's record has not expired")

	// A turn cut off before it said anything expires at once, unfinished.
	require.NoError(t, store.Finish(ctx, "C_TEST", "100.000", second, false, false))
	expired, err = store.Expired(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired, "a cut-off turn's processing call may still land: it settles first")
	expireNow(t, pool, "100.000")
	expired, err = store.Expired(ctx)
	require.NoError(t, err)
	require.Equal(t, []workingThread{{ChannelID: "C_TEST", ThreadTS: "100.000", Holder: second}}, expired)
	claimed, notify, err := store.Claim(ctx, expired[0])
	require.NoError(t, err)
	assert.True(t, claimed)
	assert.True(t, notify, "a turn that never finished is reported")
	claimed, _, err = store.Claim(ctx, expired[0])
	require.NoError(t, err)
	assert.False(t, claimed, "one sweep alone takes the row")
	expired, err = store.Expired(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired, "a claimed row waits before it is tried again")
	require.NoError(t, store.Release(ctx, workingThread{ChannelID: "C_TEST", ThreadTS: "100.000", Holder: second}))
	var left int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM slack_working_threads WHERE thread_ts = '100.000'`).Scan(&left))
	assert.Zero(t, left)

	// A row its turn renewed after the sweep listed it is left alone.
	require.NoError(t, store.Begin(ctx, "C_TEST", "150.000", first))
	_, err = pool.Exec(ctx, `UPDATE slack_working_threads SET expires_at = now() WHERE thread_ts = '150.000'`)
	require.NoError(t, err)
	expired, err = store.Expired(ctx)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	_, held, err = store.Renew(ctx, "C_TEST", "150.000", first)
	require.NoError(t, err)
	require.True(t, held)
	claimed, _, err = store.Claim(ctx, expired[0])
	require.NoError(t, err)
	assert.False(t, claimed, "a turn still running keeps its status")

	// A stopped turn whose process died is cleared without a notice.
	require.NoError(t, store.RequestStop(ctx, "C_TEST", "150.000"))
	require.NoError(t, store.Finish(ctx, "C_TEST", "150.000", first, false, false))
	expireNow(t, pool, "150.000")
	expired, err = store.Expired(ctx)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	claimed, notify, err = store.Claim(ctx, expired[0])
	require.NoError(t, err)
	assert.True(t, claimed)
	assert.False(t, notify, "its asker stopped it")

	// A finished turn that could not confirm the clear settles first.
	require.NoError(t, store.Begin(ctx, "C_TEST", "200.000", first))
	require.NoError(t, store.Finish(ctx, "C_TEST", "200.000", first, false, true))
	expired, err = store.Expired(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired, "the row waits out workingThreadSettle")

	// A cleared turn leaves nothing.
	require.NoError(t, store.Begin(ctx, "C_TEST", "300.000", first))
	require.NoError(t, store.Finish(ctx, "C_TEST", "300.000", first, true, true))
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM slack_working_threads WHERE thread_ts = '300.000'`).Scan(&count))
	assert.Zero(t, count)
}
