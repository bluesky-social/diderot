//go:build gateway

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/modelfacts"
	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
)

// TestGatewayPublishedFacts reports what a real model provider and the
// published model lists, as they are today, would make each job send. It
// lists the provider's models, matches each model the environment names for
// a job to the catalog, and runs the job's test on the entry a choice on
// the Models page would make: the published facts, then what the test
// finds. Nothing is changed anywhere.
//
// It is built only with the gateway tag and skips without
// DIDEROT_TEST_PROVIDER_URL and DIDEROT_TEST_PROVIDER_KEY. The jobs
// reported are those whose model is named (DIDEROT_TEST_MODEL_ASK and the
// others gatewayTestModels lists; DIDEROT_TEST_MODEL_EMBEDDINGS for the
// embedding model). The report goes to the file DIDEROT_TEST_FACTS_REPORT
// names, or to the test log.
//
//	go test -tags=gateway -v -count=1 -run TestGatewayPublishedFacts ./cmd/diderot
func TestGatewayPublishedFacts(t *testing.T) {
	provider := gatewayTestProvider(t)
	baseURL, apiKey := provider.Endpoint.BaseURL, provider.Endpoint.APIKey
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()
	// Never let a failure print the credential.
	fail := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(strings.ReplaceAll(err.Error(), apiKey, "[redacted]"))
		}
	}

	listing, err := modelregistry.List(ctx, nil, provider)
	fail(err)

	get := func(source string) []byte {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		fail(err)
		resp, err := http.DefaultClient.Do(req)
		fail(err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode, source)
		raw, err := io.ReadAll(resp.Body)
		fail(err)
		return raw
	}
	modelsDev, err := modelfacts.ParseModelsDev(get(modelfacts.ModelsDevURL))
	fail(err)
	liteLLM, err := modelfacts.ParseLiteLLM(get(modelfacts.LiteLLMURL))
	fail(err)
	published := modelfacts.Merge(modelsDev, liteLLM)

	serveEnv(t)
	t.Setenv("DIDEROT_ASK_ENABLED", "true")
	t.Setenv("DIDEROT_IMAGES_BUCKET", "gateway-facts-test")
	_, catalog := loadInference(t, provider)
	row := gatewayRow(true, "k")
	row.Resolved = settings.NewResolved(modelprovider.Row("gateway"), settings.Values{
		modelprovider.FieldKind: "openai", modelprovider.FieldBaseURL: baseURL,
	}, map[string]string{modelprovider.FieldKey: "k"})
	catalog.rows = func() []settings.FamilyRow { return []settings.FamilyRow{row} }
	catalog.loadFacts = func(context.Context) (*modelfacts.Catalog, modelfacts.Status, error) {
		return published, modelfacts.Status{modelfacts.SourceModelsDev: {FetchedAt: time.Now()}, modelfacts.SourceLiteLLM: {FetchedAt: time.Now()}}, nil
	}
	menu := modelprovider.Menu{Name: "gateway", Kind: modelregistry.KindOpenAI, Enabled: true, Models: listing, Listing: listing}
	catalog.menus = func(context.Context) ([]modelprovider.Menu, error) { return []modelprovider.Menu{menu}, nil }
	// The saved models are the ones the environment names, each with the
	// entry a new choice starts from.
	saved := settings.Models{Roles: map[string]string{}}
	var ids []string
	for _, role := range modelregistry.Roles {
		id := strings.TrimSpace(os.Getenv(gatewayTestModels[role]))
		if id == "" {
			continue
		}
		saved.Roles[string(role)] = modelregistry.Ref("gateway", id)
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
			saved.Available = append(saved.Available, modelregistry.NewEntry("gateway", id))
		}
	}
	embeddingModel := strings.TrimSpace(os.Getenv(gatewayTestEmbeddingModel))
	if embeddingModel != "" {
		ids = append(ids, embeddingModel)
	}
	if len(ids) == 0 {
		t.Skip("name at least one model (DIDEROT_TEST_MODEL_ASK, ..., DIDEROT_TEST_MODEL_EMBEDDINGS) to run")
	}
	view, err := catalog.facts(ctx, saved, []modelprovider.Menu{menu})
	fail(err)

	var out strings.Builder
	host, _ := url.Parse(baseURL)
	fmt.Fprintf(&out, "# Published facts against the provider at %s\n\n", host.Host)
	fmt.Fprintf(&out, "Run %s. The provider lists %d models; the catalog describes %d (models.dev %d entries, LiteLLM %d dated or sized).\n\n",
		time.Now().UTC().Format(time.RFC3339), len(listing), published.Len(), len(modelsDev.Entries), len(liteLLM.Entries))

	said := func(value *bool, source modelregistry.Source) string {
		if value == nil {
			return "not published"
		}
		return fmt.Sprintf("%s (%s)", yesNo(*value), source)
	}
	fmt.Fprintf(&out, "## Each model named\n\n")
	fmt.Fprintf(&out, "| Model | Listed by the provider as | Catalog entry | Images | Tools | Temperature | Reasoning efforts | Price in/out per M | End |\n|---|---|---|---|---|---|---|---|---|\n")
	slices.Sort(ids)
	for _, id := range ids {
		listed, inListing := menu.Listed(id)
		owner := "not listed"
		if inListing {
			owner = "owned_by " + listed.OwnedBy
		}
		facts, match := view.of("gateway", id)
		entry := "none"
		switch {
		case match.Found:
			entry = match.Entry.Ref() + ": " + match.How
		case len(match.Suggestions) > 0:
			entry = "none certain; suggested: " + match.Suggestions[0].Ref()
		}
		images, imagesBy := facts.Images()
		tools, toolsBy := facts.Tools()
		temperature, temperatureBy := facts.Temperature()
		_, efforts, effortsBy := facts.Reasoning()
		effortText := "not published"
		if effortsBy != "" {
			effortText = fmt.Sprintf("%v (%s)", efforts, effortsBy)
		}
		price := "not published"
		if in, outPrice, by, ok := facts.Price(); ok {
			price = fmt.Sprintf("$%s / $%s (%s)", trimFloat(in), trimFloat(outPrice), by)
		}
		attention, note := view.ending(id, facts, match)
		fmt.Fprintf(&out, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", id, owner, entry,
			said(images, imagesBy), said(tools, toolsBy), said(temperature, temperatureBy), effortText, price, strings.TrimSpace(attention+" "+note))
	}

	fmt.Fprintf(&out, "\n## Each job\n\n")
	fmt.Fprintf(&out, "The entry is the one a choice on the Models page would make today: the published facts, then the job's test.\n\n")
	fmt.Fprintf(&out, "| Job | Model | Test requests | Found by the test | What the job sends |\n|---|---|---|---|---|\n")
	for _, role := range modelregistry.Roles {
		ref := saved.Roles[string(role)]
		if ref == "" {
			continue
		}
		_, id, _ := modelregistry.ParseRef(ref)
		facts, _ := view.of("gateway", id)
		trace := &modelregistry.TestTrace{}
		learned, err := modelregistry.TestForRole(ctx, provider, modelregistry.NewEntry("gateway", id).WithPublished(facts, false), role,
			modelregistry.TestOptions{Trace: trace, PreferMessages: facts.Anthropic})
		result, sends := fmt.Sprintf("%d, passed", trace.Requests), "n/a"
		if err != nil {
			result = fmt.Sprintf("%d, FAILED: %s", trace.Requests, strings.ReplaceAll(err.Error(), apiKey, "[redacted]"))
		} else {
			sends = roleRequests(role, learned)
		}
		fmt.Fprintf(&out, "| %s | %s | %s | %v | %s |\n", roleInfo[role].title, id, result, trace.Learned, sends)
	}

	// The embedding model: its vector size is learned from one embedding
	// call, whatever a catalog says.
	if embeddingModel != "" {
		dims, err := modelregistry.EmbeddingDimensions(ctx, provider, embeddingModel, nil)
		fail(err)
		embedFacts, embedMatch := view.of("gateway", embeddingModel)
		fmt.Fprintf(&out, "\n## Embeddings\n\n%s: catalog match found=%v, published vector size %d; one embedding call answered %d dimensions.\n",
			embeddingModel, embedMatch.Found, embedFacts.Catalog.Dimensions, dims)
	}

	report := os.Getenv("DIDEROT_TEST_FACTS_REPORT")
	if report == "" {
		t.Log("\n" + out.String())
		return
	}
	require.NoError(t, os.WriteFile(report, []byte(out.String()), 0o600))
}

