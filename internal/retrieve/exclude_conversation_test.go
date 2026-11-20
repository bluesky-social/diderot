package retrieve

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
)

// A Slack thread as the connector stores it: the first message has the
// thread key as its external_id and no thread_id of its own; each reply
// carries the key as its thread_id.
func slackThread(channel, rootTS string) (rootExternalID, key string) {
	key = channel + ":" + rootTS
	return key, key
}

func slackEvent(externalID, threadID, body string, at int64) events.Event {
	return events.Event{
		Source: "slack", ExternalID: externalID, ThreadID: threadID,
		OccurredAt: time.Unix(at, 0), Actor: "asker", ActorID: "U0EXAMPLE1",
		Body: body, Raw: json.RawMessage(`{}`),
	}
}

// TestRetrieveLeavesOutTheConversationBeingAnswered covers both halves of
// the rule. The thread a question was asked in must not come back as a hit
// for that question, however well it matches (it matches best of all: it is
// the question). An earlier thread in which the same thing was asked is
// somebody's earlier conversation and must still come back.
func TestRetrieveLeavesOutTheConversationBeingAnswered(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	const question = "when did we ship the export worker"

	currentRoot, currentKey := slackThread("C0EXAMPLE1", "1700000500.000100")
	currentQuestion := seedEvent(t, pool, store, emb, slackEvent(currentRoot, "", question, 1700000500))
	currentReply := seedEvent(t, pool, store, emb, slackEvent("C0EXAMPLE1:1700000510.000200", currentKey, question+" please", 1700000510))

	earlierRoot, earlierKey := slackThread("C0EXAMPLE1", "1690000000.000100")
	earlierQuestion := seedEvent(t, pool, store, emb, slackEvent(earlierRoot, "", question, 1690000000))
	earlierReply := seedEvent(t, pool, store, emb, slackEvent("C0EXAMPLE1:1690000010.000200", earlierKey, "it shipped in March", 1690000010))
	record := seedEvent(t, pool, store, emb, events.Event{
		Source: "notion", ExternalID: "page-1", OccurredAt: time.Unix(1695000000, 0),
		Body: "the export worker shipped on March 9", Raw: json.RawMessage(`{}`),
	})

	hitIDs := func(conversations []Conversation) map[int64]bool {
		t.Helper()
		res, err := r.Retrieve(context.Background(), Request{
			Question: question, TopK: 10,
			Filters: Filters{AllowedScopes: []string{events.ScopePublic}, ExcludeConversations: conversations},
		})
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		ids := map[int64]bool{}
		for _, hit := range res.Hits {
			ids[hit.EventID] = true
		}
		return ids
	}

	unfiltered := hitIDs(nil)
	for name, id := range map[string]int64{"the question": currentQuestion, "a reply in its thread": currentReply} {
		if !unfiltered[id] {
			t.Fatalf("with nothing excluded, %s should be a hit: this is the defect being fixed", name)
		}
	}

	answering := hitIDs([]Conversation{{Source: "slack", ThreadID: currentKey}})
	if answering[currentQuestion] || answering[currentReply] {
		t.Errorf("the conversation being answered came back as evidence for its own answer: %v", answering)
	}
	for name, id := range map[string]int64{
		"the earlier asking of the same question": earlierQuestion,
		"the reply it got":                        earlierReply,
		"the record that answers it":              record,
	} {
		if !answering[id] {
			t.Errorf("%s was left out; only the conversation being answered may be", name)
		}
	}

	// The same key under another source names a different conversation.
	if other := hitIDs([]Conversation{{Source: "notion", ThreadID: currentKey}}); !other[currentQuestion] {
		t.Error("a conversation is excluded by source and key together")
	}
}

func TestListLeavesOutTheConversationBeingAnswered(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)

	currentRoot, currentKey := slackThread("C0EXAMPLE1", "1700000500.000100")
	seedEventOnly(t, pool, slackEvent(currentRoot, "", "the question being answered", 1700000500))
	seedEventOnly(t, pool, slackEvent("C0EXAMPLE1:1700000510.000200", currentKey, "a follow-up in the same thread", 1700000510))
	earlierRoot, earlierKey := slackThread("C0EXAMPLE1", "1690000000.000100")
	seedEventOnly(t, pool, slackEvent(earlierRoot, "", "the same question, asked last year", 1690000000))
	seedEventOnly(t, pool, slackEvent("C0EXAMPLE1:1690000010.000200", earlierKey, "last year's reply", 1690000010))
	seedEventOnly(t, pool, slackEvent("C0EXAMPLE1:1700000400.000300", "", "an unrelated message in the channel", 1700000400))

	list := func(conversations []Conversation) []string {
		t.Helper()
		res, err := r.List(context.Background(), ListRequest{
			Sources: []string{"slack"}, TopK: 50,
			AllowedScopes: []string{events.ScopePublic}, ExcludeConversations: conversations,
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return listBodies(res)
	}

	if got := list(nil); len(got) != 5 {
		t.Fatalf("with nothing excluded every message is listed, got %d: %v", len(got), got)
	}
	got := list([]Conversation{{Source: "slack", ThreadID: currentKey}})
	want := []string{"an unrelated message in the channel", "last year's reply", "the same question, asked last year"}
	if len(got) != len(want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
	}
}
