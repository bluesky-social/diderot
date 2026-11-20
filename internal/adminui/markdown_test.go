package adminui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderAskMarkdownSupportsSlackDialect(t *testing.T) {
	got := string(renderAskMarkdown("*bold* and <https://example.com/a|a link>\n* list item", true))
	assert.Contains(t, got, "<strong>bold</strong>")
	assert.Contains(t, got, `<a href="https://example.com/a"`)
	assert.Contains(t, got, `>a link</a>`)
	assert.Contains(t, got, "<li>list item</li>")
}

func TestRenderAskMarkdownRejectsUnsafeHTMLAndLinks(t *testing.T) {
	got := string(renderAskMarkdown(`<script>alert(1)</script> [click](javascript:alert(2)) [creds](https://user:pass@example.com)`, false))
	assert.NotContains(t, strings.ToLower(got), "<script")
	assert.NotContains(t, strings.ToLower(got), `href="javascript:`)
	assert.NotContains(t, strings.ToLower(got), `href="https://user:pass@`)
	assert.Contains(t, got, "click")
}

func TestSlackMarkdownDoesNotRewriteBullets(t *testing.T) {
	got := string(renderAskMarkdown("* first\n* second", true))
	assert.Contains(t, got, "<ul>")
	assert.Contains(t, got, "<li>first</li>")
	assert.Contains(t, got, "<li>second</li>")
}
