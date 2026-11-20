package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/llmclient"
	"github.com/bluesky-social/diderot/internal/modelprovider"
	"github.com/bluesky-social/diderot/internal/modelregistry"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/testdb"
)

func viewOf(t *testing.T, views []setup.RoleView, key string) setup.RoleView {
	t.Helper()
	for _, view := range views {
		if view.Key == key {
			return view
		}
	}
	t.Fatalf("no role %q", key)
	return setup.RoleView{}
}

// TestModelCatalogBootsWithNoModels: with no provider and nothing saved,
// every role is off with no warning, there is no embedding model, and the
// deployment's switches still say which roles it does not run.
func TestModelCatalogBootsWithNoModels(t *testing.T) {
	serveEnv(t)
	_, catalog := loadInference(t)
	models := catalog.live()
	for _, role := range modelregistry.Roles {
		assert.Empty(t, models.role(role).ID, role)
	}
	assert.Empty(t, models.embed.ID)
	views, err := catalog.Roles(t.Context(), settings.Models{})
	require.NoError(t, err)
	for _, view := range views {
		assert.Empty(t, view.Model, view.Key)
		assert.Empty(t, view.Warning, view.Key)
		assert.Empty(t, view.Options, view.Key)
	}
	assert.Equal(t, "Ask is off. Set DIDEROT_ASK_ENABLED=true in the deployment to turn it on.", viewOf(t, views, "ask").Unavailable)
	assert.Equal(t, "No model writes session summaries: choose one for both session summary jobs on the Models page.", catalog.noSummaryModel())
}

// TestModelCatalogResolvesSavedModels: a role uses its saved model while the
// model's entry serves the role and its provider is on; otherwise it is off
// and the page says why. Options come from the enabled providers' menus.
func TestModelCatalogResolvesSavedModels(t *testing.T) {
	recorder := newInferenceRecorder(t, 8)
	serveEnv(t)
	t.Setenv("DIDEROT_ASK_ENABLED", "true")
	_, catalog := loadInference(t, openAIProvider("lab", recorder, "k"))
	catalog.menus = func(context.Context) ([]modelprovider.Menu, error) {
		return []modelprovider.Menu{
			{Name: "lab", Kind: modelregistry.KindOpenAI, Enabled: true, Listing: []modelregistry.Listed{{ID: "big"}, {ID: "old", Retires: "2027-01-01"}},
				Models: []modelregistry.Listed{{ID: "big"}, {ID: "old", Retires: "2027-01-01"}, {ID: "embed", Embedding: true}}},
			{Name: "off", Kind: modelregistry.KindOpenAI, Models: []modelregistry.Listed{{ID: "hidden"}}},
		}, nil
	}
	responses := []modelregistry.Protocol{modelregistry.ProtocolResponses}
	saved := settings.Models{
		Roles: map[string]string{"ask": "lab/big", "titles": "lab/big", "thread_summaries": "off/hidden", "session_summary_map": "lab/old"},
		Available: []modelregistry.Model{
			{Provider: "lab", ID: "big", Protocols: responses, TestedFor: []modelregistry.Role{"ask"}},
			{Provider: "off", ID: "hidden", Protocols: []modelregistry.Protocol{modelregistry.ProtocolChat}},
		},
		Embedding: &settings.Embedding{Provider: "lab", Model: "embed", Dimensions: 8, Collection: "c"},
	}
	models := catalog.resolve(saved)
	assert.Equal(t, "big", models.ask.ID)
	assert.Equal(t, "big", models.title.ID, "titles speak Responses, which the entry has")
	assert.Empty(t, models.summarizer.ID, "its provider is off")
	assert.Equal(t, "embed", models.embed.ID)

	views, err := catalog.Roles(t.Context(), saved)
	require.NoError(t, err)
	ask := viewOf(t, views, "ask")
	assert.Equal(t, "lab/big", ask.Model)
	var refs []string
	for _, option := range ask.Options {
		refs = append(refs, option.Ref)
	}
	assert.Equal(t, []string{"lab/big", "lab/old"}, refs, "every enabled provider's menu, without embedding models")
	assert.True(t, ask.Options[0].Tested)
	assert.False(t, viewOf(t, views, "titles").Options[0].Tested, "the entry passed Ask's test, not titles'")
	assert.Equal(t, "It can't be used: its provider, off, is off or removed.", viewOf(t, views, "thread_summaries").Warning)
	assert.Equal(t, "It has not passed this job's test. Save it for this job again to test it.", viewOf(t, views, "session_summary_map").Warning)

	options, warning, _, err := catalog.Embedding(t.Context(), saved)
	require.NoError(t, err)
	assert.Empty(t, warning)
	assert.Equal(t, "lab/embed", options[0].Ref, "models a listing types as embeddings come first")
}

