package slack

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	slackgo "github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/ask"
)

type progressIntegrationRouter struct {
	execute func(ask.TurnRequest) (ask.TurnResult, error)
}

func (r *progressIntegrationRouter) LookupOrCreateSlackSession(context.Context, string, string, string) (uuid.UUID, error) {
	return uuid.MustParse("00000000-0000-0000-0000-000000000001"), nil
}

func (r *progressIntegrationRouter) SlackSessionState(context.Context, string, string) (bool, time.Time, error) {
	return true, time.Time{}, nil
}

func (r *progressIntegrationRouter) ExecuteTurn(_ context.Context, req ask.TurnRequest) (ask.TurnResult, error) {
	return r.execute(req)
}

type progressEventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *progressEventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *progressEventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (l *progressEventLog) index(prefix string) int {
	for i, event := range l.snapshot() {
		if strings.HasPrefix(event, prefix) {
			return i
		}
	}
	return -1
}

func newProgressIntegrationConsumer(t *testing.T, transport progressTransport, router AskRouter, postHandler http.HandlerFunc) *Consumer {
	t.Helper()
	server := httptest.NewServer(postHandler)
	t.Cleanup(server.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	consumer := NewConsumer(nil, nil, "", "", "example.slack.com", logger, nil)
	consumer.api = slackgo.New("xoxb-test", slackgo.OptionAPIURL(server.URL+"/"))
	consumer.progress = transport
	consumer.working = newFakeWorkingThreads()
	consumer.ask = router
	return consumer
}

func runProgressIntegrationTurn(consumer *Consumer) {
	consumer.runAskTurn(
		context.Background(), "C_TEST", "100.000", "100.000", "101.000", "claim-key",
		"U_TEST", "question", "question", "", time.Now(), true, nil, nil,
		[]string{"public", "internal", "slack-channel:C_TEST"}, false,
	)
}

func TestRunAskTurnUsesOnlyNativeStatusBeforeAnswer(t *testing.T) {
	events := &progressEventLog{}
	statusSet := make(chan struct{})
	var setOnce sync.Once
	transport := &fakeProgressTransport{statusFn: func(_ context.Context, status string) progressCallResult {
		if status == slackgo.AgentSessionStatusActive {
			events.add("status:clear")
		} else {
			events.add("status:set")
			setOnce.Do(func() { close(statusSet) })
		}
		return progressCallResult{}
	}}
	router := &progressIntegrationRouter{execute: func(req ask.TurnRequest) (ask.TurnResult, error) {
		if req.StatusEmitter != nil {
			t.Fatal("tool status emitter should be nil without a plan message")
		}
		<-statusSet
		return ask.TurnResult{Reply: "the answer", AssistantTurn: 1}, nil
	}}
	postCount := 0
	consumer := newProgressIntegrationConsumer(t, transport, router, func(w http.ResponseWriter, r *http.Request) {
		postCount++
		if r.URL.Path != "/chat.postMessage" {
			t.Errorf("unexpected Slack method %s", r.URL.Path)
		}
		events.add("reply:answer")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":""}`))
	})

	runProgressIntegrationTurn(consumer)
	if postCount != 1 {
		t.Fatalf("chat.postMessage calls = %d, want only the answer and no plan message", postCount)
	}
	answerAt := events.index("reply:answer")
	clearAt := events.index("status:clear")
	if answerAt < 0 || clearAt < 0 || answerAt >= clearAt {
		t.Fatalf("event order = %#v, want answer < status clear", events.snapshot())
	}
}

func TestRunAskTurnPostsErrorBeforeStatusClear(t *testing.T) {
	events := &progressEventLog{}
	transport := &fakeProgressTransport{statusFn: func(_ context.Context, status string) progressCallResult {
		if status == slackgo.AgentSessionStatusActive {
			events.add("status:clear")
		}
		return progressCallResult{}
	}}
	router := &progressIntegrationRouter{execute: func(ask.TurnRequest) (ask.TurnResult, error) {
		return ask.TurnResult{}, errors.New("model exploded")
	}}
	consumer := newProgressIntegrationConsumer(t, transport, router, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse Slack form: %v", err)
		}
		if got := r.Form.Get("text"); got != "sorry, I hit an unexpected error; please try again. If it keeps happening, let an operator know\nDiderot session ID: 00000000-0000-0000-0000-000000000001" {
			t.Errorf("Slack error reply = %q", got)
		}
		events.add("reply:error")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":""}`))
	})

	runProgressIntegrationTurn(consumer)
	errorAt := events.index("reply:error")
	clearAt := events.index("status:clear")
	if errorAt < 0 || clearAt < 0 || errorAt >= clearAt {
		t.Fatalf("event order = %#v, want error reply before status clear", events.snapshot())
	}
}

func TestRunAskTurnIncludesSessionIDOnAnswerPostFailure(t *testing.T) {
	postCount := 0
	router := &progressIntegrationRouter{execute: func(ask.TurnRequest) (ask.TurnResult, error) {
		return ask.TurnResult{Reply: "the answer", AssistantTurn: 1}, nil
	}}
	consumer := newProgressIntegrationConsumer(t, &fakeProgressTransport{}, router, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse Slack form: %v", err)
		}
		postCount++
		w.Header().Set("Content-Type", "application/json")
		if postCount == 1 {
			_, _ = w.Write([]byte(`{"ok":false,"error":"post_failed"}`))
			return
		}
		if got := r.Form.Get("text"); got != "sorry, I couldn't post the full answer; please try again\nDiderot session ID: 00000000-0000-0000-0000-000000000001" {
			t.Errorf("Slack error reply = %q", got)
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":""}`))
	})

	runProgressIntegrationTurn(consumer)
	if postCount != 2 {
		t.Fatalf("chat.postMessage calls = %d, want answer failure and error reply", postCount)
	}
}

