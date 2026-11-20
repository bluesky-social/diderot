package adminui

import (
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var orderedItemPattern = regexp.MustCompile(`^[0-9]+\.\s+`)

// renderAskMarkdown implements the compact Markdown subset emitted by Ask on
// HTTP and Slack surfaces. It writes tags itself and escapes every text node;
// only validated http(s) destinations become links. This keeps template.HTML
// local to a renderer whose safety does not depend on model output.
func renderAskMarkdown(text string, slackSurface bool) template.HTML {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out strings.Builder
	var list, fence string
	closeList := func() {
		if list != "" {
			out.WriteString("</" + list + ">\n")
			list = ""
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			closeList()
			if fence == "" {
				fence = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				out.WriteString("<pre><code")
				if fence != "" {
					out.WriteString(` class="language-` + template.HTMLEscapeString(fence) + `"`)
				}
				out.WriteString(">")
			} else {
				out.WriteString("</code></pre>\n")
				fence = ""
			}
			continue
		}
		if fence != "" {
			out.WriteString(template.HTMLEscapeString(line))
			out.WriteByte('\n')
			continue
		}
		if trimmed == "" {
			closeList()
			continue
		}
		kind, item := "", ""
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			kind, item = "ul", strings.TrimSpace(trimmed[2:])
		} else if loc := orderedItemPattern.FindStringIndex(trimmed); loc != nil {
			kind, item = "ol", strings.TrimSpace(trimmed[loc[1]:])
		}
		if kind != "" {
			if list != kind {
				closeList()
				list = kind
				out.WriteString("<" + list + ">\n")
			}
			out.WriteString("<li>" + renderAskInline(item, slackSurface) + "</li>\n")
			continue
		}
		closeList()
		if strings.HasPrefix(trimmed, "> ") {
			out.WriteString("<blockquote>" + renderAskInline(strings.TrimSpace(trimmed[2:]), slackSurface) + "</blockquote>\n")
			continue
		}
		heading := 0
		for heading < len(trimmed) && heading < 6 && trimmed[heading] == '#' {
			heading++
		}
		if heading > 0 && len(trimmed) > heading && trimmed[heading] == ' ' {
			tag := string(rune('0' + heading))
			out.WriteString("<h" + tag + ">" + renderAskInline(strings.TrimSpace(trimmed[heading:]), slackSurface) + "</h" + tag + ">\n")
			continue
		}
		out.WriteString("<p>" + renderAskInline(line, slackSurface) + "</p>\n")
	}
	closeList()
	if fence != "" {
		out.WriteString("</code></pre>\n")
	}
	return template.HTML(out.String()) // #nosec G203 -- all text and attributes are escaped above.
}

func renderAskInline(text string, slackSurface bool) string {
	var out strings.Builder
	for len(text) > 0 {
		if text[0] == '`' {
			if end := strings.IndexByte(text[1:], '`'); end >= 0 {
				out.WriteString("<code>" + template.HTMLEscapeString(text[1:1+end]) + "</code>")
				text = text[end+2:]
				continue
			}
		}
		if text[0] == '[' {
			if labelEnd := strings.Index(text, "]("); labelEnd > 0 {
				if urlEnd := strings.IndexByte(text[labelEnd+2:], ')'); urlEnd >= 0 {
					destination := text[labelEnd+2 : labelEnd+2+urlEnd]
					if link := safeAskLink(destination, text[1:labelEnd]); link != "" {
						out.WriteString(link)
						text = text[labelEnd+3+urlEnd:]
						continue
					}
				}
			}
		}
		if slackSurface && text[0] == '<' {
			if end := strings.IndexByte(text, '>'); end > 1 {
				destination, label, _ := strings.Cut(text[1:end], "|")
				if label == "" {
					label = destination
				}
				if link := safeAskLink(destination, label); link != "" {
					out.WriteString(link)
					text = text[end+1:]
					continue
				}
			}
		}
		markers := []struct {
			open, close string
		}{
			{"**", "strong"},
			{"~~", "del"},
			{"_", "em"},
		}
		// Ask prompts both surfaces to use Slack's single-asterisk bold
		// convention; underscore remains available for emphasis.
		markers = append(markers, struct{ open, close string }{"*", "strong"})
		matched := false
		for _, marker := range markers {
			if strings.HasPrefix(text, marker.open) {
				if end := strings.Index(text[len(marker.open):], marker.open); end > 0 {
					body := text[len(marker.open) : len(marker.open)+end]
					if strings.TrimSpace(body) != "" {
						out.WriteString("<" + marker.close + ">" + template.HTMLEscapeString(body) + "</" + marker.close + ">")
						text = text[len(marker.open)*2+end:]
						matched = true
						break
					}
				}
			}
		}
		if matched {
			continue
		}
		_, size := utf8.DecodeRuneInString(text)
		out.WriteString(template.HTMLEscapeString(text[:size]))
		text = text[size:]
	}
	return out.String()
}

func safeAskLink(rawURL, label string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	return `<a href="` + template.HTMLEscapeString(parsed.String()) + `" target="_blank" rel="noreferrer">` +
		template.HTMLEscapeString(label) + `</a>`
}
