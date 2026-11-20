package retrieve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
)

// fakeFinder is another source's channel directory: "general" and "mods",
// whose events share an external-ID prefix each.
type fakeFinder struct{}

func (fakeFinder) FindChannels(_ context.Context, entry string) ([]ChannelMatch, error) {
	switch strings.TrimPrefix(entry, "#") {
	case "general":
		return []ChannelMatch{{Source: "discord", ExternalIDPrefix: "100000000000000001/200000000000000001/", Scope: events.ScopeInternal}}, nil
	case "mods":
		return []ChannelMatch{{Source: "discord", ExternalIDPrefix: "100000000000000001/200000000000000002/", Scope: "corpus:mods"}}, nil
	}
	return nil, nil
}

// TestListChannelOfAnotherSource: a channel a ChannelFinder knows lists its
// events, threads and all, newest first; one whose scope the caller lacks
// fails as an unknown channel does; and a Slack channel and another
// source's channel can be listed together.
func TestListChannelOfAnotherSource(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	r.SetChannelFinder(fakeFinder{})
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	general, mods := "100000000000000001/200000000000000001/", "100000000000000001/200000000000000002/"
	seedEventOnly(t, pool, events.Event{Source: "discord", ExternalID: general + "300000000000000001", OccurredAt: base,
		Body: "a channel message", Scope: events.ScopeInternal})
	seedEventOnly(t, pool, events.Event{Source: "discord", ExternalID: general + "300000000000000002", ThreadID: general + "300000000000000002",
		OccurredAt: base.Add(time.Minute), Body: "a thread root", Scope: events.ScopeInternal})
	seedEventOnly(t, pool, events.Event{Source: "discord", ExternalID: general + "300000000000000003", ThreadID: general + "300000000000000002",
		OccurredAt: base.Add(2 * time.Minute), Body: "a thread reply", Scope: events.ScopeInternal})
	seedEventOnly(t, pool, events.Event{Source: "discord", ExternalID: mods + "300000000000000004", OccurredAt: base,
		Body: "moderators only", Scope: "corpus:mods"})
	seedSlackChannel(t, pool, "C0EXAMPLE120", "incidents", false)
	seedSlackMessage(t, pool, "C0EXAMPLE120", "100.1", "", "a slack message", base.Add(3*time.Minute))

	scopes := []string{events.ScopePublic, events.ScopeInternal, events.ScopeChannel("C0EXAMPLE120")}
	res, err := r.List(ctx, ListRequest{Channels: []string{"#general"}, AllowedScopes: scopes})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := listBodies(res), []string{"a thread reply", "a thread root", "a channel message"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("bodies = %v, want %v", got, want)
	}
	res, err = r.List(ctx, ListRequest{Channels: []string{"general"}, AllowedScopes: scopes, ThreadRootsOnly: true, ThreadedSources: []string{"slack", "discord"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, want := listBodies(res), []string{"a thread root", "a channel message"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("roots = %v, want %v", got, want)
	}
	res, err = r.List(ctx, ListRequest{Channels: []string{"general", "incidents"}, AllowedScopes: scopes})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := listBodies(res); len(got) != 4 || got[0] != "a slack message" {
		t.Errorf("both channels = %v, want the Slack message and the three Discord ones", got)
	}

	_, err = r.List(ctx, ListRequest{Channels: []string{"mods"}, AllowedScopes: scopes})
	if !errors.Is(err, ErrListChannelNotFound) {
		t.Fatalf("hidden channel: err = %v, want ErrListChannelNotFound", err)
	}
	res, err = r.List(ctx, ListRequest{Channels: []string{"mods"}, AllowedScopes: append(scopes, "corpus:mods")})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := listBodies(res); fmt.Sprint(got) != "[moderators only]" {
		t.Errorf("granted channel = %v", got)
	}
}

// TestSetChannelFinderConcurrentWithList is a regression for a data race:
// Retriever's doc comment promises "safe for concurrent use" (the same
// promise SetVectors keeps for the embedder/store pair via
// atomic.Pointer, because SetVectors IS called repeatedly at runtime,
// concurrently with live serving, whenever an administrator changes the
// embedding model). channelFinder is read by every List call that has a
// Channels filter (resolveListChannels) and is written by
// SetChannelFinder, but as a plain struct field with no atomic, mutex, or
// other synchronization between the two.
//
// Trigger: one goroutine calling SetChannelFinder repeatedly while another
// calls List with a Channels filter. Run with `go test -race`: an
// unsynchronized field fails with "DATA RACE" between the write in
// SetChannelFinder (list.go) and the read in resolveListChannels
// (list.go), even though nothing here mutates shared data incorrectly in
// any way a non-race test could observe -- the race detector is the
// expected-value mechanism here, not a comparison of returned data.
func TestSetChannelFinderConcurrentWithList(t *testing.T) {
	pool := listSetupPool(t)
	r := newListRetriever(pool)
	ctx := context.Background()

	seedEventOnly(t, pool, events.Event{
		Source: "slack", ExternalID: "C1:1", OccurredAt: time.Now().UTC(),
		Actor: "a", Body: "msg",
	})

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			r.SetChannelFinder(fakeFinder{})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, _ = r.List(ctx, ListRequest{
				AllowedScopes: []string{events.ScopePublic, events.ScopeInternal},
				Channels:      []string{"general"},
			})
		}
	}()
	wg.Wait()
}
