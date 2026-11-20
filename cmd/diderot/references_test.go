package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/stretchr/testify/require"
)

type fakeExactReferenceLookup struct {
	ids    map[string]int64
	scopes map[int64]string
	bodies map[int64]string
}

func (f *fakeExactReferenceLookup) SlackEventID(_ context.Context, channel, ts string) (int64, error) {
	return f.ids["slack:"+channel+":"+ts], nil
}
func (f *fakeExactReferenceLookup) SlackEventContent(_ context.Context, id int64) (string, string, string, error) {
	body, ok := f.bodies[id]
	if !ok {
		return "", "", "", errors.New("missing")
	}
	return "slack", "https://slack.example/" + strconv.FormatInt(id, 10), body, nil
}
func (f *fakeExactReferenceLookup) LinearEventID(_ context.Context, key string) (int64, error) {
	return f.ids["linear:"+key], nil
}

func (f *fakeExactReferenceLookup) ZendeskEventID(_ context.Context, key string, allowedScopes []string) (int64, error) {
	id := f.ids["zendesk:"+key]
	if !scopeAllowed(f.scopes[id], allowedScopes) {
		return 0, errors.New("missing")
	}
	return id, nil
}
func (f *fakeExactReferenceLookup) SessionEventID(_ context.Context, id string) (int64, error) {
	return f.ids["session:"+id], nil
}
func (f *fakeExactReferenceLookup) EventIDReference(_ context.Context, id int64) (int64, error) {
	if id == 0 {
		return 0, errors.New("missing")
	}
	return id, nil
}
func (f *fakeExactReferenceLookup) ChunkEventID(_ context.Context, id int64) (int64, error) {
	if id == 0 {
		return 0, errors.New("missing")
	}
	return id + 1000, nil
}
func (f *fakeExactReferenceLookup) EventScope(_ context.Context, id int64) (string, error) {
	scope, ok := f.scopes[id]
	if !ok {
		return "", errors.New("missing")
	}
	return scope, nil
}

type fakeReferenceExpander struct {
	contents  map[int64]string
	truncated map[int64]bool
	calls     []int64
}

func (f *fakeReferenceExpander) Expand(_ context.Context, id int64) (ask.ExpandResult, error) {
	f.calls = append(f.calls, id)
	content, ok := f.contents[id]
	if !ok {
		return ask.ExpandResult{}, ask.ErrExpandEventNotFound
	}
	return ask.ExpandResult{Source: "slack", Content: content, Truncated: f.truncated[id]}, nil
}

func TestSourceReferenceResolverUsesAudienceBeforeExpand(t *testing.T) {
	lookup := &fakeExactReferenceLookup{
		ids: map[string]int64{
			"slack:C_SAME:1700000000.351689":  1,
			"slack:C_OTHER:1700000000.351689": 2,
		},
		scopes: map[int64]string{1: "slack-channel:C_SAME", 2: "slack-channel:C_OTHER"},
		bodies: map[int64]string{1: "exact linked message", 2: "other-channel exact secret"},
	}
	expander := &fakeReferenceExpander{contents: map[int64]string{1: "same-channel exact content", 2: "other-channel secret"}}
	resolver := newSourceReferenceResolver(lookup, expander)

	refs := []ask.CorpusReference{
		{Kind: ask.ReferenceSlackPermalink, ChannelID: "C_SAME", MessageTS: "1700000000.351689", Role: ask.ReferencePrimary},
		{Kind: ask.ReferenceSlackPermalink, ChannelID: "C_OTHER", MessageTS: "1700000000.351689", Role: ask.ReferencePrimary},
	}
	state, err := resolver.PrepareReferences(context.Background(), refs, []string{"public", "internal", "slack-channel:C_SAME"})
	require.NoError(t, err)
	require.Len(t, state.Resolved, 1)
	require.Equal(t, int64(1), state.Resolved[0].EventID)
	require.Contains(t, state.Resolved[0].Content, "Exact linked Slack message:\nexact linked message")
	require.Contains(t, state.Resolved[0].Content, "Expanded Slack conversation context:\nsame-channel exact content")
	require.True(t, state.HasUnavailablePrimary())
	require.Equal(t, []int64{1}, expander.calls, "denied cross-private reference must not reach expand")
}

func TestSourceReferenceResolverLinkDoesNotGrantScope(t *testing.T) {
	lookup := &fakeExactReferenceLookup{
		ids:    map[string]int64{"linear:ENG-770": 7},
		scopes: map[int64]string{7: "internal"},
	}
	expander := &fakeReferenceExpander{contents: map[int64]string{7: "ticket"}}
	resolver := newSourceReferenceResolver(lookup, expander)
	ref := ask.CorpusReference{Kind: ask.ReferenceLinearIssue, Value: "ENG-770", Role: ask.ReferencePrimary}

	state, err := resolver.PrepareReferences(context.Background(), []ask.CorpusReference{ref}, []string{"public"})
	require.NoError(t, err)
	require.Empty(t, state.Resolved)
	require.True(t, state.HasUnavailablePrimary())
	require.Empty(t, expander.calls)
}