// TestModelCatalogTestRoleChecksTheRoleCanUseIt: a job's test runs against
// the provider and refuses an entry the job's client can't carry.
func TestModelCatalogTestRoleChecksTheRoleCanUseIt(t *testing.T) {
	recorder := newInferenceRecorder(t, 8)
	serveEnv(t)
	t.Setenv("DIDEROT_ASK_ENABLED", "true")
	_, catalog := loadInference(t, openAIProvider("lab", recorder, "k"))
	entry, err := catalog.TestRole(t.Context(), settings.Models{}, "thread_summaries", "lab/m", "")
	require.NoError(t, err)
	assert.Equal(t, []modelregistry.Protocol{modelregistry.ProtocolChat}, entry.Protocols)
	assert.Equal(t, "/v1/chat/completions", recorder.last().Path)

	conflicting := settings.Models{Available: []modelregistry.Model{{Provider: "lab", ID: "m", ChatExtraBody: map[string]any{"max_tokens": 5}}}}
	_, err = catalog.TestRole(t.Context(), conflicting, "thread_summaries", "lab/m", "")
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "the job can't use it")

	_, err = catalog.TestRole(t.Context(), settings.Models{}, "ask", "gone/m", "")
	require.ErrorIs(t, err, settings.ErrInvalid)
	assert.Contains(t, err.Error(), "its provider, gone, is off or removed")

	dims, err := catalog.EmbeddingDimensions(t.Context(), settings.Models{}, "lab/embed")
	require.NoError(t, err)
	assert.Equal(t, 8, dims)
}

// TestSavedModelChangeRestartsWork: the leader's work runs under
// runPerModels with the models the settings cache holds. A save on another
// replica reaches the Ask model and a worker's client without a process
// restart; a save that changes no role's model, such as Ask's limits,
// restarts nothing; and a provider's new key does.
func TestSavedModelChangeRestartsWork(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.URL(t, "cmd_models"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `DELETE FROM connector_settings WHERE connector = 'models'`)
	require.NoError(t, err)
	store := settings.NewStore(pool, testLogger())
	require.NoError(t, store.Load(ctx))
	other := settings.NewStore(pool, testLogger())
	require.NoError(t, other.Load(ctx))

	provider := newInferenceRecorder(t, 0)
	serveEnv(t)
	t.Setenv("DIDEROT_ASK_ENABLED", "true")
	var lab atomic.Pointer[modelregistry.Provider]
	first := openAIProvider("lab", provider, "sk-1")
	lab.Store(&first)
	_, catalog := loadInference(t)
	catalog.providers = func() []modelregistry.Provider { return []modelregistry.Provider{*lab.Load()} }
	catalog.saved = func() settings.Models { return store.Models().Models }

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go store.Run(runCtx, 10*time.Millisecond)
	type term struct{ ask, title, key string }
	terms := make(chan term, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPerModels(runCtx, testLogger(), "test", catalog.live, sameRoleModels, func(termCtx context.Context, models *inferenceModels) {
			if models.ask.ID != "" {
				mdl, err := ask.NewModel(termCtx, ask.ModelConfig{
					BaseURL: models.ask.endpoint.BaseURL, APIKey: models.ask.endpoint.APIKey,
					Model: models.ask.Model, MaxTokens: models.askLimits.MaxOutputTokens,
				})
				if assert.NoError(t, err) {
					_, err = mdl.Generate(termCtx, []*schema.AgenticMessage{ask.NewAgenticUserText("hello")})
					assert.NoError(t, err)
				}
			}
			terms <- term{models.ask.ID, models.title.ID, models.ask.endpoint.APIKey}
			<-termCtx.Done()
		})
	}()
	next := func() term {
		select {
		case got := <-terms:
			return got
		case <-time.After(10 * time.Second):
			t.Fatal("the work did not start")
			return term{}
		}
	}
	assert.Equal(t, term{}, next(), "nothing saved: every role is off")

	responses := []modelregistry.Protocol{modelregistry.ProtocolResponses}
	actor := settings.Actor{Email: "op@example.com"}
	_, err = other.UpdateModels(ctx, 1, actor, "test", func(models *settings.Models) error {
		models.Roles = map[string]string{"ask": "lab/big", "titles": "lab/big"}
		models.Available = []modelregistry.Model{{Provider: "lab", ID: "big", Protocols: responses}, {Provider: "lab", ID: "small", Protocols: responses}}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, term{"big", "big", "sk-1"}, next(), "another replica's save reaches this one's work")

	_, err = other.UpdateModels(ctx, 2, actor, "test", func(models *settings.Models) error {
		models.AskMaxToolTurns = 12
		return nil
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return store.Models().Version == 3 }, 5*time.Second, 10*time.Millisecond)
	select {
	case got := <-terms:
		t.Fatalf("a limits change restarted the work: %+v", got)
	case <-time.After(2 * modelsWatchInterval):
	}

	rekeyed := first
	rekeyed.Endpoint = llmclient.Endpoint{BaseURL: first.Endpoint.BaseURL, APIKey: "sk-2", Auth: llmclient.AuthBearer}
	rekeyed.Version = 2
	lab.Store(&rekeyed)
	assert.Equal(t, term{"big", "big", "sk-2"}, next(), "a provider's new key rebuilds its models' clients")

	_, err = other.UpdateModels(ctx, 3, actor, "test", func(models *settings.Models) error {
		models.Roles = map[string]string{"ask": "lab/small", "titles": "lab/small"}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, term{"small", "small", "sk-2"}, next())
	cancel()
	<-done
}
