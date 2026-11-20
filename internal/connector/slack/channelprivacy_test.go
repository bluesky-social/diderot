package slack

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/stretchr/testify/require"
)

func TestChannelPrivacyEventsUpdateAuthorization(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at)
		VALUES ('CTARGET','target',false,false,now())`)
	require.NoError(t, err)
	consumer := newTestConsumer(t, pool)
	public := NewPublicScopeResolver(pool, nil)
	public.ttl = 24 * time.Hour
	consumer.channelAuthorizer.PublicScopes = public
	private := chanWith("CTARGET", "target", true, false)
	api := &fakeAPI{channels: map[string]*slack.Channel{"CTARGET": &private}}
	consumer.dir = newDirectoryWithAPI(pool, api)
	consumer.blacklist = NewIngestBlacklist(pool, nil)
	_, err = consumer.blacklist.Add(ctx, "CTARGET", "target", "privacy events must still run", "test")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(ctx, `DELETE FROM slack_ingest_blacklist WHERE channel_id='CTARGET'`)
		require.NoError(t, err)
	})
	consumer.seen = newSeenStore(time.Hour)
	consumer.seen.MarkOrSeen("CTARGET", "1700000022.000100")
	require.Contains(t, public.Scopes(ctx), events.ScopeChannel("CTARGET"))

	for _, isPrivate := range []bool{true, false} {
		subtype := "channel_convert_to_public"
		if isPrivate {
			subtype = "channel_convert_to_private"
		}
		private.IsPrivate = isPrivate
		payload := fmt.Sprintf(`{"type":"event_callback","event":{"type":"message","subtype":%q,
			"channel":"CTARGET","user":"UACTOR","ts":"1700000022.000100","text":"changed privacy"}}`, subtype)
		event, err := slackevents.ParseEvent([]byte(payload), slackevents.OptionNoVerifyToken())
		require.NoError(t, err)
		consumer.handleEventsAPI(ctx, event)
		var actual bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT is_private FROM slack_channel_info WHERE channel_id='CTARGET'`).Scan(&actual))
		require.Equal(t, isPrivate, actual)
		access := consumer.channelAuthorizer.ResolveChannel(ctx, "COTHER")
		if isPrivate {
			require.NotContains(t, access.AllowedScopes, events.ScopeChannel("CTARGET"))
		} else {
			require.Contains(t, access.AllowedScopes, events.ScopeChannel("CTARGET"))
		}
	}
	require.EqualValues(t, 2, api.chanCalls.Load())
	var ingested int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM slack_message_details WHERE channel_id='CTARGET'`).Scan(&ingested))
	require.Zero(t, ingested)
}

func TestChannelPrivacyEventFailureRemainsPrivate(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at)
		VALUES ('CTARGET','target',false,false,now())`)
	require.NoError(t, err)
	consumer := newTestConsumer(t, pool)
	consumer.dir = newDirectoryWithAPI(pool, &fakeAPI{})
	public := NewPublicScopeResolver(pool, nil)
	consumer.channelAuthorizer.PublicScopes = public
	require.Contains(t, public.Scopes(ctx), events.ScopeChannel("CTARGET"))
	for _, subtype := range []string{"channel_convert_to_private", "channel_convert_to_public"} {
		consumer.handleEventsAPI(ctx, wrapMessageEvent(t, fmt.Sprintf(
			`{"type":"message","subtype":%q,"channel":"CTARGET","ts":"1700000022.000100"}`, subtype)))
		var isPrivate bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT is_private FROM slack_channel_info WHERE channel_id='CTARGET'`).Scan(&isPrivate))
		require.True(t, isPrivate)
		require.NotContains(t, public.Scopes(ctx), events.ScopeChannel("CTARGET"))
	}
}

func TestDelayedPublicEventUsesCurrentPrivacy(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	private := chanWith("CTARGET", "target", true, false)
	consumer := newTestConsumer(t, pool)
	consumer.dir = newDirectoryWithAPI(pool, &fakeAPI{channels: map[string]*slack.Channel{"CTARGET": &private}})
	consumer.handleEventsAPI(ctx, wrapMessageEvent(t,
		`{"type":"message","subtype":"channel_convert_to_public","channel":"CTARGET","ts":"1700000022.000100"}`))
	var isPrivate bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT is_private FROM slack_channel_info WHERE channel_id='CTARGET'`).Scan(&isPrivate))
	require.True(t, isPrivate)
}

