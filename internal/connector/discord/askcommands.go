package discord

import (
	"context"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/askchat"
)

// Interactions: /ask, the two "Summarize thread" message commands, and the
// Stop button. Each must be answered within three seconds, so a slow one
// defers its response first and edits it when done.

func (a *asker) onInteraction(i interaction) {
	ctx := a.run
	switch i.Type {
	case interactionComponent:
		if strings.HasPrefix(i.Data.CustomID, stopPrefix) {
			a.onStop(ctx, i)
		}
	case interactionCommand:
		switch {
		case i.Data.Type == commandChatInput && i.Data.Name == commandAsk:
			a.onAskCommand(ctx, i)
		case i.Data.Type == commandMessage && i.Data.Name == commandSummarize:
			a.onSummarize(ctx, i, false)
		case i.Data.Type == commandMessage && i.Data.Name == commandSummarizePrivately:
			a.onSummarize(ctx, i, true)
		}
	}
}

// respondPrivately answers an interaction with a message only its user
// sees.
func (a *asker) respondPrivately(ctx context.Context, i interaction, text string) {
	err := a.client.respond(ctx, i.ID, i.Token, interactionResponse{Type: responseMessage, Data: &outMessage{
		Content: text, Flags: flagEphemeral, AllowedMentions: noMentions,
	}})
	if err != nil {
		a.logger.Warn("discord ask: answering an interaction failed", "channel", i.ChannelID, "err", err)
	}
}

// defer acknowledges an interaction whose answer comes later, privately
// when private.
func (a *asker) deferResponse(ctx context.Context, i interaction, private bool) bool {
	data := &outMessage{}
	if private {
		data.Flags = flagEphemeral
	}
	if err := a.client.respond(ctx, i.ID, i.Token, interactionResponse{Type: responseDeferredMessage, Data: data}); err != nil {
		a.logger.Warn("discord ask: deferring an interaction failed", "channel", i.ChannelID, "err", err)
		return false
	}
	return true
}

// finishDeferred replaces a deferred response with text.
func (a *asker) finishDeferred(ctx context.Context, i interaction, text string) {
	if _, err := a.client.editOriginal(ctx, a.appID, i.Token, outMessage{Content: text, AllowedMentions: noMentions}); err != nil {
		a.logger.Warn("discord ask: answering an interaction failed", "channel", i.ChannelID, "err", err)
	}
}

// interactionChannel is the channel an interaction was made in, and the
// channel whose permissions and Ask setting it follows: itself, or a
// thread's parent.
func (a *asker) interactionChannel(ctx context.Context, i interaction) (channel, string, error) {
	ch := channel{ID: i.ChannelID}
	if i.Channel != nil && i.Channel.ID != "" {
		ch = *i.Channel
	} else {
		var err error
		if ch, err = a.client.channel(ctx, i.ChannelID); err != nil {
			return channel{}, "", err
		}
	}
	if isThread(ch.Type) {
		return ch, ch.ParentID, nil
	}
	return ch, ch.ID, nil
}

// onStop stops the turn whose placeholder carries the button. Anyone who
// can see the button may press it, as in Slack.
func (a *asker) onStop(ctx context.Context, i interaction) {
	if err := a.client.respond(ctx, i.ID, i.Token, interactionResponse{Type: responseDeferredUpdate}); err != nil {
		a.logger.Warn("discord ask: acknowledging Stop failed", "channel", i.ChannelID, "err", err)
	}
	conv := askchat.Conversation{Place: i.ChannelID, Thread: strings.TrimPrefix(i.Data.CustomID, stopPrefix)}
	a.surface.markStopped(conv)
	a.engine.Stop(ctx, conv)
}

