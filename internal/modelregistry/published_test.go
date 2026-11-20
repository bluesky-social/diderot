package modelregistry

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/llmclient"
)

func boolPtr(v bool) *bool { return &v }

// TestPublishedFactsFollowThePrecedence: the provider's own list wins over
// the catalog, fact by fact; what an administrator set, and what was stated
// when the entry was written, stay.
func TestPublishedFactsFollowThePrecedence(t *testing.T) {
	published := Published{
		Provider: Facts{Images: boolPtr(false), Reasons: boolPtr(true)},
		Catalog: Facts{
			Images: boolPtr(true), Temperature: boolPtr(false), Reasons: boolPtr(true),
			Efforts: []string{"low", "medium", "high", "bogus"}, OutputTokens: 128000,
		},
	}
	entry := NewEntry("p", "m").WithPublished(published, false)
	assert.False(t, entry.Images, "the provider's list wins where it says")
	assert.Equal(t, SourceProvider, entry.SourceOf(FactImages))
	assert.True(t, entry.ChatOmitTemperature, "the catalog fills what the list is silent on")
	assert.Equal(t, SourceCatalog, entry.SourceOf(FactTemperature))
	assert.Equal(t, []string{"low", "medium", "high"}, entry.ReasoningEfforts, "an effort Diderot does not know is left out")
	assert.Equal(t, "low", entry.ChatReasoningEffort, "chat jobs ask for the least reasoning it offers")
	assert.Equal(t, DefaultReasoningTokenAllowance, entry.ChatReasoningTokenAllowance, "which still reasons, so it gets room")
	assert.Equal(t, SourceTest, entry.SourceOf(FactTokenParam), "what nothing publishes is left to the tests")
	require.NoError(t, entry.Validate(KindOpenAI))

	// An administrator's correction is above everything.
	corrected := entry.WithSource(FactTemperature, SourceAdmin)
	corrected.ChatOmitTemperature = false
	again := corrected.WithPublished(published, true)
	assert.False(t, again.ChatOmitTemperature)
	assert.Equal(t, SourceAdmin, again.SourceOf(FactTemperature))

	// An entry whose facts were stated when it was written, as a migration
	// writes one, keeps them unless an administrator asks for the published
	// ones.
	stated := Model{Provider: "p", ID: "m", Protocols: []Protocol{ProtocolChat}, Images: true}
	assert.True(t, stated.SameRequests(stated.WithPublished(published, false)))
	asked := stated.WithPublished(published, true)
	assert.False(t, asked.Images)
	assert.True(t, asked.ChatOmitTemperature)

	// A model whose reasoning can be turned off is asked for none, and
	// needs no room; one that does not reason is sent nothing.
	off := NewEntry("p", "m").WithPublished(Published{Catalog: Facts{Reasons: boolPtr(true), Efforts: []string{"none", "high"}}}, false)
	assert.Equal(t, "none", off.ChatReasoningEffort)
	assert.Zero(t, off.ChatReasoningTokenAllowance)
	plain := NewEntry("p", "m").WithPublished(Published{Catalog: Facts{Reasons: boolPtr(false), Efforts: []string{}}}, false)
	assert.Empty(t, plain.ChatReasoningEffort)
	assert.Empty(t, plain.ReasoningEfforts)
	assert.Equal(t, SourceCatalog, plain.SourceOf(FactEfforts), "that it takes no effort is a published fact too")
	// A small model's room is half what it can write.
	small := NewEntry("p", "m").WithPublished(Published{Catalog: Facts{Reasons: boolPtr(true), OutputTokens: 8192}}, false)
	assert.Equal(t, 4096, small.ChatReasoningTokenAllowance)
	assert.Empty(t, small.ChatReasoningEffort, "no effort is listed, so none is sent")

	// Nothing published: nothing changes.
	assert.Equal(t, NewEntry("p", "m"), NewEntry("p", "m").WithPublished(Published{}, false))
}

func TestPublishedPriceLimitsAndEnd(t *testing.T) {
	two, ten, three := 2.0, 10.0, 3.0
	published := Published{
		Provider: Facts{ContextTokens: 1000, Retires: "2027-01-01"},
		Catalog:  Facts{ContextTokens: 2000, OutputTokens: 500, InputPrice: &two, OutputPrice: &ten, Deprecated: true, Retires: "2026-12-01", Successor: "next"},
	}
	in, out, source, ok := published.Price()
	assert.True(t, ok)
	assert.Equal(t, SourceCatalog, source)
	assert.InDelta(t, 2.0, in, 1e-9)
	assert.InDelta(t, 10.0, out, 1e-9)
	contextTokens, output := published.Limits()
	assert.Equal(t, 1000, contextTokens)
	assert.Equal(t, 500, output)
	retires, deprecated, successor, source := published.End()
	assert.Equal(t, "2027-01-01", retires, "the provider's own date is the one believed")
	assert.False(t, deprecated)
	assert.Empty(t, successor)
	assert.Equal(t, SourceProvider, source)
	published.Provider.InputPrice, published.Provider.OutputPrice = &three, &three
	_, _, source, _ = published.Price()
	assert.Equal(t, SourceProvider, source)
	_, _, _, source = Published{}.End()
	assert.Empty(t, source)
}

