// Package discord is the Discord source: it indexes the messages of the
// Discord channels an administrator chooses, in every server the bot is in,
// with their threads and forum posts, from the REST API's history and the
// gateway's live events.
package discord

import (
	"context"
	"errors"
	"fmt"

	"github.com/bluesky-social/diderot/internal/settings"
	"github.com/bluesky-social/diderot/internal/source"
)

// Discord connector fields.
const (
	FieldBotToken    = "bot_token"
	FieldHistoryDays = "history_days"
	FieldDocIngest   = "doc_ingest"
)

// maxHistoryDays bounds the history setting at about ten years.
const maxHistoryDays = 3650

// Connector is the Discord source: its page's fields, their validation,
// and the pre-flight Test; in source.go, its facts, its leader workers,
// its expansion and its channels.
type Connector struct {
	// APIBase replaces DefaultAPIBase. Tests point it at a fake.
	APIBase string
	// FileStorage says the deployment has the file storage Index documents
	// needs (DIDEROT_IMAGES_BUCKET).
	FileStorage bool
}

func (Connector) Name() string { return ConnectorName }

func (Connector) Fields() []settings.Field {
	return []settings.Field{
		{Key: FieldBotToken, Label: "Bot token", Kind: settings.KindSecret, Required: true,
			Help: "From the Bot page of your application in Discord's Developer Portal."},
		{Key: FieldHistoryDays, Label: "Read history from the last", Kind: settings.KindInt, Default: int64(0),
			Help: "Days of history a channel's first read covers. 0 reads all of it."},
		{Key: FieldDocIngest, Label: "Index documents", Kind: settings.KindBool,
			Help: "Extract the text of PDF, Word and Excel files posted in the chosen channels. Needs file storage."},
	}
}

// Validate checks the history setting's range.
func (Connector) Validate(values settings.Values) error {
	days, _ := values[FieldHistoryDays].(int64)
	if days < 0 || days > maxHistoryDays {
		return settings.FieldErrors{FieldHistoryDays: fmt.Sprintf("must be between 0 and %d", maxHistoryDays)}
	}
	return nil
}

// Test signs in as the bot, checks that the Message Content intent is on,
// and counts the servers the bot is in.
func (c Connector) Test(ctx context.Context, resolved settings.Resolved) settings.TestReport {
	var report settings.TestReport
	add := func(check settings.Check) { report.Checks = append(report.Checks, check) }
	token := resolved.Secret(FieldBotToken)
	if token == "" {
		add(settings.Check{Name: "Bot token", Detail: "No bot token is set.", Remedy: "Paste the token from the Bot page of your application in Discord's Developer Portal."})
		return report
	}
	client := NewClient(c.APIBase, token)
	bot, err := client.currentUser(ctx)
	if err != nil {
		add(settings.Check{Name: "Bot token", Detail: "Discord did not accept the token: " + describeErr(err) + ".",
			Remedy: "Reset the token on the Bot page in the Developer Portal and paste the new one."})
		return report
	}
	add(settings.Check{Name: "Bot token", Passed: true, Detail: "Signed in as " + bot.displayName() + "."})
	if !bot.Bot {
		report.Checks[0] = settings.Check{Name: "Bot token", Detail: "The token is not a bot's.", Remedy: "Use the token from the Bot page, not a user token."}
		return report
	}

	app, err := client.currentApplication(ctx)
	switch {
	case err != nil:
		add(settings.Check{Name: "Message Content intent", Detail: "Couldn't read the application: " + describeErr(err) + ".", Remedy: "Test again."})
	case app.Flags&(appFlagMessageContent|appFlagMessageContentLimited) == 0:
		add(settings.Check{Name: "Message Content intent", Detail: "The bot can't read what messages say.",
			Remedy: "Switch on Message Content Intent under Privileged Gateway Intents on the Bot page, then test again."})
	default:
		add(settings.Check{Name: "Message Content intent", Passed: true, Detail: "The bot can read what messages say."})
	}

	guilds, err := client.guilds(ctx)
	switch {
	case err != nil:
		add(settings.Check{Name: "Servers", Detail: "Couldn't list the bot's servers: " + describeErr(err) + ".", Remedy: "Test again."})
	case len(guilds) == 0:
		add(settings.Check{Name: "Servers", Detail: "The bot is in no server.",
			Remedy: "Invite the bot to your server with the link in Diderot's Discord guide, then test again."})
	case len(guilds) == 1:
		add(settings.Check{Name: "Servers", Passed: true, Detail: "The bot is in 1 server: " + guilds[0].Name + "."})
	default:
		add(settings.Check{Name: "Servers", Passed: true, Detail: fmt.Sprintf("The bot is in %d servers.", len(guilds))})
	}
	if resolved.Bool(FieldDocIngest) {
		add(source.DocumentIngestCheck(c.FileStorage))
	}
	return report
}

// errNoToken is Run's error without a token.
var errNoToken = errors.New("no Discord bot token is set")
