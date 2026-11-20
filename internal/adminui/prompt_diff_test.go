package adminui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/stretchr/testify/assert"
)

// diffRows renders a diff compactly: " line", "+line", "-line", "~N".
func diffRows(diff promptDiff) []string {
	var rows []string
	for _, line := range diff.Lines {
		switch line.Op {
		case diffAdd:
			rows = append(rows, "+"+line.Text)
		case diffDel:
			rows = append(rows, "-"+line.Text)
		case diffSkip:
			rows = append(rows, fmt.Sprintf("~%d", line.Skipped))
		default:
			rows = append(rows, " "+line.Text)
		}
	}
	return rows
}

func numbered(from, to int) string {
	var lines []string
	for i := from; i <= to; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	return strings.Join(lines, "\n")
}

func TestDiffTextKeepsContextAroundChanges(t *testing.T) {
	before := numbered(1, 20)
	after := strings.Replace(before, "line 10", "line ten", 1)
	diff := diffText(before, after)
	assert.Equal(t, []string{
		"~6", " line 7", " line 8", " line 9", "-line 10", "+line ten", " line 11", " line 12", " line 13", "~7",
	}, diffRows(diff))
	assert.Equal(t, 1, diff.Added)
	assert.Equal(t, 1, diff.Removed)
	assert.Equal(t, "1 line added, 1 removed", diff.Summary())
}

func TestDiffTextFindsMovedAndInsertedLines(t *testing.T) {
	diff := diffText("a\nb\nc\nd", "a\nc\nx\nd\ne")
	assert.Equal(t, []string{" a", "-b", " c", "+x", " d", "+e"}, diffRows(diff))
	assert.Equal(t, "2 lines added, 1 removed", diff.Summary())
}

func TestDiffTextEdgeCases(t *testing.T) {
	same := diffText("one\ntwo", "one\ntwo")
	assert.False(t, same.Changed())
	assert.Equal(t, "No lines changed", same.Summary())

	added := diffText("", "new")
	assert.Equal(t, []string{"+new"}, diffRows(added))
	assert.Equal(t, "1 line added", added.Summary())
	assert.Equal(t, "2 lines removed", diffText("x\ny", "").Summary())

	// A single unchanged line between changes shows rather than a skip row.
	assert.Equal(t, []string{"-a", "+A", " b", " c", " d", " e", "-f", "+F"},
		diffRows(diffText("a\nb\nc\nd\ne\nf", "A\nb\nc\nd\ne\nF")))
}

func TestDiffTextBoundsLargeRewrites(t *testing.T) {
	before := numbered(1, 1100)
	after := strings.ReplaceAll(numbered(1, 1100), "line", "row")
	diff := diffText(before, after)
	assert.Equal(t, 1100, diff.Added)
	assert.Equal(t, 1100, diff.Removed)
	assert.Equal(t, "-line 1", diffRows(diff)[0], "past the table bound every old line goes first")
}

func TestChangedPromptsCountsAddedAndRemovedPrompts(t *testing.T) {
	prompt := func(key, body string) promptcatalog.Prompt {
		return promptcatalog.Prompt{Key: key, Body: body, SHA256: promptcatalog.BodySHA256(body)}
	}
	from := promptcatalog.Bundle{Prompts: []promptcatalog.Prompt{prompt("a", "one"), prompt("b", "two"), prompt("gone", "x")}}
	to := promptcatalog.Bundle{Prompts: []promptcatalog.Prompt{prompt("a", "one"), prompt("b", "three"), prompt("new", "y")}}
	assert.Equal(t, []string{"b", "gone", "new"}, changedPrompts(from, to))
	assert.Empty(t, changedPrompts(from, from))
}