func TestPrivateEventOverridesStalePublicMetadata(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	publicInfo := chanWith("CTARGET", "target", false, false)
	consumer := newTestConsumer(t, pool)
	consumer.dir = newDirectoryWithAPI(pool, &fakeAPI{channels: map[string]*slack.Channel{"CTARGET": &publicInfo}})
	publicScopes := NewPublicScopeResolver(pool, nil)
	publicScopes.ttl = 24 * time.Hour
	consumer.channelAuthorizer.PublicScopes = publicScopes
	consumer.handleEventsAPI(ctx, wrapMessageEvent(t,
		`{"type":"message","subtype":"channel_convert_to_private","channel":"CTARGET","ts":"1700000022.000100"}`))
	var isPrivate bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT is_private FROM slack_channel_info WHERE channel_id='CTARGET'`).Scan(&isPrivate))
	require.True(t, isPrivate)
	require.Empty(t, publicScopes.Scopes(ctx))
	require.False(t, publicInfo.IsPrivate)
	consumer.handleEventsAPI(ctx, wrapMessageEvent(t,
		`{"type":"message","subtype":"channel_convert_to_public","channel":"CTARGET","ts":"1700000023.000100"}`))
	require.Contains(t, publicScopes.Scopes(ctx), events.ScopeChannel("CTARGET"))
}

func TestChannelNameRemainsBestEffortWhenPrivacyWriteFails(t *testing.T) {
	pool := getWarmPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		CREATE FUNCTION reject_channel_info_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test cache write failure'; END; $$;
		CREATE TRIGGER reject_channel_info_write BEFORE INSERT OR UPDATE ON slack_channel_info
		FOR EACH ROW EXECUTE FUNCTION reject_channel_info_write();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(ctx, `DROP TRIGGER reject_channel_info_write ON slack_channel_info; DROP FUNCTION reject_channel_info_write()`)
		require.NoError(t, err)
	})
	info := chanWith("CTARGET", "target", false, false)
	directory := newDirectoryWithAPI(pool, &fakeAPI{channels: map[string]*slack.Channel{"CTARGET": &info}})
	name, err := directory.LookupChannel(ctx, "CTARGET")
	require.NoError(t, err)
	require.Equal(t, "target", name)
	_, err = directory.refreshChannel(ctx, "CTARGET", false)
	require.ErrorContains(t, err, "test cache write failure")
	_, err = directory.refreshChannel(ctx, "CTARGET", true)
	require.ErrorContains(t, err, "test cache write failure")
	require.Empty(t, NewPublicScopeResolver(pool, nil).Scopes(ctx))
}

type privacyRaceAPI struct {
	*fakeAPI
	beforeReturn func()
}

func (api *privacyRaceAPI) GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error) {
	api.beforeReturn()
	return api.fakeAPI.GetConversationInfoContext(ctx, input)
}

func (api *privacyRaceAPI) GetConversationsContext(ctx context.Context, input *slack.GetConversationsParameters) ([]slack.Channel, string, error) {
	api.beforeReturn()
	return api.fakeAPI.GetConversationsContext(ctx, input)
}

func (api *privacyRaceAPI) GetUsersContext(context.Context, ...slack.GetUsersOption) ([]slack.User, error) {
	return nil, nil
}

func TestChannelPrivacyRejectsOlderDirectoryObservations(t *testing.T) {
	for _, path := range []string{"info", "name", "warm"} {
		t.Run(path, func(t *testing.T) {
			pool := getWarmPool(t)
			ctx := context.Background()
			_, err := pool.Exec(ctx, `INSERT INTO slack_channel_info (channel_id,name,is_private,is_archived,fetched_at)
				VALUES ('CTARGET','target',false,false,now()-interval '8 days')`)
			require.NoError(t, err)
			public := chanWith("CTARGET", "target", false, false)
			api := &privacyRaceAPI{
				fakeAPI: &fakeAPI{channels: map[string]*slack.Channel{"CTARGET": &public}},
				beforeReturn: func() {
					_, err := pool.Exec(ctx, `UPDATE slack_channel_info SET is_private=true, fetched_at=clock_timestamp() WHERE channel_id='CTARGET'`)
					require.NoError(t, err)
				},
			}
			directory := newDirectoryWithAPI(pool, api)
			switch path {
			case "info":
				_, err = directory.LookupChannel(ctx, "CTARGET")
			case "name":
				_, err = directory.LookupChannelByName(ctx, "target")
			case "warm":
				_, err = WarmChannels(ctx, api, nil, pool)
			}
			require.NoError(t, err)
			var isPrivate bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT is_private FROM slack_channel_info WHERE channel_id='CTARGET'`).Scan(&isPrivate))
			require.True(t, isPrivate)
		})
	}
}
