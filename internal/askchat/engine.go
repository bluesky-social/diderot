// Package askchat is Ask over chat: the engine that turns a question asked
// in a chat service into an Ask turn and delivers its answer, shared by
// every chat surface. A surface (Slack, Discord) implements Surface with what
// is specific to its service: posting, reactions, the progress affordance,
// uploads. The engine never asks which surface it is driving.
package askchat

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/worker"
)

// TurnTimeout bounds one chat turn from its start to its answer, unless the
// question sets a shorter bound.
const TurnTimeout = 15 * time.Minute

// Conversation is where a turn answers: a channel, and the thread in it
// that is the turn's session.
type Conversation struct {
	Place, Thread string
}

// Mark is a state of the question message a surface shows, by reacting to
// it or otherwise.
type Mark int

const (
	// MarkSeen acknowledges the question before the turn starts.
	MarkSeen Mark = iota
	// MarkAnswered replaces MarkSeen once the answer is delivered.
	MarkAnswered
	// MarkStopped takes MarkSeen off a turn its asker stopped.
	MarkStopped
)

// Surface is a chat service Ask answers in: what the engine needs of it.
type Surface interface {
	// Name is the surface's Ask run surface, recorded on the turn and on
	// artifact deliveries.
	Name() ask.AskRunSurface
	// Asker is the per-turn identity of the person with ID askerID, or nil
	// when it can't be resolved.
	Asker(ctx context.Context, askerID string) *ask.AskerInfo
	// MarkQuestion shows mark on the question message. Best effort.
	MarkQuestion(ctx context.Context, place, messageID string, mark Mark)
	// Progress is the affordance that shows q's turn working. It must not
	// be nil; a surface without one returns NoProgress.
	Progress(q Question) Progress
	// ClearProgress takes a working affordance down for a thread whose turn
	// ended elsewhere (a stop, or a turn that died). ok is false when the
	// surface can't, and the sweep then leaves the thread to expire.
	ClearProgress(ctx context.Context, conv Conversation) (ok bool, err error)
	// PostReply posts a short message the engine composed (an error or a
	// notice) into conv, in the surface's dialect and without pings.
	PostReply(ctx context.Context, conv Conversation, text string) error
	// Sanitize makes model output safe to post: no pings, no broadcasts.
	Sanitize(text string) string
	// ChunkLimit is the most an answer chunk may hold, in UTF-8 bytes.
	ChunkLimit() int
	// PostChunk posts one answer chunk and returns its message ID.
	PostChunk(ctx context.Context, conv Conversation, chunk string, opts ChunkOptions) (string, error)
	// Upload attaches a file to conv and returns the service's ID for it.
	Upload(ctx context.Context, conv Conversation, file File) (string, error)
}

// ChunkOptions say what kind of answer chunk is posted.
type ChunkOptions struct {
	// Last is the answer's final chunk.
	Last bool
	// Marked is a chunk carrying a delivery marker (MarkedDelivery).
	Marked bool
}

// MarkedDelivery is a surface that can find chunks of an earlier delivery
// by their markers, so a delivery retried after an ambiguous failure posts
// nothing twice.
type MarkedDelivery interface {
	// DeliveredChunks maps each marker found in conv's messages to the
	// message carrying it.
	DeliveredChunks(ctx context.Context, conv Conversation, marker string) (map[string]string, error)
}

// File is an artifact a turn delivers as an attachment.
type File struct {
	Name, Title, AltText string
	Data                 []byte
}

// Progress shows one turn working.
type Progress interface {
	// Start shows it, off the turn's critical path.
	Start()
	// Stop keeps a Start that has not run from running, without waiting for
	// one in flight.
	Stop()
	// Finish takes it down once the turn has said what it had to. cleared
	// says it is confirmed down; when it is not, the working record lets
	// the sweep finish the job.
	Finish(ctx context.Context) (cleared bool)
}

// NoProgress is the Progress of a surface that shows none.
type NoProgress struct{}

func (NoProgress) Start()                                {}
func (NoProgress) Stop()                                 {}
func (NoProgress) Finish(context.Context) (cleared bool) { return true }

