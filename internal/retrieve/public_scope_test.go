package retrieve

import (
	"context"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/qdrantstore"
	"github.com/bluesky-social/diderot/internal/scopesync"
	"github.com/stretchr/testify/require"
)

func TestInternetPublicScopeExcludesSlackThroughReclassification(t *testing.T) {
	pool, store, embedder := setupWorld(t)
	ctx := context.Background()
	const question = "support ticket resolution documentation"
	seed := func(source, externalID, scope string) int64 {
		return seedEvent(t, pool, store, embedder, events.Event{
			Source: source, ExternalID: externalID, Scope: scope, Body: question, OccurredAt: time.Now(),
		})
	}
	slackID := seed("slack", "CPUBLICSCOPE:1", events.ScopePublic)
	articleID := seed("zendesk", "article:public", events.ScopePublic)
	privateID := seed("slack", "CPRIVATESCOPE:1", events.ScopeChannel("CPRIVATESCOPE"))
	ownerID := seed("agent_session", "owned-session", events.ScopeOwner("alice@example.com"))
	ticketID := seed("zendesk", "ticket:private", events.ScopeZendesk)
	_, err := pool.Exec(ctx, `INSERT INTO slack_message_details (event_id,channel_id,occurred_at) VALUES ($1,'CPUBLICSCOPE',now())`, slackID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at)
	VALUES ('CPUBLICSCOPE','workspace-public',false,false,now()) ON CONFLICT (channel_id) DO UPDATE SET name=EXCLUDED.name,is_private=false,fetched_at=now()`)
	require.NoError(t, err)
	service := &scopesync.Service{Pool: pool, Store: store}
	repaired, err := service.Reconcile(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, repaired.Events)
	require.EqualValues(t, 1, repaired.Chunks)
	vectors, err := embedder.Embed(ctx, []string{question})
	require.NoError(t, err)
	staleHits, err := store.Search(ctx, vectors[0], qdrantstore.SearchFilters{AllowedScopes: []string{events.ScopePublic}}, 10)
	require.NoError(t, err)
	require.Len(t, staleHits, 2)
	retriever := NewRetriever(pool, embedder, store, nil)
	for _, phase := range []string{"payload repair pending", "payload repair complete"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "payload repair complete" {
				applied, err := service.DrainScope(ctx)
				require.NoError(t, err)
				require.Equal(t, 1, applied)
				publicHits, err := store.Search(ctx, vectors[0], qdrantstore.SearchFilters{AllowedScopes: []string{events.ScopePublic}}, 10)
				require.NoError(t, err)
				require.Len(t, publicHits, 1)
				require.Equal(t, articleID, publicHits[0].EventID)
				internalHits, err := store.Search(ctx, vectors[0], qdrantstore.SearchFilters{AllowedScopes: []string{events.ScopeChannel("CPUBLICSCOPE")}}, 10)
				require.NoError(t, err)
				require.Len(t, internalHits, 1)
				require.Equal(t, slackID, internalHits[0].EventID)
			}
			for scope, wantID := range map[string]int64{
				events.ScopePublic: articleID, events.ScopeChannel("CPUBLICSCOPE"): slackID,
				events.ScopeChannel("CPRIVATESCOPE"): privateID, events.ScopeOwner("alice@example.com"): ownerID,
				events.ScopeZendesk: ticketID,
			} {
				result, err := retriever.Retrieve(ctx, Request{Question: question, Filters: Filters{AllowedScopes: []string{scope}}})
				require.NoError(t, err)
				if phase == "payload repair pending" && scope == events.ScopeChannel("CPUBLICSCOPE") {
					require.Empty(t, result.Hits)
				} else {
					require.Len(t, result.Hits, 1, "scope=%s", scope)
					require.Equal(t, wantID, result.Hits[0].EventID)
				}
				listed, err := retriever.List(ctx, ListRequest{Sources: []string{"slack", "zendesk", "agent_session"}, AllowedScopes: []string{scope}})
				require.NoError(t, err)
				require.Len(t, listed.Hits, 1, "scope=%s", scope)
				require.Equal(t, wantID, listed.Hits[0].EventID)
			}
		})
	}
	reconciled, err := service.Reconcile(ctx)
	require.NoError(t, err)
	require.Zero(t, reconciled.Events)
}

func TestSelectedPublicChannelAndZendeskExcludeOtherCorpora(t *testing.T) {
	pool, store, embedder := setupWorld(t)
	ctx := context.Background()
	const question = "password reset troubleshooting"
	var expected []int64
	for _, fixture := range []struct {
		source, key, scope string
		allowed            bool
	}{
		{"slack", "CAPPROVED:1", events.ScopeChannel("CAPPROVED"), true},
		{"zendesk", "ticket:1", events.ScopeZendesk, true},
		{"slack", "COTHER:1", events.ScopeChannel("COTHER"), false},
		{"notion", "internal-page", events.ScopeInternal, false},
		{"zendesk", "public-article", events.ScopePublic, false},
		{"slack", "CUNKNOWN:1", events.ScopeUnstamped, false},
	} {
		eventID := seedEvent(t, pool, store, embedder, events.Event{Source: fixture.source, ExternalID: fixture.key, Scope: fixture.scope, Body: question, OccurredAt: time.Now()})
		if fixture.allowed {
			expected = append(expected, eventID)
		}
	}
	scopes := []string{events.ScopeZendesk, events.ScopeChannel("CAPPROVED")}
	retriever := NewRetriever(pool, embedder, store, nil)
	result, err := retriever.Retrieve(ctx, Request{Question: question, Filters: Filters{AllowedScopes: scopes}})
	require.NoError(t, err)
	var retrieved []int64
	for _, hit := range result.Hits {
		retrieved = append(retrieved, hit.EventID)
	}
	require.ElementsMatch(t, expected, retrieved)
	listed, err := retriever.List(ctx, ListRequest{Sources: []string{"slack", "zendesk", "notion"}, AllowedScopes: scopes})
	require.NoError(t, err)
	var enumerated []int64
	for _, hit := range listed.Hits {
		enumerated = append(enumerated, hit.EventID)
	}
	require.ElementsMatch(t, expected, enumerated)
}
