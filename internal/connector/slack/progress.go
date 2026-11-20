package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/worker"
)

const (
	progressRequestTimeout = 5 * time.Second
	progressJoinGrace      = time.Second
	// sessionTitleRunes bounds the title Slack shows for a thread's agent
	// session. Slack accepts 200 characters; a sidebar shows far fewer.
	sessionTitleRunes = 80
)

type progressTimings struct {
	requestTimeout time.Duration
	joinGrace      time.Duration
}

var defaultProgressTimings = progressTimings{
	requestTimeout: progressRequestTimeout,
	joinGrace:      progressJoinGrace,
}

// slackProgressAPI is the exact slack-go surface used by Ask's agent session
// status. Ask progress must not post or update a Slack message of its own.
type slackProgressAPI interface {
	SetAgentSessionStatusContext(ctx context.Context, params slackgo.AgentSessionSetStatusParameters) (*slackgo.AgentSessionSetStatusResponse, error)
}

type progressCallResult struct {
	err       error
	ambiguous bool
}

// sessionStatus is one agents.sessions.setStatus call: the thread, the
// status, and, for the call that creates the session, its title and the
// person who started it.
type sessionStatus struct {
	ChannelID, ThreadTS string
	Status              string
	Title, Initiator    string
}

type progressTransport interface {
	SetStatus(ctx context.Context, status sessionStatus) progressCallResult
}

type slackProgressTransport struct {
	api     slackProgressAPI
	limiter *Limiter
}

func (t *slackProgressTransport) SetStatus(ctx context.Context, status sessionStatus) progressCallResult {
	if t == nil || t.api == nil {
		return progressCallResult{err: errors.New("slack progress api not configured")}
	}
	params := slackgo.AgentSessionSetStatusParameters{
		ChannelID:       status.ChannelID,
		ThreadTS:        status.ThreadTS,
		Status:          status.Status,
		Title:           status.Title,
		InitiatorUserID: status.Initiator,
	}
	return t.call(ctx, methodSetSessionStatus, func() error {
		_, err := t.api.SetAgentSessionStatusContext(ctx, params)
		return err
	})
}

// call retries confirmed 429 responses and records whether a final transport
// error leaves the remote status ambiguous.
func (t *slackProgressTransport) call(ctx context.Context, method string, call func() error) progressCallResult {
	for {
		if t.limiter != nil {
			if err := t.limiter.Wait(ctx, method); err != nil {
				return progressCallResult{err: err}
			}
		}
		err := call()
		switch {
		case err == nil:
			if t.limiter != nil {
				t.limiter.RecordOk(method)
			}
			return progressCallResult{}
		case isRateLimited(err):
			var rateErr *slackgo.RateLimitedError
			_ = errors.As(err, &rateErr)
			if t.limiter != nil {
				t.limiter.Record429(method)
			}
			if sleepErr := sleepCtx(ctx, rateErr.RetryAfter); sleepErr != nil {
				return progressCallResult{err: sleepErr}
			}
		case isConfirmedSlackAPIError(err):
			if t.limiter != nil {
				t.limiter.RecordOther(method)
			}
			return progressCallResult{err: err}
		default:
			if t.limiter != nil {
				t.limiter.RecordOther(method)
			}
			return progressCallResult{err: err, ambiguous: true}
		}
	}
}

func isConfirmedSlackAPIError(err error) bool {
	var value slackgo.SlackErrorResponse
	if errors.As(err, &value) {
		return true
	}
	var pointer *slackgo.SlackErrorResponse
	return errors.As(err, &pointer) && pointer != nil
}

type statusJoinResult uint8

const (
	statusJoinConfirmed statusJoinResult = iota
	statusJoinAmbiguous
	statusJoinTimeout
)

// slackProgress owns only the thread's Slack agent session status. It
// never creates a message or renders tool steps into the conversation.
//
// It sets the session to processing once, when the turn starts: Slack keeps
// a session processing for an hour, longer than any turn runs. Clear sets it
// back to active, which Slack does not do on its own when the answer is
// posted.
type slackProgress struct {
	transport progressTransport
	metrics   *consumerMetrics
	logger    *slog.Logger
	timings   progressTimings
	start     sessionStatus

	statusCtx       context.Context
	cancelStatus    context.CancelFunc
	statusStop      chan struct{}
	statusDone      chan struct{}
	statusStartOnce sync.Once
	statusStopOnce  sync.Once
	statusMu        sync.Mutex
	statusStarted   bool
	statusFinalized bool
	statusAmbiguous bool
	statusJoined    statusJoinResult
}

// newSlackProgress drives channelID/threadTS's agent session. title and
// initiator name the session when this turn creates it; Slack ignores them
// for a session that already exists.
func newSlackProgress(transport progressTransport, metrics *consumerMetrics, logger *slog.Logger, channelID, threadTS, title, initiator string, timings progressTimings) *slackProgress {
	if logger == nil {
		logger = slog.Default()
	}
	statusCtx, cancelStatus := context.WithCancel(context.Background())
	return &slackProgress{
		transport: transport,
		metrics:   metrics,
		logger:    logger,
		timings:   timings,
		start: sessionStatus{
			ChannelID: channelID, ThreadTS: threadTS, Status: slackgo.AgentSessionStatusProcessing,
			Title: title, Initiator: initiator,
		},
		statusCtx:    statusCtx,
		cancelStatus: cancelStatus,
		statusStop:   make(chan struct{}),
		statusDone:   make(chan struct{}),
		statusJoined: statusJoinTimeout,
	}
}

