package slack

import "strings"

// sanitizeSlackMarkdown neutralizes the Slack <...> forms that can
// trigger a broadcast or a user/group notification when they appear
// in untrusted text (model output, user messages, or tool-provided
// summaries). Ordinary Markdown, code, standard Slack links, and
// channel references are preserved verbatim.
//
// This runs at the render boundary for model/user-generated content,
// before it is handed to slack.MsgOptionText or Markdown blocks, so
// the text fallback and any Block Kit copy always carry identical,
// safe content. Trusted application-generated messages (fixed
// templates, <#channelID> refs, operator <@U...> mentions built from
// config) are composed by code directly and are NOT routed through
// here, so their intentional mentions keep working.
//
// Dangerous forms are replaced with plain "@..." text: the reader
// still sees who/what was referenced, only the notification behavior
// is removed.
//
// # Implementation
//
// This is a hand-written, single left-to-right pass over the bytes
// that never re-scans anything it has already emitted. Two earlier
// regex-based revisions each failed a different way:
//
//  1. Each dangerous form matched as its own independent full-string
//     regex pass. A stray '<' left behind by one pass could recombine
//     with a later pass's own "@..." replacement into a brand-new,
//     well-formed "<@...>" span (reported by a property test).
//  2. Making each form's regex absorb a leading run of '<' (`<+`)
//     fixed that, but the surrounding loop retried the match at every
//     position across a long run of '<' that was not in fact followed
//     by a dangerous form, so a run of n '<' cost O(n) attempts of
//     O(n) each: superquadratic, and reachable from uncapped caller
//     input (the HTTP /ask question, the MCP retrieve/ask question,
//     and any model-authored answer, summary, or scheduled report).
//
// This version counts a run of '<' once (so it is consumed, or
// rejected, in one step, never retried position by position) and
// classifies what follows it by looking ahead a bounded distance: a
// Slack id, label, or URL cannot contain '<', '>', or a newline, so
// maxTokenLen caps how far any single classification attempt scans
// before giving up. Both bounds together make the whole function
// linear in the length of its input, however it is constructed.
//
// At each '<' (or run of them):
//   - If what follows opens a dangerous form ('!' or '@') and a
//     complete, well-formed form follows within maxTokenLen bytes, the
//     entire run plus the form is replaced with plain "@..." (or, for
//     the harmless-but-still-"<!" date directive, the label/fallback
//     text with no "@" prefix, since a date is not a mention).
//   - If what follows opens a dangerous form but no complete form is
//     found (a missing id, or content that runs into a newline or
//     another '<' before a closing '>'), only the single '<'
//     immediately before '!'/'@' is neutralized, to the HTML entity
//     "&lt;" — never leaving a raw "<!" or "<@" in the output, which
//     is this function's unconditional postcondition — while any
//     earlier '<' in the same run is ordinary text and passes through
//     raw. ("&lt;" rather than a zero-width space: Slack scans raw
//     message text for "<...>" control sequences before it unescapes
//     HTML entities for display, so "&lt;" can never be mistaken for
//     the start of a new token, while Slack still renders it to the
//     reader as a literal "<". A zero-width space would depend on
//     undocumented behavior of Slack's tokenizer, where the entity is
//     Slack's own documented mechanism for a literal '<'.)
//   - Otherwise (a link, a channel reference, or a '<' that cannot
//     start any known form) only the last '<' of the run is eligible
//     to open one of the two verbatim-preserved forms (a link or a
//     channel reference); any earlier '<' in the run is literal. A
//     '<' that cannot start any form at all — not a dangerous one, not
//     a preserved one — is always left exactly as it is: this is what
//     keeps code (`i < n`, `v := <-ch`), comparisons (`a <= b`, "is 5 >
//     3?"), generics (`List<String>`), and placeholders (`<namespace>`)
//     byte-for-byte verbatim.
//
// Dangerous-form keywords ("channel", "here", "everyone", "subteam",
// "date") are matched case-insensitively: nothing in this codebase or
// Slack's docs says its client ignores a mixed-case "<!CHANNEL>", and
// treating it as live either way is the safe assumption.
const maxTokenLen = 256