func TestZendeskExactReferencesUseFrozenScopesAndTenant(t *testing.T) {
	lookup := &fakeExactReferenceLookup{
		ids: map[string]int64{
			"zendesk:https://synthetic.zendesk.com/agent/tickets/12":    12,
			"zendesk:https://synthetic.zendesk.com/hc/en-us/articles/7": 7,
		},
		scopes: map[int64]string{12: "corpus:zendesk", 7: "public"},
	}
	expander := &fakeReferenceExpander{contents: map[int64]string{12: "private ticket", 7: "public guidance"}}
	resolver := newSourceReferenceResolver(lookup, expander)
	refs := ask.ParseReferences("https://synthetic.zendesk.com/agent/tickets/12 https://synthetic.zendesk.com/hc/en-us/articles/7-Public-guidance", nil)
	state, err := resolver.PrepareReferences(context.Background(), refs, []string{"public", "internal"})
	require.NoError(t, err)
	require.Len(t, state.Resolved, 1)
	require.Equal(t, int64(7), state.Resolved[0].EventID)
	require.True(t, state.HasUnavailablePrimary())
	require.Equal(t, []int64{7}, expander.calls)
	state, err = resolver.PrepareReferences(context.Background(), refs, []string{"public", "corpus:zendesk"})
	require.NoError(t, err)
	require.Len(t, state.Resolved, 2)
	require.False(t, state.HasUnavailablePrimary())
	state, err = resolver.PrepareReferences(context.Background(), ask.ParseReferences("https://other.zendesk.com/agent/tickets/12", nil), []string{"public", "corpus:zendesk"})
	require.NoError(t, err)
	require.Empty(t, state.Resolved)
	require.True(t, state.HasUnavailablePrimary())
}

func TestSourceReferenceResolverLegacySlackUsesExactBody(t *testing.T) {
	lookup := &fakeExactReferenceLookup{
		ids:    map[string]int64{"slack:C_LEGACY:1700000000.351689": 9},
		scopes: map[int64]string{9: "slack-channel:C_LEGACY"},
		bodies: map[int64]string{9: "legacy exact message"},
	}
	resolver := newSourceReferenceResolver(lookup, &fakeReferenceExpander{})
	ref := ask.CorpusReference{
		Kind: ask.ReferenceSlackPermalink, ChannelID: "C_LEGACY", MessageTS: "1700000000.351689", Role: ask.ReferencePrimary,
	}
	state, err := resolver.PrepareReferences(context.Background(), []ask.CorpusReference{ref}, []string{"slack-channel:C_LEGACY"})
	require.NoError(t, err)
	require.Len(t, state.Resolved, 1)
	require.Equal(t, "Exact linked Slack message:\nlegacy exact message", state.Resolved[0].Content)
	require.Equal(t, "https://slack.example/9", state.Resolved[0].URL)
}

func TestSourceReferenceResolverEmptyLegacySlackBodyIsUnavailable(t *testing.T) {
	lookup := &fakeExactReferenceLookup{
		ids:    map[string]int64{"slack:C_EMPTY:1700000000.351689": 10},
		scopes: map[int64]string{10: "slack-channel:C_EMPTY"},
		bodies: map[int64]string{10: ""},
	}
	resolver := newSourceReferenceResolver(lookup, &fakeReferenceExpander{})
	ref := ask.CorpusReference{
		Kind: ask.ReferenceSlackPermalink, ChannelID: "C_EMPTY", MessageTS: "1700000000.351689", Role: ask.ReferencePrimary,
	}

	state, err := resolver.PrepareReferences(context.Background(), []ask.CorpusReference{ref}, []string{"slack-channel:C_EMPTY"})
	require.NoError(t, err)
	require.Empty(t, state.Resolved)
	require.True(t, state.HasUnavailablePrimary())
}

func TestSourceReferenceResolverSupportingMissingDoesNotBlock(t *testing.T) {
	lookup := &fakeExactReferenceLookup{ids: map[string]int64{"linear:ENG-770": 7}, scopes: map[int64]string{7: "internal"}}
	resolver := newSourceReferenceResolver(lookup, &fakeReferenceExpander{contents: map[int64]string{}})
	ref := ask.CorpusReference{Kind: ask.ReferenceLinearIssue, Value: "ENG-770", Role: ask.ReferenceSupporting}
	state, err := resolver.PrepareReferences(context.Background(), []ask.CorpusReference{ref}, []string{"internal"})
	require.NoError(t, err)
	require.Empty(t, state.Resolved)
	require.False(t, state.HasUnavailablePrimary())
}