// roleRequests says what role's requests carry with entry: the facts of the
// entry that the role reads.
func roleRequests(role modelregistry.Role, entry modelregistry.Model) string {
	var parts []string
	say := func(what string, value any) { parts = append(parts, fmt.Sprintf("%s: %v", what, value)) }
	switch role {
	case modelregistry.RoleAsk:
		protocol, _ := entry.AgenticProtocol()
		say("protocol", protocol)
		say("service tier", entry.ServiceTier)
		say("prompt cache retention", entry.PromptCacheRetention)
		say("efforts Ask offers", entry.ReasoningEfforts)
	case modelregistry.RoleTitles:
		say("sends effort low", entry.SupportsReasoningEffort("low"))
		say("prompt cache retention", entry.PromptCacheRetention)
	case modelregistry.RoleSessionSummaryMap, modelregistry.RoleSessionSummaryReduce:
		say("answers on messages", entry.Supports(modelregistry.ProtocolMessages))
	default:
		param := string(entry.ChatTokenParam)
		if param == "" {
			param = "max_tokens"
		}
		say("output limit field", param)
		say("sends temperature", !entry.ChatOmitTemperature)
		say("reasoning_effort", entry.ChatReasoningEffort)
		say("reasoning token allowance", entry.ChatReasoningTokenAllowance)
		say("extra request fields", entry.ChatExtraBody)
		say("images", entry.Images)
	}
	return strings.Join(parts, "; ")
}
