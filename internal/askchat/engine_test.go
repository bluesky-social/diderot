package askchat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
)

// fakeSurface records what the engine did in it.
type fakeSurface struct {
	mu        sync.Mutex
	limit     int
	marks     []Mark
	replies   []string
	chunks    []string
	lastFlags []bool
	uploads   []File
	cleared   []Conversation
	progress  *fakeProgress
	failChunk int // 1-based chunk that fails; 0 none
}

func (s *fakeSurface) Name() ask.AskRunSurface { return "fake" }
func (s *fakeSurface) Asker(context.Context, string) *ask.AskerInfo {
	return &ask.AskerInfo{Email: "asker@example.com"}
}
func (s *fakeSurface) MarkQuestion(_ context.Context, _, _ string, mark Mark) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks = append(s.marks, mark)
}
func (s *fakeSurface) Progress(Question) Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = &fakeProgress{}
	return s.progress
}
func (s *fakeSurface) ClearProgress(_ context.Context, conv Conversation) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared = append(s.cleared, conv)
	return true, nil
}
func (s *fakeSurface) PostReply(_ context.Context, _ Conversation, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies = append(s.replies, text)
	return nil
}
func (s *fakeSurface) Sanitize(text string) string {
	return strings.ReplaceAll(text, "@everyone", "everyone")
}
func (s *fakeSurface) ChunkLimit() int { return s.limit }
func (s *fakeSurface) PostChunk(_ context.Context, _ Conversation, chunk string, opts ChunkOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failChunk == len(s.chunks)+1 {
		return "", errors.New("post failed")
	}
	s.chunks = append(s.chunks, chunk)
	s.lastFlags = append(s.lastFlags, opts.Last)
	return fmt.Sprintf("m%d", len(s.chunks)), nil
}
func (s *fakeSurface) Upload(_ context.Context, _ Conversation, file File) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = append(s.uploads, file)
	return "f1", nil
}

type fakeProgress struct {
	mu                       sync.Mutex
	started, stopped, finish bool
}

func (p *fakeProgress) Start() { p.mu.Lock(); p.started = true; p.mu.Unlock() }
func (p *fakeProgress) Stop()  { p.mu.Lock(); p.stopped = true; p.mu.Unlock() }
func (p *fakeProgress) Finish(context.Context) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finish = true
	return true
}

// fakeRouter answers with result, or blocks until its context ends.
type fakeRouter struct {
	result  ask.TurnResult
	err     error
	block   bool
	started chan struct{}
	req     ask.TurnRequest
	// artifacts
	delivered map[uuid.UUID]bool
}

func (r *fakeRouter) LookupOrCreateSession(context.Context, Conversation, string) (uuid.UUID, error) {
	return uuid.MustParse("00000000-0000-0000-0000-000000000001"), nil
}
func (r *fakeRouter) ExecuteTurn(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error) {
	r.req = req
	if r.started != nil {
		close(r.started)
	}
	if r.block {
		<-ctx.Done()
		return ask.TurnResult{}, ctx.Err()
	}
	return r.result, r.err
}
func (r *fakeRouter) ReadArtifact(context.Context, uuid.UUID, uuid.UUID) (ask.ArtifactRecord, []byte, error) {
	return ask.ArtifactRecord{ContentHash: "h"}, []byte("png"), nil
}
func (r *fakeRouter) ArtifactDeliveryExists(_ context.Context, _ uuid.UUID, _ int, id uuid.UUID, _, _ string) (bool, error) {
	return r.delivered[id], nil
}
func (r *fakeRouter) RecordArtifactDelivery(_ context.Context, _ uuid.UUID, _ int, id uuid.UUID, _, _, _, status, _ string) error {
	if status == "delivered" {
		if r.delivered == nil {
			r.delivered = map[uuid.UUID]bool{}
		}
		r.delivered[id] = true
	}
	return nil
}

func question() Question {
	return Question{Conversation: Conversation{"C1", "T1"}, MessageID: "Q1", AskerID: "U1", Text: "why?",
		AllowedScopes: []string{"public"}}
}

