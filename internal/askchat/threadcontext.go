package askchat

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// A turn asked in a thread reads the thread's messages its session has not
// seen as a <thread_context> block. A surface fetches them; rendering them is
// shared.

// PromptTagEscaper escapes angle brackets and ampersands in text embedded
// inside the prompt's XML-like tags (<thread_context> and the rest). Without
// it, a message containing "</thread_context>" could close the block and
// inject instructions before the question.
var PromptTagEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
)

// ThreadContextMax is how many of a thread's messages a turn's context
// holds: the newest. A thread this long is unusual, and its oldest
// messages are likely the least relevant, so the rest are left out with a
// note rather than paged in. 50 messages of ~200 characters is ~10 KB.
const ThreadContextMax = 50

// ContextBodyMax caps one message's text, in runes; a longer text is cut
// with an ellipsis. It mostly guards against a wall of text pasted right
// before the question.
const ContextBodyMax = 4000

// ContextCaptionMax caps the image captions folded under one message, in
// runes, shared by all of its images. A caption is model-written and
// several images could stack up, so a many-image message can't blow the
// context's budget.
const ContextCaptionMax = 1500

// ContextMessage is one thread message a turn's context shows.
type ContextMessage struct {
	At    time.Time
	Actor string
	Body  string
	// Captions are its images' captions, in order.
	Captions []string
}

// RenderThreadContext renders msgs, oldest first, as the turn's thread
// context: one "[time] actor: body" line each, with its image captions
// under it. truncated says older messages were left out. It returns "" for
// no messages.
func RenderThreadContext(msgs []ContextMessage, truncated bool) string {
	if len(msgs) == 0 {
		return ""
	}
	var b strings.Builder
	if truncated {
		b.WriteString("(earlier messages in this thread were omitted)\n")
	}
	for _, m := range msgs {
		body := m.Body
		if utf8.RuneCountInString(body) > ContextBodyMax {
			body = string([]rune(body)[:ContextBodyMax]) + "…"
		}
		// Newlines inside a body are kept; the next message's time
		// prefix separates them.
		fmt.Fprintf(&b, "[%s] %s: %s\n",
			m.At.UTC().Format("Mon 2006-01-02 15:04 UTC"),
			PromptTagEscaper.Replace(m.Actor),
			PromptTagEscaper.Replace(body),
		)
		b.WriteString(CaptionLines(m.Captions))
	}
	return b.String()
}

// CaptionLines renders a message's image captions as indented
// "  [image: <caption>]" blocks, one per image, or "" when there are none.
// Each is escaped for prompt tags, and together they are capped at
// ContextCaptionMax runes; once the budget is spent the rest are dropped
// behind an ellipsis marker. A caption's own continuation lines are
// indented to keep it under its "[image:" opener.
func CaptionLines(captions []string) string {
	if len(captions) == 0 {
		return ""
	}
	var b strings.Builder
	remaining := ContextCaptionMax
	for _, capt := range captions {
		if remaining <= 0 {
			b.WriteString("  [image: …]\n")
			break
		}
		capt = strings.TrimRight(capt, "\n")
		if utf8.RuneCountInString(capt) > remaining {
			capt = string([]rune(capt)[:remaining]) + "…"
		}
		remaining -= utf8.RuneCountInString(capt)
		capt = PromptTagEscaper.Replace(capt)
		capt = strings.ReplaceAll(capt, "\n", "\n  ")
		fmt.Fprintf(&b, "  [image: %s]\n", capt)
	}
	return b.String()
}