func sanitizeSlackMarkdown(s string) string {
	if !strings.Contains(s, "<") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i:] // s[0] == '<'

		// Count the whole run of '<' once; nothing below ever rescans
		// it a character at a time.
		run := 1
		for run < len(s) && s[run] == '<' {
			run++
		}
		tail := s[run:]

		if len(tail) > 0 && (tail[0] == '!' || tail[0] == '@') {
			if repl, n, ok := matchDangerousForm(tail); ok {
				b.WriteString(repl)
				s = tail[n:]
				continue
			}
			// Malformed: an id-less "<@", or a keyword whose id/label
			// runs into a newline or another '<' before a closing '>'.
			// Only the single '<' touching '!'/'@' is neutralized; any
			// earlier '<' in the run is ordinary text.
			if run > 1 {
				b.WriteString(s[:run-1])
			}
			b.WriteString("&lt;")
			s = tail
			continue
		}

		if n, ok := matchPreservedForm(tail); ok {
			if run > 1 {
				b.WriteString(s[:run-1])
			}
			b.WriteByte('<')
			b.WriteString(tail[:n])
			s = tail[n:]
			continue
		}

		// Nothing in this run opens any known form: all of it is
		// ordinary text.
		b.WriteString(s[:run])
		s = tail
	}
}

// matchPreservedForm recognizes the two forms this function preserves
// verbatim — a Slack link/mailto link or a channel reference — at the
// start of tail (which does not include the form's opening '<': the
// caller supplies that separately, since only the last '<' of a run
// can open one of these). It returns the number of bytes of tail the
// form occupies (its content plus the closing '>'), or ok=false if
// tail does not open one of these forms within maxTokenLen bytes.
func matchPreservedForm(tail string) (n int, ok bool) {
	switch {
	case strings.HasPrefix(tail, "https://"),
		strings.HasPrefix(tail, "http://"),
		strings.HasPrefix(tail, "mailto:"),
		strings.HasPrefix(tail, "#"):
	default:
		return 0, false
	}
	idx, term, found := boundedFind(tail)
	if !found || term != '>' {
		return 0, false
	}
	return idx + 1, true
}

// matchDangerousForm recognizes a broadcast, user-group, date, or
// direct-mention form at the start of tail, which must begin with '!'
// or '@' (the caller checks this). It returns that form's plain-text
// replacement and the number of bytes of tail it occupies (including
// the leading '!'/'@' and the closing '>'), or ok=false if tail opens
// no complete, well-formed instance of any of them within
// maxTokenLen bytes.
func matchDangerousForm(tail string) (repl string, n int, ok bool) {
	if tail[0] == '@' {
		return matchUserMention(tail)
	}
	if repl, n, ok := matchSubteam(tail); ok {
		return repl, n, true
	}
	if repl, n, ok := matchDate(tail); ok {
		return repl, n, true
	}
	if repl, n, ok := matchBroadcast(tail); ok {
		return repl, n, true
	}
	return "", 0, false
}

// matchUserMention recognizes "@id" or "@id|label" at the start of
// tail (tail[0] == '@').
func matchUserMention(tail string) (repl string, n int, ok bool) {
	body := tail[1:]
	idEnd := scanWhile(body, isIDByte)
	if idEnd == 0 {
		return "", 0, false
	}
	label, consumed, ok := parseOptionalLabel(body[idEnd:])
	if !ok {
		return "", 0, false
	}
	id := body[:idEnd]
	if label != "" {
		return "@" + strings.TrimSpace(label), 1 + idEnd + consumed, true
	}
	return "@" + strings.TrimSpace(id), 1 + idEnd + consumed, true
}

// broadcastWords are the keywords matchBroadcast recognizes, matched
// case-insensitively. None is a prefix of another, so trying them in
// any order and requiring an exact word boundary (the next byte must
// be '>' or '|') cannot make the wrong one match.
var broadcastWords = []string{"channel", "here", "everyone"}

// matchBroadcast recognizes "!word" or "!word|label" at the start of
// tail (tail[0] == '!'), word case-insensitively one of
// broadcastWords.
func matchBroadcast(tail string) (repl string, n int, ok bool) {
	body := tail[1:]
	for _, word := range broadcastWords {
		if len(body) < len(word) || !strings.EqualFold(body[:len(word)], word) {
			continue
		}
		label, consumed, ok := parseOptionalLabel(body[len(word):])
		if !ok {
			return "", 0, false
		}
		if label != "" {
			return "@" + strings.TrimSpace(label), 1 + len(word) + consumed, true
		}
		return "@" + body[:len(word)], 1 + len(word) + consumed, true
	}
	return "", 0, false
}