func TestRunTurnAnswersInChunksAndMarksTheQuestion(t *testing.T) {
	surface := &fakeSurface{limit: 300}
	artifact := uuid.New()
	router := &fakeRouter{result: ask.TurnResult{Reply: strings.Repeat("line of the answer @everyone\n", 25), AssistantTurn: 1,
		IncompleteReason: ask.IncompleteMaxTokens, Artifacts: []ask.ArtifactDescriptor{{ID: artifact, Title: "Latency"}}}}
	e := &Engine{Surface: surface, Ask: router, Turns: &Turns{}, Working: &memWorking{}}
	e.RunTurn(context.Background(), question())

	assert.Equal(t, ask.AskRunSurface("fake"), router.req.Surface)
	assert.Equal(t, []string{"public"}, router.req.AllowedScopes)
	require.Greater(t, len(surface.chunks), 1, "a long answer is split")
	for _, c := range surface.chunks {
		assert.LessOrEqual(t, len(c), 300)
		assert.NotContains(t, c, "@everyone", "the surface sanitizes the answer")
	}
	assert.Contains(t, surface.chunks[len(surface.chunks)-1], ask.IncompleteNotice(ask.IncompleteMaxTokens))
	assert.Equal(t, []bool{false, true}, surface.lastFlags[len(surface.lastFlags)-2:], "only the last chunk is the last")
	require.Len(t, surface.uploads, 1)
	assert.Equal(t, "Latency", surface.uploads[0].Title)
	assert.Equal(t, []Mark{MarkAnswered}, surface.marks)
	assert.True(t, surface.progress.started && surface.progress.stopped && surface.progress.finish)
	assert.Empty(t, surface.replies)

	// Delivering the same artifacts again posts nothing twice.
	require.NoError(t, e.DeliverArtifacts(context.Background(), Conversation{"C1", "T1"}, "fake", uuid.MustParse("00000000-0000-0000-0000-000000000001"), 1, router.result.Artifacts))
	assert.Len(t, surface.uploads, 1)
}

func TestRunTurnReportsAnErrorWithItsSession(t *testing.T) {
	surface := &fakeSurface{limit: 300}
	e := &Engine{Surface: surface, Ask: &fakeRouter{err: context.DeadlineExceeded}, Turns: &Turns{}}
	q := question()
	q.KnownSession = true
	e.RunTurn(context.Background(), q)
	require.Len(t, surface.replies, 1)
	assert.Equal(t, "sorry, the model timed out before finishing; please try again\nDiderot session ID: 00000000-0000-0000-0000-000000000001", surface.replies[0])
	assert.Empty(t, surface.chunks)
	assert.Empty(t, surface.marks, "a follow-up's question is not marked")
}

func TestStopStopsARunningTurnSilently(t *testing.T) {
	surface := &fakeSurface{limit: 300}
	router := &fakeRouter{block: true, started: make(chan struct{})}
	e := &Engine{Surface: surface, Ask: router, Turns: &Turns{}, Working: &memWorking{}}
	done := make(chan struct{})
	go func() { e.RunTurn(context.Background(), question()); close(done) }()
	<-router.started
	assert.Equal(t, 1, e.Stop(context.Background(), Conversation{"C1", "T1"}))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn did not stop")
	}
	assert.Empty(t, surface.replies, "a stopped turn posts nothing")
	assert.Empty(t, surface.chunks)
	assert.Equal(t, []Mark{MarkStopped}, surface.marks)
}

func TestDeliverStopsAtTheFirstFailedChunk(t *testing.T) {
	surface := &fakeSurface{limit: 50, failChunk: 2}
	e := &Engine{Surface: surface, Ask: &fakeRouter{}, Turns: &Turns{}}
	_, err := e.Deliver(context.Background(), Conversation{"C1", "T1"}, "", 0, strings.Repeat("0123456789\n", 20), "", "")
	require.ErrorContains(t, err, "post answer chunk 2/")
	assert.Len(t, surface.chunks, 1, "the chunk that landed stays")
}

func TestSweepTellsAnInterruptedConversationAndClearsIt(t *testing.T) {
	surface := &fakeSurface{limit: 300}
	store := &sweepStore{rows: []WorkingThread{{ChannelID: "C1", ThreadTS: "T1", Holder: uuid.New()}}}
	e := &Engine{Surface: surface, Ask: &fakeRouter{}, Turns: &Turns{}, Working: store}
	n, err := e.Sweep(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{InterruptedNotice}, surface.replies)
	assert.Equal(t, []Conversation{{"C1", "T1"}}, surface.cleared)
	assert.True(t, store.released)
}

type sweepStore struct {
	memWorking
	rows     []WorkingThread
	released bool
}

func (s *sweepStore) Expired(context.Context) ([]WorkingThread, error) { return s.rows, nil }
func (s *sweepStore) Claim(context.Context, WorkingThread) (bool, bool, error) {
	return true, true, nil
}
func (s *sweepStore) Release(context.Context, WorkingThread) error { s.released = true; return nil }