// TestAPublishedShapeIsSentAsItIs: a chat request carries the effort the
// entry says, and a fact that is published is not one the test varies.
func TestAPublishedShapeIsSentAsItIs(t *testing.T) {
	// The model refuses a temperature, which the catalog said.
	fake := &fakeProvider{answer: func(_ string, body map[string]any) int {
		if _, ok := body["temperature"]; ok {
			return http.StatusBadRequest
		}
		return http.StatusOK
	}}
	server := fake.serve(t)
	entry := NewEntry("p", "m").WithPublished(Published{Catalog: Facts{
		Temperature: boolPtr(false), Reasons: boolPtr(true), Efforts: []string{"minimal", "high"},
	}}, false)
	trace := &TestTrace{}
	learned, err := TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), entry, RoleThreadSummaries, TestOptions{Trace: trace})
	require.NoError(t, err)
	assert.Equal(t, 1, trace.Requests)
	assert.Equal(t, []string{FactProtocols, FactTokenParam}, trace.Learned)
	body := fake.last().Body
	assert.Equal(t, "minimal", body["reasoning_effort"])
	assert.NotContains(t, body, "temperature")
	assert.Equal(t, SourceCatalog, learned.SourceOf(FactTemperature))

	// Where the catalog is wrong, the test fails without trying the
	// opposite: a published fact is corrected by an administrator, not by a
	// guess.
	wrong := NewEntry("p", "m").WithPublished(Published{Catalog: Facts{Temperature: boolPtr(true)}}, false)
	trace = &TestTrace{}
	_, err = TestForRole(context.Background(), testProvider(KindOpenAI, server, "/v1"), wrong, RoleThreadSummaries, TestOptions{Trace: trace})
	require.Error(t, err)
	assert.Equal(t, 2, trace.Requests, "the output-limit field, which nothing publishes, is the only thing tried")

	// A model of Anthropic's is tried on Messages first.
	messages := &fakeProvider{answer: func(string, map[string]any) int { return http.StatusOK }}
	gateway := messages.serve(t)
	_, err = TestForRole(context.Background(), testProvider(KindOpenAI, gateway, "/v1"), NewEntry("p", "claude:main"), RoleAsk, TestOptions{PreferMessages: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/messages"}, messages.paths())
	assert.Equal(t, map[string]any{"reasoning_effort": "low", "x": 1}, Model{ChatReasoningEffort: "low", ChatExtraBody: map[string]any{"x": 1}}.ChatFields())
	assert.Error(t, Model{ID: "m", ChatReasoningEffort: "low", ChatExtraBody: map[string]any{"reasoning_effort": "high"}}.Validate(KindOpenAI))
	_ = llmclient.ChatMaxTokens
}

func TestListReadsWhatAGatewaySaysAboutAModel(t *testing.T) {
	listed := openAIModel{
		ID: "gemini", OwnedBy: "gcp.vertex_ai", MaxTokens: 65536,
		Capabilities:        []byte(`{"reasoning":true,"tools":true,"temperature":true,"structured_outputs":true}`),
		SupportedParameters: []string{"max_tokens", "reasoning_effort", "tools"},
	}.listed()
	assert.Equal(t, "gcp.vertex_ai", listed.OwnedBy)
	assert.Equal(t, 65536, listed.OutputTokens)
	require.NotNil(t, listed.Temperature)
	assert.True(t, *listed.Temperature, "the flags win over the parameter list")
	assert.True(t, *listed.Tools)
	assert.True(t, *listed.Reasons)
	facts := listed.Facts()
	assert.Equal(t, 65536, facts.OutputTokens)

	// A parameter list alone says, by leaving one out, that the model does
	// not take it; a capabilities value of another shape is ignored.
	router := openAIModel{ID: "o", SupportedParameters: []string{"tools", "max_tokens"}, Capabilities: []byte(`["completion","vision"]`)}.listed()
	require.NotNil(t, router.Temperature)
	assert.False(t, *router.Temperature)
	assert.True(t, *router.Tools)
	assert.Nil(t, router.Reasons)
	assert.Nil(t, openAIModel{ID: "bare"}.listed().Temperature)
}
