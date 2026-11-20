package slack

import (
	"context"
	"testing"

	"github.com/slack-go/slack"

	"github.com/bluesky-social/diderot/internal/events"
)

func TestStampScopeDMRequiresVerifiedInternalHuman(t *testing.T) {
	pool := getConsumerPool(t)
	tests := []struct {
		name      string
		user      *slack.User
		wantScope string
	}{
		{
			name:      "internal human",
			user:      slackUserWithEmail("U_INTERNAL", "Avery@Example.com"),
			wantScope: events.ScopeOwner("avery@example.com"),
		},
		{
			name: "external user",
			user: func() *slack.User {
				u := slackUserWithEmail("U_EXTERNAL", "vendor@example.com")
				u.IsStranger = true
				return u
			}(),
			wantScope: events.ScopeUnstamped,
		},
		{
			name: "bot",
			user: func() *slack.User {
				u := slackUserWithEmail("U_BOT", "bot@example.com")
				u.IsBot = true
				return u
			}(),
			wantScope: events.ScopeUnstamped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := newDirectoryWithAPI(pool, &fakeAPI{users: map[string]*slack.User{
				tt.user.ID: tt.user,
			}})
			ev := &events.Event{ActorID: tt.user.ID}

			stampScope(context.Background(), dir, nil, "D_TEST", ev)

			if ev.Scope != tt.wantScope {
				t.Fatalf("scope = %q, want %q", ev.Scope, tt.wantScope)
			}
			if tt.wantScope == events.ScopeUnstamped && ev.ActorCanonical != "" {
				t.Fatalf("actor canonical = %q, want empty", ev.ActorCanonical)
			}
		})
	}
}

func slackUserWithEmail(userID, email string) *slack.User {
	u := &slack.User{ID: userID, Name: userID}
	u.Profile.Email = email
	return u
}

func TestStampScopeChannelsAreNotInternetPublic(t *testing.T) {
	pool := getConsumerPool(t)
	ctx := context.Background()
	publicChannel := chanWith("CSCOPEPUBLIC", "workspace-public", false, false)
	privateChannel := chanWith("CSCOPEPRIVATE", "private", true, false)
	directory := newDirectoryWithAPI(pool, &fakeAPI{channels: map[string]*slack.Channel{
		publicChannel.ID: &publicChannel, privateChannel.ID: &privateChannel,
	}})
	for _, test := range []struct {
		name      string
		channel   string
		directory *Directory
		want      string
	}{
		{name: "workspace public", channel: publicChannel.ID, directory: directory, want: events.ScopeChannel(publicChannel.ID)},
		{name: "private", channel: privateChannel.ID, directory: directory, want: events.ScopeChannel(privateChannel.ID)},
		{name: "unknown channel", channel: "CSCOPEUNKNOWN", directory: directory, want: events.ScopeUnstamped},
		{name: "missing directory", channel: publicChannel.ID, want: events.ScopeUnstamped},
		{name: "missing channel", directory: directory, want: events.ScopeUnstamped},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := &events.Event{Scope: events.ScopePublic}
			stampScope(ctx, test.directory, nil, test.channel, event)
			if event.Scope != test.want {
				t.Fatalf("scope=%q want=%q", event.Scope, test.want)
			}
		})
	}
}
