package retrieve

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
)

// TestRetrieveAcrossSourcesRanksEachSourceOnItsOwn: a search that is not
// limited to one source must not let a large source take every place. Twelve
// chat messages here match the question almost word for word; the one page
// and the one issue that answer it match less closely, and ranked together
// with the messages they would never be returned.
func TestRetrieveAcrossSourcesRanksEachSourceOnItsOwn(t *testing.T) {
	pool, store, emb := setupWorld(t)
	r := NewRetriever(pool, emb, store, nil)
	const question = "when does the export worker contract renew"

	for i := 0; i < 12; i++ {
		seedEvent(t, pool, store, emb, events.Event{
			Source: "slack", ExternalID: fmt.Sprintf("C0EXAMPLE1:17000000%02d.000100", i),
			OccurredAt: time.Unix(1700000000+int64(i), 0), Actor: "asker", ActorID: "U0EXAMPLE1",
			Body: question + strings.Repeat("?", i+1), Raw: json.RawMessage(`{}`),
		})
	}
	page := seedEvent(t, pool, store, emb, events.Event{
		Source: "notion", ExternalID: "page-1", OccurredAt: time.Unix(1690000000, 0),
		Body: "Vendor contracts. The export worker contract renews on 15 February; notice is 60 days.", Raw: json.RawMessage(`{}`),
	})
	issue := seedEvent(t, pool, store, emb, events.Event{
		Source: "linear", ExternalID: "issue-1", OccurredAt: time.Unix(1695000000, 0),
		Body: "ENG-1: decide whether to renew the export worker contract", Raw: json.RawMessage(`{}`),
	})

	search := func(sources []string, topK int) (bySource map[string]int, ids map[int64]bool, scores []float32) {
		t.Helper()
		res, err := r.Retrieve(context.Background(), Request{
			Question: question, TopK: topK,
			Filters: Filters{Sources: sources, AllowedScopes: []string{events.ScopePublic}},
		})
		if err != nil {
			t.Fatalf("retrieve %v: %v", sources, err)
		}
		bySource, ids = map[string]int{}, map[int64]bool{}
		for _, hit := range res.Hits {
			bySource[hit.Source]++
			ids[hit.EventID] = true
			scores = append(scores, hit.Score)
		}
		return bySource, ids, scores
	}

	// Every source, nothing asked for: the best few of each, best first.
	bySource, ids, scores := search(nil, 0)
	if bySource["slack"] != EachSourceDefault || bySource["notion"] != 1 || bySource["linear"] != 1 {
		t.Errorf("an unfiltered search returned %v; want %d messages, the page and the issue", bySource, EachSourceDefault)
	}
	if !ids[page] || !ids[issue] {
		t.Errorf("the page and the issue that answer the question were crowded out: %v", bySource)
	}
	for i := 1; i < len(scores); i++ {
		if scores[i] > scores[i-1] {
			t.Errorf("hits are not best first: %v", scores)
			break
		}
	}

	// More than a search of every source gives is capped for each source.
	if bySource, _, _ = search(nil, 50); bySource["slack"] != EachSourceMax || bySource["notion"] != 1 {
		t.Errorf("an unfiltered search for 50 returned %v; want %d messages and the page", bySource, EachSourceMax)
	}

	// Several named sources are each ranked on their own too.
	if bySource, _, _ = search([]string{"slack", "notion"}, 2); bySource["slack"] != 2 || bySource["notion"] != 1 || bySource["linear"] != 0 {
		t.Errorf("a search of two sources for 2 returned %v; want 2 messages and the page", bySource)
	}

	// One long page must not use up its source's places: its chunks are
	// folded into one hit, and the next page still comes back.
	long := seedEvent(t, pool, store, emb, events.Event{
		Source: "notion", ExternalID: "page-long", OccurredAt: time.Unix(1691000000, 0),
		Body: strings.Repeat(question+". ", 400), Raw: json.RawMessage(`{}`),
	})
	var chunks int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM event_chunks WHERE event_id = $1`, long).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks < 2*EachSourceMax {
		t.Fatalf("the long page has %d chunks; the test needs more than a search asks for", chunks)
	}
	if bySource, ids, _ = search(nil, 0); !ids[long] || !ids[page] || bySource["notion"] != 2 {
		t.Errorf("with a %d-chunk page in the source, an unfiltered search returned %v; want both pages", chunks, bySource)
	}

	// An event deleted after it was indexed still has its point in the
	// vector store for a while. It must not cost its source a place.
	if _, err := pool.Exec(context.Background(),
		`UPDATE events SET deleted = true WHERE source = 'slack' AND external_id = 'C0EXAMPLE1:1700000000.000100'`); err != nil {
		t.Fatal(err)
	}
	if bySource, _, _ = search([]string{"slack", "linear"}, 1); bySource["slack"] != 1 || bySource["linear"] != 1 {
		t.Errorf("with the best message deleted, a search for 1 of each source returned %v; want the next message and the issue", bySource)
	}

	// One source alone is its best top_k, as it always was.
	if bySource, _, _ = search([]string{"slack"}, 8); bySource["slack"] != 8 || len(bySource) != 1 {
		t.Errorf("a search of one source for 8 returned %v", bySource)
	}
}