// referenceContentBudget is expand's model-facing cap, which every resolved
// reference shares.
const referenceContentBudget = 64 * 1024

func TestSourceReferenceResolverCapsExpandedContent(t *testing.T) {
	lookup := &fakeExactReferenceLookup{
		ids:    map[string]int64{"linear:ENG-1": 1, "linear:ENG-2": 2, "linear:ENG-3": 3},
		scopes: map[int64]string{1: "internal", 2: "internal", 3: "internal"},
	}
	big := strings.Repeat("界", referenceContentBudget) // multi-byte, so the cut must find a rune boundary
	expander := &fakeReferenceExpander{
		contents:  map[int64]string{1: big, 2: "small issue body", 3: "already cut by the source"},
		truncated: map[int64]bool{3: true},
	}
	resolver := newSourceReferenceResolver(lookup, expander)
	refs := []ask.CorpusReference{
		{Kind: ask.ReferenceLinearIssue, Value: "ENG-1", Role: ask.ReferencePrimary},
		{Kind: ask.ReferenceLinearIssue, Value: "ENG-2", Role: ask.ReferencePrimary},
		{Kind: ask.ReferenceLinearIssue, Value: "ENG-3", Role: ask.ReferencePrimary},
	}

	state, err := resolver.PrepareReferences(context.Background(), refs, []string{"internal"})
	require.NoError(t, err)
	require.Len(t, state.Resolved, 3)

	capped := state.Resolved[0]
	require.True(t, capped.Truncated)
	require.LessOrEqual(t, len(capped.Content), referenceContentBudget)
	require.Greater(t, len(capped.Content), referenceContentBudget-utf8.UTFMax)
	require.True(t, utf8.ValidString(capped.Content))
	require.True(t, strings.HasPrefix(big, capped.Content))

	require.Equal(t, "small issue body", state.Resolved[1].Content)
	require.False(t, state.Resolved[1].Truncated)

	require.Equal(t, "already cut by the source", state.Resolved[2].Content)
	require.True(t, state.Resolved[2].Truncated, "a source-side cut must stay visible")
}

func TestSourceReferenceResolverSlackCapKeepsExactMessage(t *testing.T) {
	const prefix = "Exact linked Slack message:\n"
	const contextHeader = "\n\nExpanded Slack conversation context:\n"
	exact := strings.Repeat("e", 8*1024)
	oversizedExact := strings.Repeat("x", referenceContentBudget+1)
	lookup := &fakeExactReferenceLookup{
		ids: map[string]int64{
			"slack:C1:1700000000.351689": 1,
			"slack:C1:1700000000.351690": 2,
			"slack:C1:1700000000.351691": 3,
		},
		scopes: map[int64]string{1: "slack-channel:C1", 2: "slack-channel:C1", 3: "slack-channel:C1"},
		bodies: map[int64]string{1: exact, 2: oversizedExact, 3: "short exact"},
	}
	hugeContext := strings.Repeat("c", 4*referenceContentBudget)
	expander := &fakeReferenceExpander{contents: map[int64]string{1: hugeContext, 2: hugeContext, 3: "short thread"}}
	resolver := newSourceReferenceResolver(lookup, expander)
	slackRef := func(ts string) ask.CorpusReference {
		return ask.CorpusReference{Kind: ask.ReferenceSlackPermalink, ChannelID: "C1", MessageTS: ts, Role: ask.ReferencePrimary}
	}
	refs := []ask.CorpusReference{slackRef("1700000000.351689"), slackRef("1700000000.351690"), slackRef("1700000000.351691")}

	state, err := resolver.PrepareReferences(context.Background(), refs, []string{"slack-channel:C1"})
	require.NoError(t, err)
	require.Len(t, state.Resolved, 3)

	trimmed := state.Resolved[0]
	require.True(t, trimmed.Truncated)
	require.Len(t, trimmed.Content, referenceContentBudget, "the context fills the budget left after the exact message")
	require.True(t, strings.HasPrefix(trimmed.Content, prefix+exact+contextHeader+"ccc"))

	// An exact message that alone exceeds the budget stays whole; the
	// context is what gets dropped.
	require.True(t, state.Resolved[1].Truncated)
	require.Equal(t, prefix+oversizedExact, state.Resolved[1].Content)

	require.False(t, state.Resolved[2].Truncated)
	require.Equal(t, prefix+"short exact"+contextHeader+"short thread", state.Resolved[2].Content)
}
