package sessionsummary

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReduceInputPreservesSmallInputs(t *testing.T) {
	chunks := []ChunkSummary{{Evidence: StringList{"first", "second"}, Summary: "unchanged"}}
	actual, err := renderReduceInput(chunks)
	if err != nil {
		t.Fatal(err)
	}
	want := "<chunk_summaries>\n  <chunk index=\"1\">\n    <evidence>\n      - first\n      - second\n    </evidence>\n    <summary>unchanged</summary>\n  </chunk>\n</chunk_summaries>"
	if actual != want {
		t.Fatalf("small reduce input changed: %q", actual)
	}
}

func TestReduceInputBoundsEveryChunkWithoutChangingStoredSummaries(t *testing.T) {
	for _, count := range []int{1, 3488, 4456, 10000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			chunks := make([]ChunkSummary, count)
			for index := range chunks {
				label := fmt.Sprintf("chunk-%d", index+1)
				chunks[index] = ChunkSummary{
					Evidence: StringList{label + strings.Repeat("证据🦋", 1000)},
					Summary:  label + strings.Repeat("摘要🦋", 1000),
				}
			}
			before := make([]ChunkSummary, count)
			for index, chunk := range chunks {
				before[index] = ChunkSummary{Summary: chunk.Summary, Evidence: append(StringList(nil), chunk.Evidence...)}
			}
			actual, err := renderReduceInput(chunks)
			if err != nil {
				t.Fatal(err)
			}
			if len(actual) > maxReduceInputBytes || !utf8.ValidString(actual) {
				t.Fatalf("invalid bounded input: %d bytes, valid UTF-8 %v", len(actual), utf8.ValidString(actual))
			}
			for index := range chunks {
				label := fmt.Sprintf("<chunk index=\"%d\">", index+1)
				if !strings.Contains(actual, label) {
					t.Fatalf("chunk %d disappeared from the merge", index+1)
				}
				if count <= 4456 && !strings.Contains(actual, fmt.Sprintf("chunk-%d", index+1)) {
					t.Fatalf("chunk %d lost its contribution to the merge", index+1)
				}
			}
			if !reflect.DeepEqual(chunks, before) {
				t.Fatal("bounding the merge changed full chunk summaries")
			}
		})
	}
}

func TestReduceInputDoesNotCutOffTheTail(t *testing.T) {
	chunks := []ChunkSummary{
		{Summary: "start " + strings.Repeat("x", maxReduceInputBytes)},
		{Evidence: StringList{"tail-evidence"}, Summary: "tail-summary"},
	}
	actual, err := renderReduceInput(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(actual, "tail-evidence") || !strings.Contains(actual, "tail-summary") {
		t.Fatal("an oversized early chunk hid the end of the session")
	}
}

func TestReduceInputUsesEntryLabels(t *testing.T) {
	chunks := []ChunkSummary{
		{Label: "1-3", Evidence: StringList{"hint"}, Summary: "earlier session"},
		{Label: "3", Summary: "grown chunk"},
		{Summary: "unlabelled"},
	}
	actual, err := renderReduceInput(chunks)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(actual, `<chunk index="1-3">`) {
		t.Fatalf("reduce input %q lacks the earlier summary's label", actual)
	}
	if strings.Count(actual, `<chunk index="3">`) != 2 {
		t.Fatalf("a labelled entry keeps its label and an unlabelled one takes its position: %q", actual)
	}
}
