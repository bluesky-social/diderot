package adminui

import (
	"fmt"
	"strings"
)

// Diff row kinds.
const (
	diffSame = "same"
	diffAdd  = "add"
	diffDel  = "del"
	diffSkip = "skip"
)

const (
	// diffContext is how many unchanged lines show around a change.
	diffContext = 3
	// diffMaxCells bounds the line-matching table. Past it the changed
	// middle of two texts shows as removed, then added: still correct, only
	// longer than it needs to be.
	diffMaxCells = 1 << 20
)

// diffLine is one row of a line diff: a line kept, added or removed, or a
// run of Skipped unchanged lines left out.
type diffLine struct {
	Op      string
	Text    string
	Skipped int
}

// promptDiff is how one prompt text changed into another.
type promptDiff struct {
	Lines   []diffLine
	Added   int
	Removed int
}

// Changed reports whether the texts differ.
func (d promptDiff) Changed() bool { return d.Added > 0 || d.Removed > 0 }

// Summary counts the changed lines: "3 lines added, 1 removed".
func (d promptDiff) Summary() string {
	switch {
	case d.Added > 0 && d.Removed > 0:
		return fmt.Sprintf("%s added, %d removed", plural(d.Added, "line"), d.Removed)
	case d.Added > 0:
		return plural(d.Added, "line") + " added"
	case d.Removed > 0:
		return plural(d.Removed, "line") + " removed"
	}
	return "No lines changed"
}

// diffText compares two texts line by line and keeps diffContext unchanged
// lines around each change.
func diffText(before, after string) promptDiff {
	a, b := splitLines(before), splitLines(after)
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	var lines []diffLine
	for _, line := range a[:prefix] {
		lines = append(lines, diffLine{Op: diffSame, Text: line})
	}
	lines = append(lines, diffMiddle(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix])...)
	for _, line := range a[len(a)-suffix:] {
		lines = append(lines, diffLine{Op: diffSame, Text: line})
	}
	var diff promptDiff
	for _, line := range lines {
		switch line.Op {
		case diffAdd:
			diff.Added++
		case diffDel:
			diff.Removed++
		}
	}
	diff.Lines = collapseUnchanged(lines)
	return diff
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// diffMiddle diffs two runs of lines by their longest common subsequence.
func diffMiddle(a, b []string) []diffLine {
	var lines []diffLine
	if len(a)*len(b) > diffMaxCells {
		for _, line := range a {
			lines = append(lines, diffLine{Op: diffDel, Text: line})
		}
		for _, line := range b {
			lines = append(lines, diffLine{Op: diffAdd, Text: line})
		}
		return lines
	}
	// common[i][j] is the longest common subsequence of a[i:] and b[j:].
	width := len(b) + 1
	common := make([]int32, (len(a)+1)*width)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				common[i*width+j] = common[(i+1)*width+j+1] + 1
			case common[(i+1)*width+j] >= common[i*width+j+1]:
				common[i*width+j] = common[(i+1)*width+j]
			default:
				common[i*width+j] = common[i*width+j+1]
			}
		}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			lines = append(lines, diffLine{Op: diffSame, Text: a[i]})
			i, j = i+1, j+1
		case common[(i+1)*width+j] >= common[i*width+j+1]:
			lines = append(lines, diffLine{Op: diffDel, Text: a[i]})
			i++
		default:
			lines = append(lines, diffLine{Op: diffAdd, Text: b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		lines = append(lines, diffLine{Op: diffDel, Text: a[i]})
	}
	for ; j < len(b); j++ {
		lines = append(lines, diffLine{Op: diffAdd, Text: b[j]})
	}
	return lines
}

// collapseUnchanged replaces each run of two or more unchanged lines farther
// than diffContext from a change with one skip row.
func collapseUnchanged(lines []diffLine) []diffLine {
	near := make([]bool, len(lines))
	for index, line := range lines {
		if line.Op == diffSame {
			continue
		}
		for k := max(0, index-diffContext); k <= min(len(lines)-1, index+diffContext); k++ {
			near[k] = true
		}
	}
	var out []diffLine
	for index := 0; index < len(lines); {
		end := index
		for end < len(lines) && !near[end] {
			end++
		}
		switch end - index {
		case 0:
			out = append(out, lines[index])
			index++
		case 1:
			out = append(out, lines[index])
			index = end
		default:
			out = append(out, diffLine{Op: diffSkip, Skipped: end - index})
			index = end
		}
	}
	return out
}
