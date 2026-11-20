package sessioningest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/sessiondecode"
	"github.com/bluesky-social/diderot/internal/sessionsummary"
)

func legacySessionChunks(test testing.TB, body []byte, targetBytes int) []sessionsummary.Chunk {
	test.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	first := true
	var canonical []byte
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if first {
			first = false
			continue
		}
		var record sessiondecode.Record
		if err := json.Unmarshal(line, &record); err != nil {
			test.Fatal(err)
		}
		if _, err := sessiondecode.NormalizeBskyRecord(&record); err != nil {
			test.Fatal(err)
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			test.Fatal(err)
		}
		if len(canonical) > 0 {
			canonical = append(canonical, '\n')
		}
		canonical = append(canonical, encoded...)
	}
	if err := scanner.Err(); err != nil {
		test.Fatal(err)
	}
	bounded, _ := sessionsummary.BoundLineSizes(canonical)
	var chunks []sessionsummary.Chunk
	var current []byte
	start := 0
	lines := bytes.Split(bounded, []byte("\n"))
	flush := func(end int) {
		if len(current) > 0 {
			chunks = append(chunks, sessionsummary.Chunk{RecordStart: start, RecordEnd: end, Bytes: bytes.Clone(current)})
		}
		current = nil
		start = end
	}
	for index, line := range lines {
		if len(current) > 0 && len(current)+len(line)+1 > targetBytes {
			flush(index)
		}
		if len(current) > 0 {
			current = append(current, '\n')
		}
		current = append(current, line...)
	}
	flush(len(lines))
	return chunks
}

func assertLegacySessionChunks(test *testing.T, body []byte, doc *sessionDocument) {
	test.Helper()
	bundle := sessionsummary.PromptBundle{MapSystem: "map", ReduceSystem: "reduce"}
	mapOptions := sessionsummary.CallOptions{ModelID: "map-model", MaxTokens: 1024}
	reduceOptions := sessionsummary.CallOptions{ModelID: "reduce-model", MaxTokens: 4096}
	for _, target := range []int{4096, sessionsummary.DefaultChunkBytes} {
		expected := legacySessionChunks(test, body, target)
		actual := (&sessionsummarySummarizer{}).chunkRecords(doc, target)
		if len(actual) != len(expected) {
			test.Fatalf("target=%d: chunks=%d, want %d", target, len(actual), len(expected))
		}
		for index, chunk := range actual {
			old := expected[index]
			if chunk.RecordStart != old.RecordStart || chunk.RecordEnd != old.RecordEnd || !bytes.Equal(chunk.Bytes, old.Bytes) {
				test.Fatalf("target=%d: chunk %d changed bytes or record range", target, index)
			}
		}
		if summaryInputDigest(bundle, "session_summary", mapOptions, reduceOptions, actual) != summaryInputDigest(bundle, "session_summary", mapOptions, reduceOptions, expected) {
			test.Fatal("published summary identity changed")
		}
	}
}

func memoryTestSessionBody(count int, text string) []byte {
	var records bytes.Buffer
	for index := range count {
		fmt.Fprintf(&records, `{"index":%d,"id":"r%d","role":"user","content":%q,"timestamp":"2026-10-06T12:00:00Z"}`+"\n", index, index, text)
	}
	envelope := fmt.Sprintf(`{"type":"session","schema":"bsky.session.v1","session_id":"ses-memory","revision":"sha256:test","record_count":%d,"user_turn_count":%d,"content_bytes":%d,"complete":true,"owner":"test@example.com","owner_kind":"human","source":"codex","session_role":"primary","started_at":"2026-10-06T12:00:00Z","updated_at":"2026-10-06T12:00:00Z"}`+"\n", count, count, records.Len())
	return append([]byte(envelope), records.Bytes()...)
}

func TestSessionDocumentChunkCompatibility(test *testing.T) {
	for _, text := range []string{"small content", strings.Repeat("日本語 🦋 \"quoted\"\n", 300), strings.Repeat("large content ", 16000)} {
		body := memoryTestSessionBody(8, text)
		doc, err := parseSessionDocument(body, Identity{}, "sha256:test", 256<<20)
		if err != nil {
			test.Fatal(err)
		}
		assertLegacySessionChunks(test, body, doc)
	}
}

func TestSessionChunksShareTranscript(test *testing.T) {
	body := memoryTestSessionBody(64, strings.Repeat("x", 2048))
	doc, err := parseSessionDocument(body, Identity{}, "sha256:test", 256<<20)
	if err != nil {
		test.Fatal(err)
	}
	original := bytes.Clone(doc.Transcript)
	chunks := (&sessionsummarySummarizer{}).chunkRecords(doc, 4096)
	offset := 0
	for _, chunk := range chunks {
		if &chunk.Bytes[0] != &doc.Transcript[offset] {
			test.Fatal("chunk allocated its own transcript copy")
		}
		if cap(chunk.Bytes) != len(chunk.Bytes) {
			test.Fatal("appending to a chunk could overwrite the next chunk")
		}
		offset += len(chunk.Bytes) + 1
	}
	if !bytes.Equal(original, doc.Transcript) {
		test.Fatal("chunking changed the reusable document")
	}
	assertLegacySessionChunks(test, body, doc)
}

func BenchmarkSessionDocumentMemory(bench *testing.B) {
	body := memoryTestSessionBody(8192, strings.Repeat("x", 2048))
	bench.SetBytes(int64(len(body)))
	bench.ReportAllocs()
	bench.ResetTimer()
	for range bench.N {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		doc, err := parseSessionDocument(body, Identity{}, "sha256:test", 256<<20)
		if err != nil {
			bench.Fatal(err)
		}
		chunks := (&sessionsummarySummarizer{}).chunkRecords(doc, 0)
		runtime.GC()
		runtime.ReadMemStats(&after)
		bench.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc), "retained-B/op")
		runtime.KeepAlive(doc)
		runtime.KeepAlive(chunks)
	}
}