// Router is the Ask service a turn runs on.
type Router interface {
	// LookupOrCreateSession is conv's session; askerEmail becomes the
	// owner of a session this creates.
	LookupOrCreateSession(ctx context.Context, conv Conversation, askerEmail string) (uuid.UUID, error)
	ExecuteTurn(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error)
}

// ArtifactRouter is a Router that keeps a turn's artifacts.
type ArtifactRouter interface {
	ReadArtifact(ctx context.Context, sessionID, artifactID uuid.UUID) (ask.ArtifactRecord, []byte, error)
	ArtifactDeliveryExists(ctx context.Context, sessionID uuid.UUID, assistantTurn int, artifactID uuid.UUID, surface, target string) (bool, error)
	RecordArtifactDelivery(ctx context.Context, sessionID uuid.UUID, assistantTurn int, artifactID uuid.UUID, surface, target, externalID, status, failure string) error
}

// ThreadSessions is an Ask service whose chat sessions are keyed by a
// channel and a thread: ask_slack_sessions, named for the first surface,
// whose keys are any surface's IDs.
type ThreadSessions interface {
	// LookupOrCreateSlackSession is the session of channelID's threadTS;
	// askerEmail owns a session this creates.
	LookupOrCreateSlackSession(ctx context.Context, channelID, threadTS, askerEmail string) (uuid.UUID, error)
	ExecuteTurn(ctx context.Context, req ask.TurnRequest) (ask.TurnResult, error)
}

// NewRouter is s as the engine's Router: a conversation's session is its
// place's thread. It is an ArtifactRouter when s keeps artifacts.
func NewRouter(s ThreadSessions) Router {
	if artifacts, ok := s.(ArtifactRouter); ok {
		return artifactRouter{threadRouter{s}, artifacts}
	}
	return threadRouter{s}
}

type threadRouter struct{ ThreadSessions }

func (r threadRouter) LookupOrCreateSession(ctx context.Context, conv Conversation, askerEmail string) (uuid.UUID, error) {
	return r.LookupOrCreateSlackSession(ctx, conv.Place, conv.Thread, askerEmail)
}

type artifactRouter struct {
	threadRouter
	ArtifactRouter
}

// Question is one question routed to a turn.
type Question struct {
	Conversation
	// MessageID is the question's message, which MarkQuestion marks.
	MessageID string
	AskerID   string
	// Text is the question as the model reads it; Title names the session
	// where the surface shows one.
	Text, Title string
	// ThreadContext and ContextThrough are the conversation's messages the
	// session has not seen, and how far they reach.
	ThreadContext  string
	ContextThrough time.Time
	// KnownSession says the conversation already has a session: the
	// question was not marked seen, and is not marked answered.
	KnownSession bool
	// Asker is the asker's identity, or nil for the engine to resolve.
	Asker         *ask.AskerInfo
	AllowedScopes []string
	CorpusOnly    bool
	// Timeout, when set, bounds the turn instead of TurnTimeout: a
	// surface whose answer must land within a window (a Discord
	// interaction's token) sets it.
	Timeout time.Duration
	// OnSession, when set, hears the session the turn runs in, or "" when
	// none could be found or made.
	OnSession func(sessionID string)
	// BeforeTurn, when set, runs once the session is known and before the
	// model does.
	BeforeTurn func(ctx context.Context, sessionID string)
}

// Engine runs chat turns for one surface.
type Engine struct {
	Surface Surface
	Ask     Router
	// Answers registers delivered chunks for feedback; nil registers none.
	Answers *AnswerStore
	// Working records the conversations a turn is working in; nil shows no
	// progress.
	Working WorkingStore
	// Turns are the turns running in this process, for Stop.
	Turns  *Turns
	Logger *slog.Logger
}

