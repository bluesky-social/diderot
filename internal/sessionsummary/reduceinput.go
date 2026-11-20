package sessionsummary

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxReduceInputBytes = 768 << 10

func renderReduceInput(chunks []ChunkSummary) (string, error) {
	const opening = "<chunk_summaries>\n"
	const closing = "</chunk_summaries>"
	originalBytes := len(opening) + len(closing)
	for index, chunk := range chunks {
		originalBytes += len(fmt.Sprintf("  <chunk index=%q>\n    <evidence>\n    </evidence>\n    <summary></summary>\n  </chunk>\n", chunkLabel(chunk, index))) + len(chunk.Summary)
		for _, evidence := range chunk.Evidence {
			originalBytes += len("      - \n") + len(evidence)
		}
		if originalBytes > maxReduceInputBytes {
			break
		}
	}
	if originalBytes <= maxReduceInputBytes {
		var original strings.Builder
		original.Grow(originalBytes)
		original.WriteString(opening)
		for index, chunk := range chunks {
			fmt.Fprintf(&original, "  <chunk index=%q>\n", chunkLabel(chunk, index))
			original.WriteString("    <evidence>\n")
			for _, evidence := range chunk.Evidence {
				original.WriteString("      - " + evidence + "\n")
			}
			original.WriteString("    </evidence>\n")
			original.WriteString("    <summary>" + chunk.Summary + "</summary>\n")
			original.WriteString("  </chunk>\n")
		}
		original.WriteString(closing)
		return original.String(), nil
	}

	overhead := len(opening) + len(closing)
	for index, chunk := range chunks {
		overhead += len(fmt.Sprintf("<chunk index=%q><evidence></evidence><summary></summary></chunk>\n", chunkLabel(chunk, index)))
	}
	if overhead > maxReduceInputBytes {
		return "", fmt.Errorf("reduce chunk labels exceed %d-byte input budget", maxReduceInputBytes)
	}
	perChunk := (maxReduceInputBytes - overhead) / len(chunks)
	var bounded strings.Builder
	bounded.Grow(maxReduceInputBytes)
	bounded.WriteString(opening)
	for index, chunk := range chunks {
		evidence := strings.Join(chunk.Evidence, "; ")
		summaryBytes := min(len(chunk.Summary), (perChunk+1)/2)
		evidenceBytes := min(len(evidence), perChunk-summaryBytes)
		summaryBytes = min(len(chunk.Summary), perChunk-evidenceBytes)
		fmt.Fprintf(&bounded, "<chunk index=%q><evidence>", chunkLabel(chunk, index))
		bounded.WriteString(reducePrefix(evidence, evidenceBytes))
		bounded.WriteString("</evidence><summary>")
		bounded.WriteString(reducePrefix(chunk.Summary, summaryBytes))
		bounded.WriteString("</summary></chunk>\n")
	}
	bounded.WriteString(closing)
	return bounded.String(), nil
}

func reducePrefix(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

func chunkLabel(chunk ChunkSummary, index int) string {
	if chunk.Label != "" {
		return chunk.Label
	}
	return fmt.Sprint(index + 1)
}
