package sessionsummary

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestChunkJSONLMatchesLegacyWithBlankLines(test *testing.T) {
	for _, transcript := range []string{
		"", " \t\r\n\n", "first", "first\n", "first\nsecond\n",
		"\nfirst\n\nsecond\n  \nthird\n\n",
		"  first  \r\n\tsecond\r\nthird", "first\n\u00a0\nsecond\n",
	} {
		for target := 1; target <= 40; target++ {
			var expected []Chunk
			var current []byte
			start := 0
			flush := func(end int) {
				if len(current) > 0 {
					expected = append(expected, Chunk{RecordStart: start, RecordEnd: end, Bytes: bytes.Clone(current)})
				}
				current = nil
				start = end
			}
			lines := splitJSONLines([]byte(transcript))
			for index, line := range lines {
				if len(current) > 0 && len(current)+len(line)+1 > target {
					flush(index)
				}
				if len(current) > 0 {
					current = append(current, '\n')
				}
				current = append(current, line...)
			}
			flush(len(lines))
			actual := chunkJSONL([]byte(transcript), target)
			if len(actual) != len(expected) {
				test.Fatalf("transcript=%q target=%d: chunk count changed", transcript, target)
			}
			for index, chunk := range actual {
				old := expected[index]
				if chunk.RecordStart != old.RecordStart || chunk.RecordEnd != old.RecordEnd || !bytes.Equal(chunk.Bytes, old.Bytes) {
					test.Fatalf("transcript=%q target=%d: chunk %d changed", transcript, target, index)
				}
			}
		}
	}
}

func TestChunkTranscript_SmallTranscript_OneChunk(t *testing.T) {
	transcript := []byte(`{"role":"user","content":"hi"}
{"role":"assistant","content":"hello"}`)
	chunks := ChunkTranscript(transcript, DefaultChunkBytes)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if !strings.Contains(string(chunks[0].Bytes), `"hi"`) ||
		!strings.Contains(string(chunks[0].Bytes), `"hello"`) {
		t.Fatalf("lost content: %s", chunks[0].Bytes)
	}
}

func TestChunkTranscript_LargeTranscript_Splits(t *testing.T) {
	var lines []string
	for i := 0; i < 1000; i++ {
		lines = append(lines,
			fmt.Sprintf(`{"role":"user","content":"message %d with some padding to have real size"}`, i))
	}
	transcript := []byte(strings.Join(lines, "\n"))

	chunks := ChunkTranscript(transcript, 4096)
	if len(chunks) < 2 {
		t.Fatalf("want multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if len(c.Bytes) > 4096+512 {
			t.Fatalf("chunk %d too big: %d bytes", i, len(c.Bytes))
		}
	}
	if chunks[0].RecordStart != 0 {
		t.Fatalf("first chunk RecordStart=%d, want 0", chunks[0].RecordStart)
	}
	for i := 1; i < len(chunks); i++ {
		if chunks[i].RecordStart != chunks[i-1].RecordEnd {
			t.Fatalf("chunk %d gap: prev RecordEnd=%d this RecordStart=%d",
				i, chunks[i-1].RecordEnd, chunks[i].RecordStart)
		}
	}
	if got, want := chunks[len(chunks)-1].RecordEnd, len(lines); got != want {
		t.Fatalf("last chunk RecordEnd=%d, want %d", got, want)
	}

	joined := strings.Join(chunksToStrings(chunks), "\n")
	for _, line := range lines {
		if strings.Count(joined, line) != 1 {
			t.Fatalf("line appears wrong number of times: %q", line)
		}
	}
}

func TestChunkTranscript_OversizedSingleLine_KeptIntact(t *testing.T) {
	huge := `{"role":"user","content":"` + strings.Repeat("x", 10000) + `"}`
	transcript := []byte(huge)
	chunks := ChunkTranscript(transcript, 1024)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk for single oversized line, got %d", len(chunks))
	}
	if len(chunks[0].Bytes) != len(huge) {
		t.Fatalf("oversized line corrupted: want %d bytes, got %d",
			len(huge), len(chunks[0].Bytes))
	}
}

func TestChunkTranscript_EmptyInput(t *testing.T) {
	if chunks := ChunkTranscript(nil, DefaultChunkBytes); chunks != nil {
		t.Fatalf("want nil for empty input, got %v", chunks)
	}
	if chunks := ChunkTranscript([]byte(""), DefaultChunkBytes); chunks != nil {
		t.Fatalf("want nil for zero-length input, got %v", chunks)
	}
}

func TestChunkTranscript_BlankLinesSkipped(t *testing.T) {
	transcript := []byte(`{"role":"user","content":"hi"}


{"role":"assistant","content":"hello"}

`)
	chunks := ChunkTranscript(transcript, DefaultChunkBytes)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
}

func chunksToStrings(chunks []Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = string(c.Bytes)
	}
	return out
}

// A JSONL transcript whose FIRST LINE nests a "messages" key inside
// a tool payload must not be misrouted to the messages-doc parser —
// that parser decodes one JSON value and would drop every later
// line, summarizing the session as (nearly) empty.
func TestChunkTranscript_NestedMessagesKeyPreservesAllRecords(t *testing.T) {
	jsonl := `{"role":"user","content":"call the api","tool_input":{"messages":[{"role":"system"}]}}
{"role":"assistant","content":"done"}
{"role":"user","content":"thanks"}`
	chunks := ChunkTranscript([]byte(jsonl), DefaultChunkBytes)
	if len(chunks) == 0 {
		t.Fatal("JSONL transcript chunked to nothing")
	}
	total := 0
	for _, c := range chunks {
		total += c.RecordEnd - c.RecordStart
	}
	if total != 3 {
		t.Fatalf("expected all 3 JSONL lines covered, got %d", total)
	}
}

func TestRecordPrefix(t *testing.T) {
	transcript := []byte("{\"a\":1}\n\n{\"b\":2}\n  \n{\"c\":3}")
	for _, tc := range []struct {
		records int
		want    string
		ok      bool
	}{
		{0, "", true},
		{1, "{\"a\":1}", true},
		{2, "{\"a\":1}\n\n{\"b\":2}", true},
		{3, string(transcript), true},
		{4, "", false},
	} {
		prefix, ok := RecordPrefix(transcript, tc.records)
		if ok != tc.ok || string(prefix) != tc.want {
			t.Errorf("RecordPrefix(%d) = %q, %v; want %q, %v", tc.records, prefix, ok, tc.want, tc.ok)
		}
	}
}