func TestReplyAskErrorWithoutSessionID(t *testing.T) {
	consumer := newProgressIntegrationConsumer(t, &fakeProgressTransport{}, nil, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse Slack form: %v", err)
		}
		if got := r.Form.Get("text"); got != "sorry, I couldn't start this conversation; please try again shortly" {
			t.Errorf("Slack error reply = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":""}`))
	})

	consumer.replyAskError(context.Background(), "C_TEST", "100.000", "", "I couldn't start this conversation; please try again shortly")
}

func TestRunAskTurnDMUsesRealSlackThreadForDelivery(t *testing.T) {
	transport := &fakeProgressTransport{}
	router := &progressIntegrationRouter{execute: func(req ask.TurnRequest) (ask.TurnResult, error) {
		return ask.TurnResult{Reply: "the answer", AssistantTurn: 1}, nil
	}}
	var deliveredThread string
	consumer := newProgressIntegrationConsumer(t, transport, router, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse Slack form: %v", err)
		}
		deliveredThread = r.Form.Get("thread_ts")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"D_TEST","ts":""}`))
	})

	consumer.runAskTurn(
		context.Background(), "D_TEST", "100.000", "100.000", "101.000", "claim-key",
		"U_TEST", "question", "question", "", time.Now(), true, nil, nil,
		[]string{"public", "internal", "owner:test@example.com"}, false,
	)

	if deliveredThread != "100.000" {
		t.Fatalf("Slack thread_ts = %q, want triggering DM thread", deliveredThread)
	}
}

func TestRunAskTurnNotifiesOperatorOnceForChannelAndDM(t *testing.T) {
	tests := []struct {
		name          string
		channelID     string
		sessionThread string
		wantSource    string
	}{
		{name: "channel", channelID: "C_TEST", sessionThread: "100.000", wantSource: "*Source:* Slack in #C_TEST"},
		{name: "direct message", channelID: "D_HARPER", sessionThread: "100.000", wantSource: "*Source:* Slack in direct message"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type slackCall struct {
				path    string
				text    string
				channel string
			}
			calls := make(chan slackCall, 8)
			router := &progressIntegrationRouter{execute: func(ask.TurnRequest) (ask.TurnResult, error) {
				return ask.TurnResult{Reply: "the answer", AssistantTurn: 1}, nil
			}}
			consumer := newProgressIntegrationConsumer(t, &fakeProgressTransport{}, router, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse Slack form: %v", err)
				}
				calls <- slackCall{path: r.URL.Path, text: r.Form.Get("text"), channel: r.Form.Get("channel")}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/chat.getPermalink":
					_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","permalink":"https://example.slack.com/thread"}`))
				case "/conversations.open":
					_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_OPERATOR"}}`))
				case "/users.lookupByEmail":
					_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U_OPERATOR"}}`))
				default:
					_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":""}`))
				}
			})
			consumer.notifyEmails = []string{"operator@example.com"}
			consumer.notifyQuestions = true

			consumer.runAskTurn(
				context.Background(), tt.channelID, tt.sessionThread, "100.000", "101.000", "claim-key",
				"U_HARPER", "where is the deploy?", "where is the deploy?", "", time.Now(), true, nil, nil,
				[]string{"public", "internal"}, false,
			)

			var got []slackCall
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			for len(got) < 5 {
				select {
				case call := <-calls:
					got = append(got, call)
				case <-deadline.C:
					t.Fatalf("Slack calls = %#v, want answer plus one complete operator notification", got)
				}
			}

			var notifications []slackCall
			for _, call := range got {
				if strings.Contains(call.text, "*diderot ask*") {
					notifications = append(notifications, call)
				}
			}
			if len(notifications) != 1 {
				t.Fatalf("operator notifications = %#v, want exactly one", notifications)
			}
			if got := notifications[0].channel; got != "D_OPERATOR" {
				t.Errorf("operator notification channel = %q, want D_OPERATOR", got)
			}
			if !strings.Contains(notifications[0].text, tt.wantSource) {
				t.Errorf("operator notification = %q, missing %q", notifications[0].text, tt.wantSource)
			}
		})
	}
}

func TestRunAskTurnStatusFailuresDoNotPreventAnswer(t *testing.T) {
	transport := &fakeProgressTransport{statusFn: func(context.Context, string) progressCallResult {
		return progressCallResult{err: slackgo.SlackErrorResponse{Err: "not_allowed"}}
	}}
	router := &progressIntegrationRouter{execute: func(ask.TurnRequest) (ask.TurnResult, error) {
		return ask.TurnResult{Reply: "still answered", AssistantTurn: 1}, nil
	}}
	posted := false
	consumer := newProgressIntegrationConsumer(t, transport, router, func(w http.ResponseWriter, _ *http.Request) {
		posted = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C_TEST","ts":""}`))
	})

	runProgressIntegrationTurn(consumer)
	if !posted {
		t.Fatal("answer was not posted after native status failures")
	}
}