// RunTurn drives q's turn to its end:
//
//  1. Find or make the conversation's session.
//  2. Record the conversation as working, then show progress.
//  3. Run the turn. A stop (Stop, here or on another replica) cancels it
//     with ask.ErrTurnStopped; a stopped turn posts nothing.
//  4. Keep a progress call that has not started from starting.
//  5. Post the answer, or the turn's error.
//  6. Take the progress down and settle the working record.
//  7. Mark the question answered, on a first turn.
//
// parent is the surface's run context. Delivery and cleanup use contexts
// bounded from it, so an answer the model finished is still posted after
// the turn's own deadline, but not after the run ends.
func (e *Engine) RunTurn(parent context.Context, q Question) {
	timeout := TurnTimeout
	if q.Timeout > 0 {
		timeout = q.Timeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// turnCtx is the model's: a stop cancels it with ask.ErrTurnStopped.
	// Delivery uses ctx, so an answer the model has finished is still
	// posted.
	turnCtx, stopTurn := context.WithCancelCause(ctx)
	defer stopTurn(nil)
	holder := uuid.New()
	e.Turns.add(q.Place, q.Thread, holder, stopTurn)
	defer e.Turns.remove(q.Place, q.Thread, holder)

	// The asker is resolved before the session so that a session this turn
	// creates is owned by them.
	askerInfo := q.Asker
	if askerInfo == nil {
		askerInfo = e.Surface.Asker(ctx, q.AskerID)
	}
	var askerEmail string
	if askerInfo != nil {
		askerEmail = askerInfo.Email
	}
	sessionID, err := e.Ask.LookupOrCreateSession(ctx, q.Conversation, askerEmail)
	if err != nil {
		if q.OnSession != nil {
			q.OnSession("")
		}
		e.logger().Warn("ask: session lookup-or-create failed",
			"channel", q.Place, "session_thread", q.Thread, "err", err)
		e.ReplyError(parent, q.Conversation, "", "I couldn't start this conversation; please try again shortly")
		return
	}
	sessionIDText := sessionID.String()
	if q.OnSession != nil {
		q.OnSession(sessionIDText)
	}

	// The conversation is recorded before progress shows: without the
	// record, a turn whose process died would leave it showing.
	progress := e.Surface.Progress(q)
	working, workingErr := beginWorking(ctx, e.Working, e.logger(), q.Place, q.Thread, holder, stopTurn)
	if workingErr != nil {
		e.logger().Warn("ask: working thread not recorded; answering without a status",
			"channel", q.Place, "thread_ts", q.Thread, "err", workingErr)
	} else {
		progress.Start()
	}
	// finished is whether the turn said what it had to: its answer, its
	// error, or nothing because its asker stopped it. A turn cut off by the
	// run ending did not, and the next sweep tells the conversation.
	finished := false
	cleanupDone := false
	cleanup := func() {
		if cleanupDone {
			return
		}
		cleanupDone = true
		// Cleanup is bounded from the run, not the turn's deadline: an
		// ordinary timeout still cleans up, the run ending does not.
		cleanupCtx, cleanupCancel := context.WithTimeout(parent, 10*time.Second)
		defer cleanupCancel()
		cleared := false
		if parent.Err() == nil {
			// progress.Finish posts to the surface (Slack/Discord API
			// calls) and can itself panic. working.finish below is what
			// stops the working-record renewal goroutine and releases or
			// expires the row (see workingTurn.renew's own recovery for
			// why that goroutine's context outlives ctx); a panic here
			// must not skip it, or the renewal loop and its row leak for
			// as long as the replica runs.
			func() {
				defer worker.Recover(e.logger(), "ask-progress-finish")
				cleared = progress.Finish(cleanupCtx)
			}()
		}
		if working != nil {
			working.finish(cleanupCtx, cleared, finished)
		}
	}
	defer cleanup()
	// A panic anywhere from here on (BeforeTurn, the turn itself, answer
	// delivery) must still end this one turn the way an ordinary failure
	// does -- the asker gets a reply and the working record above is
	// released by the deferred cleanup -- rather than crashing the whole
	// process and leaving every other Slack/Discord turn and the
	// HTTP/MCP server down with it. AskService.ExecuteTurn already
	// recovers panics from the turn it runs; this is the backstop for
	// everything else RunTurn itself does. !finished guards against
	// double-posting a failure reply over an answer that had already
	// landed (a panic in the post-delivery artifact/mark steps).
	defer worker.Recover(e.logger(), "ask-turn", func() {
		if !finished {
			e.ReplyError(parent, q.Conversation, sessionIDText, "something went wrong while answering; please try again")
			finished = parent.Err() == nil
		}
	})

	if q.BeforeTurn != nil {
		q.BeforeTurn(ctx, sessionIDText)
	}
	turn, runErr := e.Ask.ExecuteTurn(turnCtx, ask.TurnRequest{
		SessionID:      sessionID,
		UserInput:      q.Text,
		Surface:        e.Surface.Name(),
		Asker:          askerInfo,
		ThreadContext:  q.ThreadContext,
		ContextThrough: q.ContextThrough,
		AllowedScopes:  append([]string(nil), q.AllowedScopes...),
		CorpusOnly:     q.CorpusOnly,
	})
	// Answer and error delivery come first; a status call that has not
	// started never will.
	progress.Stop()

	if runErr != nil && ask.TurnStopped(turnCtx) {
		e.logger().Info("ask: turn stopped by its asker",
			"channel", q.Place, "thread_ts", q.Thread, "session_id", sessionID)
		finished = true
		cleanup()
		if !q.KnownSession {
			e.Surface.MarkQuestion(ctx, q.Place, q.MessageID, MarkStopped)
		}
		return
	}
	if runErr != nil {
		e.logger().Warn("ask: run turn failed",
			"channel", q.Place, "thread_ts", q.Thread, "session_id", sessionID, "err", runErr)
		e.ReplyError(parent, q.Conversation, sessionIDText, ErrorText(runErr))
		finished = parent.Err() == nil
		cleanup()
		return
	}
	if _, err := e.Deliver(ctx, q.Conversation, sessionIDText, turn.AssistantTurn, turn.Reply, turn.IncompleteReason, ""); err != nil {
		e.logger().Warn("ask: answer post failed",
			"channel", q.Place, "thread_ts", q.Thread, "err", err)
		// Earlier chunks that landed stay visible.
		e.ReplyError(parent, q.Conversation, sessionIDText, "I couldn't post the full answer; please try again")
		finished = parent.Err() == nil
		cleanup()
		return
	}
	// Progress comes down only after every chunk is confirmed.
	finished = true
	cleanup()
	if err := e.DeliverArtifacts(ctx, q.Conversation, string(e.Surface.Name()), sessionID, turn.AssistantTurn, turn.Artifacts); err != nil {
		e.logger().Warn("ask: panel artifact delivery failed", "channel", q.Place, "thread_ts", q.Thread, "session_id", sessionID, "err", err)
		e.PostNotice(ctx, q.Conversation, "the answer is complete, but I couldn't attach one or more selected panel images. The render remains available in the Ask session.")
	}
	if !q.KnownSession {
		e.Surface.MarkQuestion(ctx, q.Place, q.MessageID, MarkAnswered)
	}
}

// ReplyError posts a brief error into conv, bounded from parent so it
// outlives the turn's deadline but not its run.
func (e *Engine) ReplyError(parent context.Context, conv Conversation, sessionID, msg string) {
	text := "sorry, " + msg
	if sessionID != "" {
		text += "\nDiderot session ID: " + sessionID
	}
	errCtx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if err := e.Surface.PostReply(errCtx, conv, text); err != nil {
		e.logger().Warn("ask: post error reply failed", "channel", conv.Place, "err", err)
	}
}

// PostNotice posts a best-effort notice into conv; a failure is logged.
func (e *Engine) PostNotice(ctx context.Context, conv Conversation, text string) {
	if err := e.Surface.PostReply(ctx, conv, text); err != nil {
		e.logger().Warn("ask: thread notice post failed",
			"channel", conv.Place, "thread_ts", conv.Thread, "err", err)
	}
}

// Stop stops conv's turn: at once when it runs here, and through the
// working record when it runs on another replica. It returns how many turns
// it stopped here.
func (e *Engine) Stop(ctx context.Context, conv Conversation) int {
	stopped := e.Turns.stop(conv.Place, conv.Thread)
	if e.Working != nil {
		if err := e.Working.RequestStop(ctx, conv.Place, conv.Thread); err != nil {
			e.logger().Warn("ask: stop request not recorded", "channel", conv.Place, "thread_ts", conv.Thread, "err", err)
		}
	}
	return stopped
}

func (e *Engine) logger() *slog.Logger {
	if e.Logger == nil {
		return slog.Default()
	}
	return e.Logger
}
