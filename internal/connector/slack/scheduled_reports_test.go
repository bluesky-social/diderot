package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"
	slackgo "github.com/slack-go/slack"
)

func TestDeliverScheduledReportAddsRunMetadata(t *testing.T) {
	forms := make(chan url.Values, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		forms <- r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"CLEGAL","ts":"1700000000.000100"}`))
	}))
	t.Cleanup(srv.Close)

	c := &Consumer{api: slackgo.New("xoxb-test", slackgo.OptionAPIURL(srv.URL+"/"))}
	runID := uuid.New()
	ts, err := c.DeliverScheduledReport(context.Background(), "slack_channel", "CLEGAL", runID, "**Weekly update**")
	if err != nil {
		t.Fatal(err)
	}
	if ts != "1700000000.000100" {
		t.Fatalf("ts=%q", ts)
	}
	form := <-forms
	if form.Get("channel") != "CLEGAL" || form.Get("text") != "**Weekly update**" {
		t.Fatalf("form=%v", form)
	}
	if form.Get("mrkdwn") != "false" {
		t.Fatalf("scheduled report fallback enables legacy mrkdwn: %v", form)
	}
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(form.Get("blocks")), &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "markdown" || blocks[0]["text"] != "**Weekly update**" {
		t.Fatalf("blocks=%#v", blocks)
	}
	var metadata slackgo.SlackMetadata
	if err := json.Unmarshal([]byte(form.Get("metadata")), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.EventType != "diderot_scheduled_report" || metadata.EventPayload["run_id"] != runID.String() {
		t.Fatalf("metadata=%+v", metadata)
	}
}

func TestDeliverScheduledReportRejectsUnknownKind(t *testing.T) {
	c := &Consumer{api: slackgo.New("xoxb-test")}
	if _, err := c.DeliverScheduledReport(context.Background(), "email", "x", uuid.New(), "body"); err == nil {
		t.Fatal("expected unknown delivery error")
	}
}