func (p *slackProgress) Start() {
	p.statusStartOnce.Do(func() {
		p.statusMu.Lock()
		if p.statusFinalized {
			p.statusMu.Unlock()
			return
		}
		select {
		case <-p.statusStop:
			p.statusFinalized = true
			p.statusJoined = statusJoinConfirmed
			p.statusMu.Unlock()
			return
		default:
		}
		p.statusStarted = true
		p.statusMu.Unlock()
		go p.runStatus()
	})
}

// runStatus sets the session to processing, off the turn's critical path.
func (p *slackProgress) runStatus() {
	defer close(p.statusDone)
	// A panic here (the status transport call, metric recording) must not
	// crash the whole process over a processing-indicator update; close(p.statusDone)
	// above still runs, so Finish does not wait forever. Its Discord twin
	// (asksurface.go showWorking) is recovered as "discord-ask-progress".
	defer worker.Recover(p.logger, "slack-ask-progress")
	select {
	case <-p.statusCtx.Done():
		return
	case <-p.statusStop:
		return
	default:
	}
	requestCtx, cancel := context.WithTimeout(p.statusCtx, p.timings.requestTimeout)
	result := p.transport.SetStatus(requestCtx, p.start)
	cancel()
	outcome := "set_ok"
	if result.err != nil {
		outcome = "set_error"
		p.logger.Warn("ask: agent session status failed",
			"channel", p.start.ChannelID, "thread_ts", p.start.ThreadTS, "err", result.err)
	}
	p.recordStatus(outcome)
	if result.ambiguous {
		p.statusMu.Lock()
		p.statusAmbiguous = true
		p.statusMu.Unlock()
	}
}

// Stop prevents the status call if it has not started, without waiting for
// one in flight.
func (p *slackProgress) Stop() {
	p.statusStopOnce.Do(func() { close(p.statusStop) })
}

func (p *slackProgress) Join(ctx context.Context) statusJoinResult {
	p.statusMu.Lock()
	if !p.statusStarted {
		p.statusFinalized = true
		p.statusJoined = statusJoinConfirmed
		p.statusMu.Unlock()
		return statusJoinConfirmed
	}
	p.statusMu.Unlock()
	select {
	case <-p.statusDone:
		p.statusMu.Lock()
		defer p.statusMu.Unlock()
		p.statusFinalized = true
		if p.statusAmbiguous {
			p.statusJoined = statusJoinAmbiguous
			return statusJoinAmbiguous
		}
		p.statusJoined = statusJoinConfirmed
		return statusJoinConfirmed
	case <-ctx.Done():
		p.cancelStatus()
		select {
		case <-p.statusDone:
		case <-time.After(p.timings.joinGrace):
		}
		p.statusMu.Lock()
		p.statusFinalized = true
		p.statusJoined = statusJoinTimeout
		p.statusMu.Unlock()
		p.recordStatus("join_timeout")
		p.logger.Warn("ask: agent session status join timed out",
			"channel", p.start.ChannelID, "thread_ts", p.start.ThreadTS)
		return statusJoinTimeout
	}
}

// Clear sets the session back to active. It is safe only after Join proves
// no ambiguous processing call can still land after it.
func (p *slackProgress) Clear(ctx context.Context) error {
	p.statusMu.Lock()
	joined := p.statusJoined
	started := p.statusStarted
	p.statusMu.Unlock()
	if joined != statusJoinConfirmed {
		p.recordStatus("clear_skipped_ambiguous")
		return fmt.Errorf("status clear requires confirmed join, got %d", joined)
	}
	if !started {
		return nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, p.timings.requestTimeout)
	result := p.transport.SetStatus(requestCtx, sessionStatus{
		ChannelID: p.start.ChannelID, ThreadTS: p.start.ThreadTS, Status: slackgo.AgentSessionStatusActive,
	})
	cancel()
	if result.err != nil {
		p.recordStatus("clear_error")
		return result.err
	}
	p.recordStatus("clear_ok")
	return nil
}

func (p *slackProgress) recordStatus(outcome string) {
	if p.metrics != nil && p.metrics.askStatus != nil {
		p.metrics.askStatus.WithLabelValues(outcome).Inc()
	}
}

// sessionTitle is the title Slack shows for the agent session a question
// starts: the question on one line, without the bot's mention when the
// question opens with it (opensWithMention).
func sessionTitle(question string, opensWithMention bool) string {
	words := strings.Fields(question)
	if opensWithMention && len(words) > 1 && strings.HasPrefix(words[0], "@") {
		words = words[1:]
	}
	title := strings.Join(words, " ")
	runes := []rune(title)
	if len(runes) > sessionTitleRunes {
		title = strings.TrimSpace(string(runes[:sessionTitleRunes-1])) + "…"
	}
	return title
}