// matchSubteam recognizes "!subteam^id" or "!subteam^id|label" at the
// start of tail (tail[0] == '!'), "subteam" matched case-insensitively.
func matchSubteam(tail string) (repl string, n int, ok bool) {
	const kw = "subteam"
	body := tail[1:]
	if len(body) < len(kw)+1 || !strings.EqualFold(body[:len(kw)], kw) || body[len(kw)] != '^' {
		return "", 0, false
	}
	idStart := body[len(kw)+1:]
	idEnd := scanWhile(idStart, isIDByte)
	if idEnd == 0 {
		return "", 0, false
	}
	label, consumed, ok := parseOptionalLabel(idStart[idEnd:])
	if !ok {
		return "", 0, false
	}
	id := idStart[:idEnd]
	total := 1 + len(kw) + 1 + idEnd + consumed
	if label != "" {
		return "@" + strings.TrimSpace(label), total, true
	}
	return "@" + strings.TrimSpace(id), total, true
}

// matchDate recognizes the harmless "!date^timestamp" and
// "!date^timestamp|label" directives at the start of tail
// (tail[0] == '!'), "date" matched case-insensitively. Unlike the
// other dangerous forms its replacement carries no "@" prefix: a date
// is not a mention. The timestamp segment may be empty (Slack's own
// grammar requires a real timestamp, but an empty one is still a
// complete, well-formed "<!date^...>" span as far as this function's
// job — never letting raw "<!" survive — is concerned).
func matchDate(tail string) (repl string, n int, ok bool) {
	const kw = "date"
	body := tail[1:]
	if len(body) < len(kw)+1 || !strings.EqualFold(body[:len(kw)], kw) || body[len(kw)] != '^' {
		return "", 0, false
	}
	tsStart := body[len(kw)+1:]
	tsEnd := scanWhile(tsStart, isIDByte)
	label, consumed, ok := parseOptionalLabel(tsStart[tsEnd:])
	if !ok {
		return "", 0, false
	}
	total := 1 + len(kw) + 1 + tsEnd + consumed
	if label != "" {
		return strings.TrimSpace(label), total, true
	}
	return "date", total, true
}

// parseOptionalLabel parses an optional "|label" immediately followed
// by the form's closing '>', at the start of after. It returns the
// label (empty if there was none), the number of bytes of after
// consumed (1 for a bare '>', or the whole "|label>" span), and
// ok=false if after is empty, starts with neither '>' nor '|', or (for
// a "|label") no closing '>' is found within maxTokenLen bytes before
// a '<' or a newline.
func parseOptionalLabel(after string) (label string, n int, ok bool) {
	if after == "" {
		return "", 0, false
	}
	switch after[0] {
	case '>':
		return "", 1, true
	case '|':
		idx, term, found := boundedFind(after[1:])
		if !found || term != '>' {
			return "", 0, false
		}
		return after[1 : 1+idx], 1 + idx + 1, true
	default:
		return "", 0, false
	}
}

// isIDByte reports whether b can appear in a Slack id or date
// timestamp: anything but '|', '<', '>', or whitespace.
func isIDByte(b byte) bool {
	switch b {
	case '|', '<', '>', ' ', '\t', '\n', '\r', '\v', '\f':
		return false
	default:
		return true
	}
}

// scanWhile returns the length of the longest prefix of s, up to
// maxTokenLen bytes, all satisfying pred. The cap keeps every call
// bounded work, regardless of s's total length.
func scanWhile(s string, pred func(byte) bool) int {
	n := len(s)
	if n > maxTokenLen {
		n = maxTokenLen
	}
	i := 0
	for i < n && pred(s[i]) {
		i++
	}
	return i
}

// boundedFind returns the index and value of the first '<', '>', or
// '\n' in s, scanning at most maxTokenLen bytes. found is false if
// none of those three bytes appears in that window: the candidate
// token is unterminated, or too long to be a well-formed Slack form,
// within the bytes this function is willing to look at.
func boundedFind(s string) (idx int, term byte, found bool) {
	n := len(s)
	if n > maxTokenLen {
		n = maxTokenLen
	}
	for i := 0; i < n; i++ {
		switch s[i] {
		case '<', '>', '\n':
			return i, s[i], true
		}
	}
	return 0, 0, false
}
