package pagerduty

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeQueue struct {
	job       *Job
	output    string
	completed string
	failure   error
	saveErr   error
}

func (queue *fakeQueue) Claim(context.Context) (*Job, error) { return queue.job, nil }
func (queue *fakeQueue) SaveSession(_ context.Context, job *Job, id uuid.UUID) error {
	job.SessionID = &id
	return queue.saveErr
}
func (queue *fakeQueue) SaveOutput(_ context.Context, job *Job, output string, assistantTurn int) error {
	queue.output = output
	job.AssistantTurn = assistantTurn
	return queue.saveErr
}
func (queue *fakeQueue) SaveNote(_ context.Context, _ *Job, _ string) error         { return nil }
func (queue *fakeQueue) SaveSlackMessage(_ context.Context, _ *Job, _ string) error { return nil }
func (queue *fakeQueue) Complete(_ context.Context, job *Job) error {
	queue.completed = job.NoteID
	return nil
}
func (queue *fakeQueue) Retry(_ context.Context, _ *Job, err error) error {
	queue.failure = err
	return nil
}

type fakeAgent struct {
	request ask.TurnRequest
	calls   int
	creates int
	err     error
}

func (agent *fakeAgent) CreateSession(context.Context) (uuid.UUID, error) {
	agent.creates++
	return uuid.New(), nil
}
func (agent *fakeAgent) ExecuteTurn(_ context.Context, request ask.TurnRequest) (ask.TurnResult, error) {
	agent.request = request
	agent.calls++
	return ask.TurnResult{Reply: "Root cause not established.", AssistantTurn: 7, IncompleteReason: ask.IncompleteContextExceeded}, agent.err
}

type fakePager struct {
	calls     int
	body      string
	err       error
	enrichErr error
}

type fakeSlack struct {
	calls int
	err   error
	body  string
}

func (delivery *fakeSlack) Deliver(_ context.Context, _ *Job, body string) (string, error) {
	delivery.calls++
	delivery.body = body
	return "1234.000001", delivery.err
}

func (pager *fakePager) Context(context.Context, string) (string, error) {
	return "incident details", pager.enrichErr
}
func (pager *fakePager) Deliver(_ context.Context, _ Event, body string) (string, error) {
	pager.calls++
	pager.body = body
	return "NOTE1", pager.err
}

func TestRunnerPersistsBeforeDeliveryAndReusesOutput(t *testing.T) {
	queue := &fakeQueue{job: &Job{Event: testEvent(), Token: uuid.New(), Attempts: 1}}
	agent := &fakeAgent{}
	pager := &fakePager{err: errors.New("note delivery failed"), enrichErr: errors.New("PD API unavailable")}
	runner := &Runner{Queue: queue, Agent: agent, Pager: pager, Slack: &fakeSlack{}, PublicURL: "https://diderot.example",
		AllowedScopes: []string{"public", "slack-channel:COPS"}}
	require.ErrorContains(t, runner.Tick(context.Background()), "note delivery failed")
	require.Equal(t, 1, agent.calls)
	require.Equal(t, ask.AskRunSurfacePagerDuty, agent.request.Surface)
	require.Equal(t, runner.AllowedScopes, agent.request.AllowedScopes)
	require.Contains(t, agent.request.UserInput, "2026-10-14T19:30:00Z")
	require.Contains(t, agent.request.UserInput, "PagerDuty enrichment unavailable")
	require.Contains(t, agent.request.UserInput, `final section headed exactly "Possibly related agent work"`)
	require.Less(t, strings.Index(agent.request.UserInput, relatedWorkHeading), strings.Index(agent.request.UserInput, "untrusted data"))
	require.Contains(t, queue.output, "Partial investigation:")
	require.Contains(t, queue.output, "/admin/ask/sessions/"+agent.request.SessionID.String())
	require.NotNil(t, queue.failure)
	pager.err = nil
	require.NoError(t, runner.Tick(context.Background()))
	require.Equal(t, 1, agent.calls)
	require.Equal(t, 1, agent.creates)
	require.Equal(t, queue.output, pager.body)
	require.Equal(t, "NOTE1", queue.completed)
}

func TestRunnerDoesNotDeliverUnsavedOutput(t *testing.T) {
	id := uuid.New()
	queue := &fakeQueue{job: &Job{Event: testEvent(), SessionID: &id}, saveErr: ErrLeaseLost}
	pager := &fakePager{}
	runner := &Runner{Queue: queue, Agent: &fakeAgent{}, Pager: pager, Slack: &fakeSlack{}, AllowedScopes: []string{"public"}}
	require.ErrorIs(t, runner.Tick(context.Background()), ErrLeaseLost)
	require.Zero(t, pager.calls)
	require.Nil(t, queue.job.Output)
}

func TestRunnerRetainsSessionAcrossGenerationRetry(t *testing.T) {
	id := uuid.New()
	queue := &fakeQueue{job: &Job{Event: testEvent(), SessionID: &id}}
	agent := &fakeAgent{err: errors.New("model failed")}
	pager := &fakePager{}
	runner := &Runner{Queue: queue, Agent: agent, Pager: pager, Slack: &fakeSlack{}, AllowedScopes: []string{"public"}}
	require.ErrorContains(t, runner.Tick(context.Background()), "model failed")
	require.Equal(t, id, agent.request.SessionID)
	require.Zero(t, agent.creates)
	require.Zero(t, pager.calls)
	require.NotNil(t, queue.failure)
}

func TestRunnerRetriesLateSlackThreadWithoutRegenerationOrDuplicateNote(t *testing.T) {
	queue := &fakeQueue{job: &Job{Event: testEvent(), Attempts: 1}}
	agent, pager := &fakeAgent{}, &fakePager{}
	slack := &fakeSlack{err: ErrSlackThreadNotFound}
	runner := &Runner{Queue: queue, Agent: agent, Pager: pager, Slack: slack, AllowedScopes: []string{"public"}}
	require.ErrorIs(t, runner.Tick(t.Context()), ErrSlackThreadNotFound)
	require.Equal(t, "NOTE1", queue.job.NoteID)
	require.Empty(t, queue.completed)
	require.Equal(t, 1, pager.calls)
	require.Equal(t, 1, agent.calls)
	slack.err = nil
	require.NoError(t, runner.Tick(t.Context()))
	require.Equal(t, 1, agent.calls)
	require.Equal(t, 1, pager.calls)
	require.Equal(t, 2, slack.calls)
	require.Equal(t, queue.output, slack.body)
	require.Equal(t, "NOTE1", queue.completed)
}