// onAskCommand answers /ask. A public answer goes in a thread started from
// the question (or the thread it was asked in), like a mention's; a
// private one, or one in the bot's DMs, comes back through the interaction
// with what its asker can read.
func (a *asker) onAskCommand(ctx context.Context, i interaction) {
	question := strings.TrimSpace(i.option("question"))
	private := i.option("private") == "true" || i.GuildID == ""
	settings, err := a.settings(ctx)
	if err != nil {
		a.respondPrivately(ctx, i, "I couldn't read my settings; please try again shortly.")
		return
	}
	var ch channel
	parentID := ""
	if i.GuildID == "" {
		if !settings.DirectMessages {
			a.respondPrivately(ctx, i, offInDMs)
			return
		}
	} else {
		if ch, parentID, err = a.interactionChannel(ctx, i); err != nil {
			a.respondPrivately(ctx, i, accessUnknown)
			return
		}
		if !settings.answersIn(i.GuildID, parentID) {
			a.respondPrivately(ctx, i, offInChannel)
			return
		}
	}
	if question == "" {
		a.respondPrivately(ctx, i, "Ask a question.")
		return
	}
	asker := i.user()
	if private {
		a.askPrivately(ctx, i, settings, asker, question)
		return
	}
	if !isThread(ch.Type) && ch.Type != channelText && ch.Type != channelAnnouncement {
		a.respondPrivately(ctx, i, "I answer /ask in text channels and their threads; add private:True to ask here.")
		return
	}
	if !a.deferResponse(ctx, i, false) {
		return
	}
	posted, err := a.client.editOriginal(ctx, a.appID, i.Token, outMessage{
		Content:         "**" + a.surface.Sanitize(askerInfo(asker, i.Member).DisplayName) + "** asked: " + a.surface.Sanitize(question),
		AllowedMentions: noMentions, Flags: flagSuppressEmbeds,
	})
	if err != nil {
		a.logger.Warn("discord ask: showing the question failed", "channel", i.ChannelID, "err", err)
		return
	}
	place, inThread := i.ChannelID, isThread(ch.Type)
	if !inThread {
		thread, err := a.client.startThread(ctx, i.ChannelID, posted.ID, threadName(question))
		if err != nil {
			a.logger.Warn("discord ask: starting a thread failed", "channel", i.ChannelID, "err", err)
			a.finishDeferred(ctx, i, "I couldn't start a thread for the answer: "+describeErr(err)+".")
			return
		}
		place = thread.ID
		a.surface.rememberParent(place, i.ChannelID)
		defer a.surface.forgetParent(place)
	}
	m := message{ID: posted.ID, ChannelID: i.ChannelID, GuildID: i.GuildID, Author: asker, Member: i.Member, Timestamp: snowflakeTime(posted.ID)}
	if m.Timestamp.IsZero() {
		m.Timestamp = time.Now().UTC()
	}
	a.answer(ctx, m, question, place, parentID, inThread, a.channelAccess(i.GuildID, parentID))
}

// askPrivately answers /ask through the interaction, which only its asker
// sees, with what they can read: a new session each time.
func (a *asker) askPrivately(ctx context.Context, i interaction, settings AskSettings, asker user, question string) {
	if !a.deferResponse(ctx, i, true) {
		return
	}
	scopes, isMember := a.memberScopes(ctx, settings, asker.ID)
	if !isMember {
		a.finishDeferred(ctx, i, notAMember)
		return
	}
	conv := askchat.Conversation{Place: i.ChannelID, Thread: "i-" + i.ID}
	// Each private /ask is a conversation of its own, so it needs no
	// place's lock.
	a.surface.mu.Lock()
	a.surface.private[conv] = &privateReply{token: i.Token}
	a.surface.mu.Unlock()
	defer func() {
		a.surface.mu.Lock()
		delete(a.surface.private, conv)
		a.surface.mu.Unlock()
	}()
	a.engine.RunTurn(ctx, askchat.Question{
		Conversation: conv, AskerID: asker.ID, Text: question, Title: question,
		Asker: askerInfo(asker, i.Member), AllowedScopes: scopes, Timeout: interactionTurnTimeout,
	})
}

