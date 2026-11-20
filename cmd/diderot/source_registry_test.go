package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/adminui"
	"github.com/bluesky-social/diderot/internal/ask"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/identity"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/setup"
	"github.com/bluesky-social/diderot/internal/source"
)

// TestSourceRegistryIsComplete: every source this binary registers has the
// facts the tools, pages, identity and expand read (the registry refuses
// one without) and is a connector its pages and supervisor know. The source
// surface golden pins which sources there are, in order.
func TestSourceRegistryIsComplete(t *testing.T) {
	var sources source.Registry
	require.NotPanics(t, func() { sources = sourceRegistry(nil, nil, nil, nil, nil, nil, false) })
	var connectors []string
	for _, def := range connectorRegistry(sources, toolRegistry(), automationRegistry(&config.Config{}, nil)).Definitions() {
		connectors = append(connectors, def.Name())
	}
	require.Subset(t, connectors, sources.Names())
	for _, src := range sources.Sources() {
		if provider, ok := src.(source.SectionProvider); ok {
			require.True(t, adminui.HasSection(provider.Section()), "the admin UI renders %s's section", src.Name())
		}
	}
}

// notesSource is a source written only against the Source interface: notes
// from a folder, which expand to their stored body.
type notesSource struct{}

func (notesSource) Name() string                   { return "notes" }
func (notesSource) Fields() []settings.Field       { return nil }
func (notesSource) Validate(settings.Values) error { return nil }
func (notesSource) Test(context.Context, settings.Resolved) settings.TestReport {
	return settings.TestReport{Checks: []settings.Check{{Name: "Folder", Passed: true}}}
}
func (notesSource) Info() source.Info {
	return source.Info{
		EventSource: "note", Title: "Notes", ResultNoun: "Note", Summary: "Notes from a folder.",
		ToolDescription: "plain-text notes from a mounted folder.",
		ActorRole:       "note_author", IdentityNamespace: func(string) string { return "note_writer" },
	}
}
func (notesSource) Run(ctx context.Context, _ source.Deps, _ settings.Resolved) error {
	<-ctx.Done()
	return nil
}
func (notesSource) Expand(ctx context.Context, deps source.Deps, eventID int64, _ []string) (source.Expanded, error) {
	stored, err := deps.Sink.StoredBody(ctx, eventID)
	if err != nil {
		return source.Expanded{}, err
	}
	return source.Expanded{Content: "note: " + stored.Body}, nil
}

