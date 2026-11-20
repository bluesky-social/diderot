package ask

import (
	"encoding/json"
	"strings"
	"time"
)

// AskerInfo is transport-neutral asker context supplied by the calling
// surface on every turn. Used to format the per-turn <context>...</context>
// envelope that gives the model:
//
//   - the asker's identity (so "I" / "my" / "me" route to a specific
//     person rather than guessing it's the operator),
//   - the asker's local time (so "today" / "yesterday" / "last week"
//     resolve relative to the asker's frame, not the server's), and
//   - the canonical actor key (asker.Email) the model should pass to
//     retrieve(actors=[...]) for "find my X" questions.
//
// nil AskerInfo means the calling surface has no usable identity. The
// formatter degrades to a UTC-only block in that case.
//
// Optional fields degrade gracefully: JSON omits missing values instead of
// collapsing several identity facts into one presentation string.
type AskerInfo struct {
	// DisplayName is the human-readable name (Slack profile.real_name
	// or fallback).
	DisplayName string
	// Handle is the short slack handle (profile.display_name). May be
	// equal to DisplayName for users who haven't set a separate
	// display_name; both remain distinct facts in the JSON object.
	Handle string
	// Email is the asker's slack-registered email. Used as the
	// canonical actor key for retrieve(actors=[...]) lookups
	// (the actors filter matches events.actor_canonical as well as
	// the display name). Empty drops the email field.
	Email string
	// SlackUserID is the asker's Slack user ID (e.g. "U0EXAMPLE01").
	// Rendered in its own namespaced JSON field.
	SlackUserID string
	// LocalTime is now() in the asker's TZ. .Location() carries the
	// IANA name. Zero value means TZ unknown, so the time object emits
	// UTC only.
	LocalTime time.Time
	// TZ is the IANA timezone name that produced LocalTime. Keeping it
	// explicit makes the model-facing JSON unambiguous even when a
	// time.Location has only a short display abbreviation.
	TZ string
	// TZLabel is the human-readable tz name from Slack (e.g.
	// "Pacific Daylight Time"). Display only.
	TZLabel string
	// Viewing is the Slack channel the asker has open while asking in a
	// direct message, when the turn may read it. nil leaves it out.
	Viewing *ViewedChannel
}

// ViewedChannel is a Slack channel by ID and, when known, its name.
type ViewedChannel struct {
	ID, Name string
}

type viewingContext struct {
	SlackChannelID string `json:"slack_channel_id"`
	SlackChannel   string `json:"slack_channel,omitempty"`
}

type askerIdentityContext struct {
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	SlackUserID string `json:"slack_user_id,omitempty"`
	SlackHandle string `json:"slack_handle,omitempty"`
}

type askerTimeContext struct {
	Local         string `json:"local,omitempty"`
	Timezone      string `json:"timezone,omitempty"`
	TimezoneLabel string `json:"timezone_label,omitempty"`
	UTC           string `json:"utc"`
}

type turnContext struct {
	Asker   *askerIdentityContext `json:"asker,omitempty"`
	Viewing *viewingContext       `json:"viewing,omitempty"`
	Time    askerTimeContext      `json:"time"`
}

// formatContextEnvelope wraps the user input in a compact
// <context>...</context><question>...</question> envelope carrying only
// per-turn facts. The stable instructions for interpreting those facts live
// in the system prompt, so repeating them here would spend the first tokens
// of every turn on prose the model has already seen.
//
// threadContext, when non-empty, is a pre-rendered timestamped
// transcript of thread messages the session has not yet seen — either
// messages before the first @-mention (first-touch) or messages posted
// between diderot's turns (follow-up). It is injected as a
// <thread_context>...</thread_context> block between </context> and
// <question>, with framing prose that tells the model to treat it as
// background. Empty threadContext produces byte-identical output to the
// no-context format (no block emitted) — so turns with no interleaved
// chatter are unaffected, and the provider's prompt cache prefix is not
// disrupted (the thread_context block is in the user message, not the
// system blocks, so it only affects the specific turn that has it).
//
// now is the wall clock; injectable so tests can pin a fixed moment
// rather than relying on time.Now. Non-test callers pass time.Now().
//
// The user's literal input is wrapped in <question>...</question> so
// the model can clearly distinguish injected metadata from what the
// user typed. Anthropic models reliably treat structural tags as
// input-only and don't echo them back in answers.
func formatContextEnvelope(asker *AskerInfo, now time.Time, threadContext, userInput string) string {
	var b strings.Builder
	b.WriteString("<context>\n")
	utc := now.UTC()
	contextValue := turnContext{
		Time: askerTimeContext{UTC: utc.Format(time.RFC3339)},
	}
	if asker != nil && (asker.DisplayName != "" || asker.Email != "" ||
		asker.SlackUserID != "" || asker.Handle != "") {
		contextValue.Asker = &askerIdentityContext{
			DisplayName: asker.DisplayName,
			Email:       asker.Email,
			SlackUserID: asker.SlackUserID,
			SlackHandle: asker.Handle,
		}
	}
	if asker != nil && asker.Viewing != nil && asker.Viewing.ID != "" {
		contextValue.Viewing = &viewingContext{SlackChannelID: asker.Viewing.ID}
		if asker.Viewing.Name != "" {
			contextValue.Viewing.SlackChannel = "#" + asker.Viewing.Name
		}
	}
	if asker != nil && !asker.LocalTime.IsZero() {
		// Rich form: both asker's local AND UTC. The model uses
		// asker-local for relative-time interpretation; UTC anchors
		// the corpus filter (events.occurred_at is UTC).
		//
		// Take the asker's location off LocalTime but project the
		// caller-supplied `now` into it. asker.LocalTime is captured
		// at buildAskerInfo time (call site) and could be a few
		// hundred ms older than `now` (formatter call site). For a
		// minute-precision render that's almost never visible — but
		// when the gap straddles a minute boundary you'd see "asker:
		// 09:00 / UTC: 16:01" which looks wrong. Re-project to
		// guarantee both lines render the same instant.
		local := now.In(asker.LocalTime.Location())
		tz := asker.TZ
		if tz == "" {
			tz = local.Location().String()
		}
		tzLabel := asker.TZLabel
		if tzLabel == "" {
			tzLabel = local.Format("MST")
		}
		contextValue.Time.Local = local.Format(time.RFC3339)
		contextValue.Time.Timezone = tz
		contextValue.Time.TimezoneLabel = tzLabel
	}
	// turnContext contains only strings and nested structs, so encoding cannot
	// fail unless the type is later changed to include a custom marshaler.
	encoded, _ := json.Marshal(contextValue)
	b.Write(encoded)
	b.WriteString("\n")
	b.WriteString("</context>\n\n")
	if threadContext != "" {
		b.WriteString("<thread_context>\n")
		b.WriteString("Slack thread background, oldest first:\n")
		b.WriteString(threadContext)
		b.WriteString("</thread_context>\n\n")
	}
	b.WriteString("<question>\n")
	b.WriteString(userInput)
	b.WriteString("\n</question>")
	return b.String()
}
