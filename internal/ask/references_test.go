package ask

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseZendeskReferences(t *testing.T) {
	input := "https://synthetic.zendesk.com/agent/tickets/12?view=1#comment " +
		"https://synthetic.zendesk.com/hc/en-us/articles/7-Some-title?x=y " +
		"https://synthetic.zendesk.com/hc/en-us/articles/7 " +
		"https://other.zendesk.com/agent/tickets/12"
	refs := ParseReferences(input, nil)
	require.Len(t, refs, 3)
	require.Equal(t, ReferenceZendeskURL, refs[0].Kind)
	require.Equal(t, "https://synthetic.zendesk.com/agent/tickets/12", refs[0].Value)
	require.Equal(t, "https://synthetic.zendesk.com/hc/en-us/articles/7", refs[1].Value)
	require.Equal(t, "https://other.zendesk.com/agent/tickets/12", refs[2].Value)
	for _, raw := range []string{
		"https://example.com/agent/tickets/12", "https://zendesk.com.evil.test/agent/tickets/12",
		"https://user@synthetic.zendesk.com/agent/tickets/12", "https://synthetic.zendesk.com:8443/agent/tickets/12",
		"https://synthetic.zendesk.com/agent/tickets/0", "https://synthetic.zendesk.com/agent/tickets/12/not-a-ticket",
	} {
		require.Empty(t, ParseReferences(raw, nil), raw)
	}
}

func TestParseReferencesRecognizesExactCorpusHandles(t *testing.T) {
	input := "See https://example.slack.com/archives/C0EXAMPLE01/p1700000000123456, ENG-770, session 550e8400-e29b-41d4-a716-446655440000, event_id:42, and chunk=99."
	got := ParseReferences(input, testLinearTeams)
	require.Len(t, got, 5)
	require.Equal(t, CorpusReference{
		Kind: ReferenceSlackPermalink, Value: "C0EXAMPLE01\x001700000000.123456", Raw: "https://example.slack.com/archives/C0EXAMPLE01/p1700000000123456", Role: ReferencePrimary,
		ChannelID: "C0EXAMPLE01", MessageTS: "1700000000.123456",
	}, got[0])
	require.Equal(t, ReferenceLinearIssue, got[1].Kind)
	require.Equal(t, "ENG-770", got[1].Value)
	require.Equal(t, ReferenceSessionUUID, got[2].Kind)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", got[2].Value)
	require.Equal(t, ReferenceEventID, got[3].Kind)
	require.Equal(t, "42", got[3].Value)
	require.Equal(t, ReferenceChunkID, got[4].Kind)
	require.Equal(t, "99", got[4].Value)
}

func TestParseReferencesRejectsSemanticAndUnsafeLookalikes(t *testing.T) {
	input := "try searching ENG-999 or https://example.com/archives/C123/p1700000000123456; 2026-08-12 and 123 are not handles"
	got := ParseReferences(input, testLinearTeams)
	require.Len(t, got, 1)
	require.Equal(t, ReferenceLinearIssue, got[0].Kind)
	require.Equal(t, "ENG-999", got[0].Value)
}

func TestParseReferencesLinearURLAndDeduplication(t *testing.T) {
	input := "https://linear.app/example/issue/ENG-770/title and ENG-770"
	got := ParseReferences(input, testLinearTeams)
	require.Len(t, got, 1)
	require.Equal(t, ReferenceLinearIssue, got[0].Kind)
	require.Equal(t, "ENG-770", got[0].Value)
	require.Equal(t, "https://linear.app/example/issue/ENG-770/title", got[0].Raw)
}

func TestParseReferencesRejectsLinearLookalikeHost(t *testing.T) {
	input := "https://evil-linear.app/example/issue/ENG-770/title and https://sub.linear.app/example/issue/ENG-771/title"
	got := ParseReferences(input, testLinearTeams)

	require.Len(t, got, 1)
	require.Equal(t, ReferenceLinearIssue, got[0].Kind)
	require.Equal(t, "ENG-771", got[0].Value)
	require.Equal(t, "https://sub.linear.app/example/issue/ENG-771/title", got[0].Raw)
}

func TestParseReferencesDoesNotExtractStandaloneHandlesFromUnknownURLs(t *testing.T) {
	unknown := "https://example.com/session/550e8400-e29b-41d4-a716-446655440000?event_id=42"
	input := unknown + " repeated " + unknown + " and chunk=99"
	got := ParseReferences(input, testLinearTeams)

	require.Len(t, got, 1)
	require.Equal(t, ReferenceChunkID, got[0].Kind)
	require.Equal(t, "99", got[0].Value)
}

// A bare key is a Linear reference only for a team the corpus holds. Anything
// else shaped like a key is ordinary text for retrieval, never an unavailable
// reference that would fail the turn closed.
func TestParseReferencesGatesBareLinearKeysOnKnownTeams(t *testing.T) {
	for _, input := range []string{
		"SHA-256 checksum mismatch", "UTF-8 decoding", "CVE-2024-3094 in xz", "HTTP-2 upgrades", "GPT-5 evals",
		"OPS-12 is not a known team",
	} {
		require.Empty(t, ParseReferences(input, testLinearTeams), input)
	}

	got := ParseReferences("compare ENG-770 with SHA-256 and APP-12", testLinearTeams)
	require.Equal(t, []CorpusReference{
		{Kind: ReferenceLinearIssue, Value: "ENG-770", Raw: "ENG-770", Role: ReferencePrimary},
		{Kind: ReferenceLinearIssue, Value: "APP-12", Raw: "APP-12", Role: ReferencePrimary},
	}, got)

	url := "https://linear.app/example/issue/OPS-12/title"
	got = ParseReferences(url, testLinearTeams)
	require.Len(t, got, 1, "a Linear URL is explicit whatever its team")
	require.Equal(t, CorpusReference{Kind: ReferenceLinearIssue, Value: "OPS-12", Raw: url, Role: ReferencePrimary}, got[0])

	for _, teams := range []map[string]struct{}{nil, {}} {
		require.Empty(t, ParseReferences("status of ENG-770", teams), "an unloaded or empty set admits no bare key")
		require.Len(t, ParseReferences(url, teams), 1)
	}
}