// onSummarize summarizes the thread of the message the command was run on:
// for the channel, or only for its user.
func (a *asker) onSummarize(ctx context.Context, i interaction, private bool) {
	settings, err := a.settings(ctx)
	if err != nil {
		a.respondPrivately(ctx, i, "I couldn't read my settings; please try again shortly.")
		return
	}
	ch, parentID, err := a.interactionChannel(ctx, i)
	if err != nil {
		a.respondPrivately(ctx, i, accessUnknown)
		return
	}
	if !settings.answersIn(i.GuildID, parentID) {
		a.respondPrivately(ctx, i, offInChannel)
		return
	}
	threadID, threadName := "", ""
	target := i.Data.Resolved.Messages[i.Data.TargetID]
	switch {
	case isThread(ch.Type):
		threadID, threadName = ch.ID, ch.Name
	case target.Thread != nil:
		threadID, threadName = target.Thread.ID, target.Thread.Name
	default:
		a.respondPrivately(ctx, i, notAThread)
		return
	}
	if a.summarizer == nil {
		a.respondPrivately(ctx, i, noSummaries)
		return
	}
	g, err := a.readGuildAccess(ctx, i.GuildID)
	if err != nil {
		a.respondPrivately(ctx, i, accessUnknown)
		return
	}
	clicker := i.user()
	switch {
	case private && (i.Member == nil || !g.memberReads(*i.Member, clicker.ID, parentID)):
		a.respondPrivately(ctx, i, cannotRead)
		return
	case !private && !g.viewersRead(parentID):
		a.respondPrivately(ctx, i, summaryPrivate)
		return
	}
	if !a.deferResponse(ctx, i, private) {
		return
	}
	summary, err := a.summarize(ctx, i.GuildID, threadID, parentID, threadName)
	if err != nil {
		a.logger.Warn("discord ask: summarizing a thread failed", "thread", threadID, "err", err)
		a.finishDeferred(ctx, i, "sorry, I couldn't summarize this thread: "+askchat.ErrorText(err))
		return
	}
	flags := flagSuppressEmbeds
	if private {
		flags |= flagEphemeral
	}
	for n, chunk := range askchat.Split(a.surface.Sanitize(summary), answerChunkLimit) {
		out := outMessage{Content: chunk, AllowedMentions: noMentions, Flags: flags}
		if n == 0 {
			out.Flags = 0
			_, err = a.client.editOriginal(ctx, a.appID, i.Token, out)
		} else {
			_, err = a.client.followUp(ctx, a.appID, i.Token, out)
		}
		if err != nil {
			a.logger.Warn("discord ask: posting a summary failed", "thread", threadID, "err", err)
			return
		}
	}
}

// summarize reads a thread and summarizes it.
func (a *asker) summarize(ctx context.Context, guildID, threadID, parentID, name string) (string, error) {
	var newestFirst []message
	err := a.client.messagesBefore(ctx, threadID, "", func(page []message) (bool, error) {
		newestFirst = append(newestFirst, page...)
		return len(newestFirst) < askchat.SummaryMaxFetchedMessages, nil
	})
	if err != nil {
		return "", err
	}
	if starter, err := a.client.message(ctx, parentID, threadID); err == nil {
		newestFirst = append(newestFirst, starter)
	}
	p := a.r.place(guildID, parentID, "", "")
	var thread []askchat.ThreadMessage
	for n := len(newestFirst) - 1; n >= 0; n-- {
		m := newestFirst[n]
		if !indexedMessageType(m.Type) {
			continue
		}
		thread = append(thread, askchat.ThreadMessage{
			Timestamp: m.Timestamp.UTC().Format(time.RFC3339), UserID: m.Author.ID, Username: authorName(m),
			Text: renderBody(m, p), Pinned: m.Pinned || pinReacted(m),
		})
	}
	thread = askchat.KeepNewestAndPinned(thread, askchat.SummaryMaxMessages)
	transcript, note := askchat.RenderTranscript("Discord thread "+name+" in #"+p.ChannelName, thread)
	summarizeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	summary, _, err := a.summarizer.Summarize(summarizeCtx, transcript)
	if err != nil {
		return "", err
	}
	if note != "" {
		summary += "\n\n_" + note + "_"
	}
	return summary, nil
}

// pinReacted reports whether someone reacted to m with 📌, the team's mark
// of an action item, as in Slack.
func pinReacted(m message) bool {
	for _, r := range m.Reactions {
		if r.Emoji.Name == "📌" {
			return true
		}
	}
	return false
}

func (s *surface) forgetParent(threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.parents, threadID)
}

// SummaryPrompt is the instructions of Discord's thread summaries.
const SummaryPrompt = `You summarize Discord threads for a busy team.

You will receive a Discord thread transcript. Messages are formatted as:
  [index] <display-name>: <text>
Messages marked PINNED were pinned in Discord or flagged by the team with a
pushpin (📌) reaction as action items.

Produce a summary with exactly these sections, in this order:

1. A "Summary" section: 2-5 sentences capturing what the thread is
   about, key decisions made, and where things ended up.
2. An "Action Items" section: one bullet per PINNED message, each
   prefixed with the pushpin emoji and the author, e.g.
   "- 📌 alice: deploy the fix Friday". Preserve the author's intent;
   do not invent, merge, or drop pinned items. If there are no pinned
   messages, write "None — no messages were pinned."

Rules:
- Discord Markdown: **bold** for section headers and "-" bullets for lists.
  No code fences, no tables, no # headings.
- Be concise and factual. Do not speculate beyond the transcript.
- Ignore bot messages unless they carry decision-relevant content.`
