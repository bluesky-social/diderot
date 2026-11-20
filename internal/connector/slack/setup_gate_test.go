package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/settings"
)

type fixedAskGate struct{ live bool }

func (g fixedAskGate) AskAllowed(admin bool) bool { return admin || g.live }

// TestAskRoutingRefusedDuringSetup: while the install is in setup, a mention
// gets the setup notice in its thread and starts no Ask turn, whoever sent
// it. Ingest still runs.
func TestAskRoutingRefusedDuringSetup(t *testing.T) {
	pool := getConsumerPool(t)
	var mu sync.Mutex
	var notices []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/chat.postMessage" {
			mu.Lock()
			notices = append(notices, r.Form.Get("thread_ts")+" "+r.Form.Get("text"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"channel":"C_OPS","ts":"1700000009.000100"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	c.api = slack.New("token", slack.OptionAPIURL(server.URL+"/"))
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router
	c.WithAskGate(fixedAskGate{live: false})

	raw := `{
		"type": "message",
		"channel": "C_OPS",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> what did we decide?",
		"ts": "1700000101.000100"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := append([]string(nil), notices...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "1700000101.000100 "+settings.SetupNotice {
		t.Fatalf("notices = %q, want the setup notice in the mention's thread", got)
	}
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.knownCalls) != 0 || len(router.createCalls) != 0 {
		t.Fatalf("a refused mention reached the Ask router: known=%v create=%v", router.knownCalls, router.createCalls)
	}
	var ingested int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE source = 'slack' AND external_id = 'C_OPS:1700000101.000100'`).Scan(&ingested); err != nil {
		t.Fatal(err)
	}
	if ingested != 1 {
		t.Fatalf("ingested = %d, want the mention ingested while Ask is closed", ingested)
	}
}

// TestAskRoutingOpenOnceLive: the same mention routes to Ask once the install
// is live.
func TestAskRoutingOpenOnceLive(t *testing.T) {
	pool := getConsumerPool(t)
	c := newTestConsumer(t, pool)
	c.selfUserID.Store("U_DIDEROT")
	router := &fakeAskRouter{known: map[string]bool{}}
	c.ask = router
	c.WithAskGate(fixedAskGate{live: true})
	raw := `{
		"type": "message",
		"channel": "C_OPS",
		"user": "U_AVERY",
		"text": "<@U_DIDEROT> what did we decide?",
		"ts": "1700000102.000100"
	}`
	c.handleEventsAPI(context.Background(), wrapMessageEvent(t, raw))
	waitForLen(t, func() int { router.mu.Lock(); defer router.mu.Unlock(); return len(router.createCalls) }, 1,
		"a live install routes the mention")
}