func TestReferenceRolesAndBlocking(t *testing.T) {
	primary := CorpusReference{Kind: ReferenceEventID, Value: "1", Role: ReferencePrimary}
	supporting := primary.WithRole(ReferenceSupporting)
	example := primary.WithRole(ReferenceFormatExample)
	require.Equal(t, ReferencePrimary, primary.Role)
	require.Equal(t, ReferenceSupporting, supporting.Role)
	require.Equal(t, ReferenceFormatExample, example.Role)

	state := ReferencePrerequisiteState{Unavailable: []CorpusReference{supporting}}
	require.False(t, state.HasUnavailablePrimary())
	state.Unavailable = append(state.Unavailable, primary)
	require.True(t, state.HasUnavailablePrimary())
}

type blockingReferenceResolver struct {
	allowed []string
}

func (r *blockingReferenceResolver) PrepareReferences(_ context.Context, refs []CorpusReference, allowed []string) (ReferencePrerequisiteState, error) {
	r.allowed = append([]string(nil), allowed...)
	return ReferencePrerequisiteState{
		References:     refs,
		Unavailable:    []CorpusReference{refs[0]},
		PrimaryBlocked: true,
	}, nil
}

func TestFormatReferenceContextCarriesRoleAndNeverSearches(t *testing.T) {
	ref := CorpusReference{Kind: ReferenceSlackPermalink, Role: ReferenceSupporting}
	got := formatReferenceContext([]ResolvedReference{{Reference: ref, Source: "slack", Content: "exact"}})
	require.Contains(t, got, `role="supporting"`)
	require.Contains(t, got, "authoritative context, not search results")
	require.NotContains(t, strings.ToLower(got), "retrieve(")
	require.NotContains(t, got, "truncated")
}

// A capped reference is marked the way expand marks a capped result, so the
// model knows the item continues past what it was given.
func TestFormatReferenceContextMarksTruncatedReference(t *testing.T) {
	ref := CorpusReference{Kind: ReferenceLinearIssue, Role: ReferencePrimary}
	got := formatReferenceContext([]ResolvedReference{{Reference: ref, Source: "linear", Content: "leading slice", Truncated: true}})
	require.Contains(t, got, `<reference role="primary" kind="linear_issue" source="linear" truncated="true">`+"\nleading slice\n</reference>")
}

func TestAskServiceBlocksRequiredReferenceBeforeModel(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	sessionID := newAgenticSession(t, mgr)
	modelCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		modelCalled = true
		t.Fatal("required exact reference must block before model generation")
	}))
	defer server.Close()

	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())
	resolver := &blockingReferenceResolver{}
	svc.references = resolver
	result, err := svc.ExecuteTurn(context.Background(), TurnRequest{
		SessionID: sessionID, UserInput: "summarize event_id:42",
		Surface: AskRunSurfaceHTTP, AllowedScopes: []string{"slack-channel:C_SAME"},
	})
	require.NoError(t, err)
	require.Equal(t, ReferenceUnavailableText, result.Reply)
	require.Equal(t, IncompleteReferenceUnavailable, result.IncompleteReason)
	require.Equal(t, []string{"slack-channel:C_SAME"}, resolver.allowed)
	require.False(t, modelCalled)

	_, history, err := mgr.LoadAgenticSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Equal(t, ReferenceUnavailableText, AgenticMessageText(history[1].Message))
}

// The live repro: "SHA-256" parsed as a Linear key failed the turn closed.
// A key-shaped word from an unknown team must reach the model as an ordinary
// question, while an unresolvable key on a known team still blocks first.
func TestAskServiceGatesBareLinearKeysOnKnownTeams(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	var modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		writeRespondFixture(t, w, "resp_final", "nobody has")
	}))
	defer server.Close()

	svc := newServiceForTest(t, mgr, server.URL+"/v1", nil, time.Now())
	resolver := &fakeExactReferenceResolver{}
	svc.references = resolver

	result, err := executeHTTPTestTurn(context.Background(), svc, newAgenticSession(t, mgr),
		"Has anyone discussed SHA-256 checksum mismatches during deploys?")
	require.NoError(t, err)
	require.Equal(t, "nobody has", result.Reply)
	require.Empty(t, result.IncompleteReason)
	require.Equal(t, int32(1), modelCalls.Load())
	require.Equal(t, 0, resolver.calls)

	result, err = executeHTTPTestTurn(context.Background(), svc, newAgenticSession(t, mgr), "what is the status of ENG-999999?")
	require.NoError(t, err)
	require.Equal(t, ReferenceUnavailableText, result.Reply)
	require.Equal(t, IncompleteReferenceUnavailable, result.IncompleteReason)
	require.Equal(t, int32(1), modelCalls.Load(), "an unresolvable known-team key blocks before generation")
	require.Equal(t, 1, resolver.calls)
	require.Equal(t, "ENG-999999", resolver.refs[0].Value)
}
