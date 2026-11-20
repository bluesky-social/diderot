package urlx

import "regexp"

// ExtractURLs pulls every http(s) URL out of a text, in order of
// first appearance, deduplicated. Handles the three shapes ask
// answers actually contain:
//
//   - markdown links: [text](https://...)
//   - slack mrkdwn links: <https://...|text> and bare <https://...>
//   - bare URLs in prose
//
// The single scan regex finds URL starts; trimming handles the
// wrapper-specific terminators, so a URL containing parens or pipes
// in its query string survives the common cases (slack permalinks,
// session viewer URLs, notion pages have neither).
func ExtractURLs(text string) []string {
	matches := urlRE.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		u := trimURL(m)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// urlRE matches from a scheme to the first character that can't be
// part of any of the three link shapes: whitespace always ends a
// URL; the wrapper terminators are trimmed afterwards.
var urlRE = regexp.MustCompile(`https?://[^\s<>]+`)

// trimURL strips trailing characters that belong to the surrounding
// link syntax or prose, not the URL: the markdown ")" close, the
// slack "|text" label (the regex stops at whitespace so a label
// with spaces leaves "|" + first word attached), and trailing
// punctuation from prose ("see https://example.com." / "(https://example.com)").
func trimURL(u string) string {
	// Slack label: everything from the first "|" is the link text.
	for i := 0; i < len(u); i++ {
		if u[i] == '|' {
			u = u[:i]
			break
		}
	}
	// Trailing prose/wrapper punctuation. ")" is ambiguous (legal in
	// URLs) but our sources never emit unbalanced-paren URLs, and the
	// markdown form ends in ")" always.
	for len(u) > 0 {
		switch u[len(u)-1] {
		case ')', '.', ',', ';', ':', '!', '?', '"', '\'':
			u = u[:len(u)-1]
		default:
			return u
		}
	}
	return u
}
