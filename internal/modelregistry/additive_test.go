package modelregistry

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

// testMade is an entry as an earlier job's test left it: every fact is the
// tests' to find.
func testMade(m Model) Model {
	for _, fact := range RequestFacts {
		m = m.WithSource(fact, SourceTest)
	}
	return m
}

func TestAFreshModelsFactsAreTheTests(t *testing.T) {
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusOK }}
	server := fake.serve(t)
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), Model{Provider: "p", ID: "new"}, RoleThreadSummaries, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, RequestFacts, learned.LearnedFacts(), "what a test found is recorded as the test's")
	assert.Equal(t, SourceTest, learned.SourceOf(FactTokenParam))
}

// The models row as an installation's migration wrote it: facts stated, with
// no source. This is what the caption worker sends.
func statedCaptioner() Model {
	return Model{
		Provider: "p", ID: "gemini-flash", Protocols: []Protocol{ProtocolChat}, Images: true,
		ChatExtraBody: map[string]any{"thinking_config": map[string]any{"thinkingBudget": 0}},
	}
}

func TestOneRefusedRequestNeverChangesAWorkingChatShape(t *testing.T) {
	for name, model := range map[string]Model{
		"stated":         statedCaptioner(),
		"found by tests": testMade(Model{Provider: "p", ID: "m", Protocols: []Protocol{ProtocolChat}, Images: true}),
	} {
		t.Run(name, func(t *testing.T) {
			// The provider refuses once, as a passing fault would; every
			// other shape would be accepted.
			refusals := 0
			fake := &fakeProvider{answer: func(_ string, body map[string]any) int {
				if _, ok := body["max_tokens"]; ok {
					refusals++
					return http.StatusBadRequest
				}
				return http.StatusOK
			}}
			server := fake.serve(t)
			learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), model, RoleImageCaptions, TestOptions{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "refused the request its jobs send now")
			assert.Len(t, fake.paths(), 1, "no other shape is tried on a model that already answers on chat")
			assert.Equal(t, model, learned, "and nothing about it changes")
			assert.Empty(t, learned.ChatTokenParam)
			assert.Zero(t, learned.ChatReasoningTokenAllowance)
		})
	}
}

func TestAStatedEntryIsSentAsStatedAndOnlyGrows(t *testing.T) {
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusOK }}
	server := fake.serve(t)
	p := testProvider(KindOpenAI, server, "/v1")

	// Captions on the stated captioner: its own request, nothing learned.
	model := statedCaptioner()
	learned, err := TestForRole(context.Background(), p, model, RoleImageCaptions, TestOptions{})
	require.NoError(t, err)
	assert.Len(t, fake.paths(), 1)
	assert.Equal(t, map[string]any{"thinkingBudget": float64(0)}, fake.last().Body["thinking_config"])
	assert.True(t, model.SameRequests(learned), "the test changed nothing a job sends")
	assert.Empty(t, learned.Sources, "and claimed no fact as its own")
	assert.Equal(t, []Role{RoleImageCaptions}, learned.TestedFor)

	// Given to Ask, it gains the protocol that answered; what was stated
	// stays stated.
	learned, err = TestForRole(context.Background(), p, model, RoleAsk, TestOptions{})
	require.NoError(t, err)
	assert.Equal(t, []Protocol{ProtocolChat, ProtocolResponses}, learned.Protocols)
	assert.False(t, learned.Learned(FactProtocols))
	assert.Equal(t, model.ChatExtraBody, learned.ChatExtraBody)
}

func TestATestNeverSetsAStatedServiceTierOrEffort(t *testing.T) {
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusOK }}
	server := fake.serve(t)
	p := testProvider(KindOpenAI, server, "/v1")
	// Stated: Responses, three efforts, no service tier.
	model := Model{Provider: "p", ID: "gpt", Protocols: []Protocol{ProtocolResponses, ProtocolChat}, ReasoningEfforts: []string{"low", "medium", "high"}}

	_, err := TestForRole(context.Background(), p, model, RoleAsk, TestOptions{Effort: "low", ServiceTier: "priority"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service tier is set, and its entry says it takes none")
	_, err = TestForRole(context.Background(), p, model, RoleAsk, TestOptions{Effort: "none"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "low, medium, high, not none")
	assert.Empty(t, fake.paths(), "both are refused before any request")

	learned, err := TestForRole(context.Background(), p, model, RoleAsk, TestOptions{Effort: "high"})
	require.NoError(t, err)
	assert.False(t, learned.ServiceTier)
	assert.Equal(t, model.ReasoningEfforts, learned.ReasoningEfforts)

	// With the tier stated, the test sends it and leaves it.
	model.ServiceTier = true
	learned, err = TestForRole(context.Background(), p, model, RoleAsk, TestOptions{Effort: "high", ServiceTier: "priority"})
	require.NoError(t, err)
	assert.Equal(t, "priority", fake.last().Body["service_tier"])
	assert.True(t, model.SameRequests(learned))
}

func TestAStatedEntryThatTakesNoImagesIsNotGivenThem(t *testing.T) {
	fake := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusOK }}
	server := fake.serve(t)
	model := Model{Provider: "p", ID: "text-only", Protocols: []Protocol{ProtocolChat}}
	_, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), model, RoleImageCaptions, TestOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not accept images")
	assert.Empty(t, fake.paths())
}

func TestTitlesSendTheStatedEffortOrNone(t *testing.T) {
	fake := &fakeProvider{answer: func(_ string, body map[string]any) int {
		if body["reasoning"] != nil {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}}
	server := fake.serve(t)
	p := testProvider(KindOpenAI, server, "/v1")

	// Stated without low: one request, with no effort.
	model := Model{Provider: "p", ID: "mini", Protocols: []Protocol{ProtocolChat}}
	learned, err := TestForRole(context.Background(), p, model, RoleTitles, TestOptions{})
	require.NoError(t, err)
	assert.Len(t, fake.paths(), 1)
	assert.Nil(t, fake.last().Body["reasoning"])
	assert.Empty(t, learned.ReasoningEfforts)

	// Stated with low, which the model refuses: the test fails, and the
	// entry keeps what was stated.
	model.ReasoningEfforts = []string{"low"}
	learned, err = TestForRole(context.Background(), p, model, RoleTitles, TestOptions{})
	require.Error(t, err)
	assert.Equal(t, model, learned)
}

func TestSameRequestsIgnoresTestsAndSources(t *testing.T) {
	model := statedCaptioner()
	tested := testMade(model)
	tested.TestedFor = []Role{RoleImageCaptions}
	assert.True(t, model.SameRequests(tested))
	assert.Equal(t, model.RequestKey(), tested.RequestKey())
	tested.ChatTokenParam = llmclient.ChatMaxCompletionTokens
	assert.False(t, model.SameRequests(tested))
	assert.Error(t, model.WithSource("nonsense", SourceTest).Validate(KindOpenAI))
}