// TestAddingASourceReachesEveryConsumer: registering one more source, and
// touching nothing else, puts it in the corpus tools' sources filters and
// progress line, actor identity, expand dispatch, the connector registry,
// the setup checklist, and {{SOURCES}}.
func TestAddingASourceReachesEveryConsumer(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_source_registry")
	registered := sourceRegistry(pool, nil, nil, nil, nil, nil, false)
	sources := source.NewRegistry(append(registered.Sources(), notesSource{})...)
	infos := sources.Infos()

	// The retrieve and list tools offer it as a filter and say what it
	// holds.
	for _, registration := range []ask.RegisteredTool{ask.NewEinoRetrieveTool(nil, infos), ask.NewEinoListTool(nil, infos)} {
		info, err := registration.Tool.Info(ctx)
		require.NoError(t, err)
		parameters, err := info.ToJSONSchema()
		require.NoError(t, err)
		raw, err := json.Marshal(parameters)
		require.NoError(t, err)
		var schema struct {
			Properties struct {
				Sources struct {
					Items struct {
						Enum []string `json:"enum"`
					} `json:"items"`
					Description string `json:"description"`
				} `json:"sources"`
			} `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(raw, &schema))
		require.Equal(t, append(registered.Infos().EventSources(), "note"), schema.Properties.Sources.Items.Enum, info.Name)
		require.Contains(t, schema.Properties.Sources.Description, "'note' = plain-text notes from a mounted folder.", info.Name)
	}
	output, err := json.Marshal(map[string]any{"hits": []map[string]any{{"source": "note", "url": "https://example.test/note"}}})
	require.NoError(t, err)
	require.Equal(t, "Note", ask.NewEinoRetrieveTool(nil, infos).Presentation.StatusSources(output)[0].Label)

	// Its events expand through its own Expand, with its actors projected.
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	written, err := sink.Upsert(ctx, events.Event{
		Scope: events.ScopeInternal, Source: "note", ExternalID: "notes/ideas.txt", OccurredAt: time.Now().UTC(),
		Actor: "Alex", ActorID: "alex", Body: "ship the folder source",
	})
	require.NoError(t, err)
	expanded, err := newSourceExpander(sources, source.Deps{Pool: pool, Sink: sink}).
		Expand(ask.WithAllowedScopes(ctx, []string{events.ScopeInternal}), written.EventID)
	require.NoError(t, err)
	require.Equal(t, "note: ship the folder source", expanded.Content)
	require.Equal(t, identity.Key{Namespace: "note_writer", ID: "alex"}, expanded.Actor.SourceIdentity)
	require.Equal(t, "note_author", expanded.Actor.Role)

	// It is a connector, and the setup checklist counts it as a source.
	store := settings.NewStore(pool, discardLogger()).WithConnectors(connectorRegistry(sources, toolRegistry(), automationRegistry(&config.Config{}, nil)), settings.Secrets{})
	require.NoError(t, store.Load(ctx))
	_, err = pool.Exec(ctx, `UPDATE connector_settings SET enabled = true, version = version + 1 WHERE connector = 'notes'`)
	require.NoError(t, err)
	require.NoError(t, store.Load(ctx))
	checklist, err := setup.New(pool, store, setup.Registries{Sources: sources}, setup.Facts{}, nil, nil).Checklist(ctx)
	require.NoError(t, err)
	require.Equal(t, "On: Notes.", checklist.Step(setup.StepSources).Status)

	// {{SOURCES}} lists it after the others.
	list := infos.PromptList()
	require.True(t, strings.HasSuffix(list, "\n- `note` — plain-text notes from a mounted folder."), list)
	base, err := promptcatalog.MustEmbedded().Render(map[string]string{promptcatalog.VarSources: list}).Text(promptcatalog.AskBase)
	require.NoError(t, err)
	require.Contains(t, base, "- `note` — plain-text notes from a mounted folder.")
}

// TestExpandChecksScopesForEverySource: the expand dispatcher checks every
// event against the caller's scopes before its source reads it, so a source
// with no check of its own, like notesSource, still can't expand an event
// outside them, and the answer doesn't say whose event it was.
func TestExpandChecksScopesForEverySource(t *testing.T) {
	ctx := t.Context()
	pool := ingestTestPool(t, "cmd_expand_scopes")
	sink := events.NewDBSink(pool, prometheus.NewRegistry())
	written, err := sink.Upsert(ctx, events.Event{
		Scope: "slack-channel:CPRIVATE", Source: "note", ExternalID: "notes/private.txt", OccurredAt: time.Now().UTC(),
		Actor: "Alex", ActorID: "alex", Body: "a private note",
	})
	require.NoError(t, err)
	expander := newSourceExpander(source.NewRegistry(notesSource{}), source.Deps{Pool: pool, Sink: sink})

	for name, scopes := range map[string][]string{
		"other scopes": {events.ScopePublic, events.ScopeInternal, "slack-channel:COTHER"},
		"no scopes":    nil,
	} {
		result, err := expander.Expand(ask.WithAllowedScopes(ctx, scopes), written.EventID)
		require.ErrorIs(t, err, ask.ErrExpandEventNotFound, name)
		require.Equal(t, ask.ExpandResult{}, result, "%s: nothing about the event", name)
	}

	result, err := expander.Expand(ask.WithAllowedScopes(ctx, []string{events.ScopeInternal, "slack-channel:CPRIVATE"}), written.EventID)
	require.NoError(t, err)
	require.Equal(t, "note: a private note", result.Content)
}
