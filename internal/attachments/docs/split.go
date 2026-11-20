package docs

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bluesky-social/diderot/internal/chunk"
)

type Chunk struct {
	Content string
	Page    int
	Section string
}

func Split(markdown string) []Chunk {
	var out []Chunk
	for _, page := range splitPages(markdown) {
		sections := splitSections(page.body)
		if len(sections) == 0 {
			sections = []sectionBlock{{body: page.body}}
		}
		for _, section := range sections {
			for _, content := range chunk.Chunk(strings.TrimSpace(section.body)) {
				if content != "" {
					out = append(out, Chunk{Content: content, Page: page.page, Section: section.title})
				}
			}
		}
	}
	return out
}

type pageBlock struct {
	page int
	body string
}

func splitPages(markdown string) []pageBlock {
	if strings.TrimSpace(markdown) == "" {
		return nil
	}
	var blocks []pageBlock
	page := 0
	seenMarker := false
	var body strings.Builder
	flush := func() {
		if text := strings.TrimSpace(body.String()); text != "" {
			blocks = append(blocks, pageBlock{page: page, body: text})
		}
		body.Reset()
	}
	for _, line := range strings.Split(markdown, "\n") {
		if n, ok := parsePageMarker(line); ok {
			if seenMarker {
				flush()
			}
			page = n
			seenMarker = true
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush()
	return blocks
}

func parsePageMarker(line string) (int, bool) {
	text := strings.TrimSpace(line)
	if !strings.HasPrefix(text, pageMarkerPrefix) {
		return 0, false
	}
	text = strings.TrimRight(strings.TrimPrefix(text, pageMarkerPrefix), "- \t")
	n, err := strconv.Atoi(strings.TrimSpace(text))
	return n, err == nil && n > 0
}

type sectionBlock struct {
	title string
	body  string
}

func splitSections(text string) []sectionBlock {
	var blocks []sectionBlock
	var title string
	var body strings.Builder
	flush := func() {
		if content := strings.TrimSpace(body.String()); content != "" {
			blocks = append(blocks, sectionBlock{title: title, body: content})
		}
		body.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "## ") {
			flush()
			title = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "## "))
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush()
	return blocks
}

func ContextPrefix(filename, sharer, channel, date string, c Chunk) string {
	var b strings.Builder
	b.WriteString("[document] ")
	b.WriteString(filename)
	if c.Section != "" {
		fmt.Fprintf(&b, " (section %s)", c.Section)
	} else if c.Page > 0 {
		fmt.Fprintf(&b, " (page %d)", c.Page)
	}
	if sharer != "" {
		b.WriteString(" shared by ")
		b.WriteString(sharer)
	}
	if channel != "" {
		b.WriteString(" in #")
		b.WriteString(channel)
	}
	if date != "" {
		b.WriteString(" on ")
		b.WriteString(date)
	}
	return b.String()
}
