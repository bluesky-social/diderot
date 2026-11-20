package sessionsummary

import (
	"bytes"
)

// Chunk bounds. Haiku 4.5 has a 200K token context window. Our
// transcripts have wildly variable byte/token density:
//
//   - Prose-heavy assistant text:    ~0.8 bytes/token
//   - Dense tool_use JSON:           ~0.5 bytes/token
//   - Base64 image data: tokens get fragmented into 3-4 char
//     pieces, so ~270K base64 chars from a 200KB PNG produce
//     ~600K tokens — 3x worse than even the JSON case. We strip
//     these pre-chunking (see images.go), but the worst-case
//     density we still need to budget for is dense JSON.
//
// Measured worst case: 60KB chunks of dense tool JSON expand about 3.5x,
// to ~210-220K tokens, which overflows a 200K-token context.
//
// Sizing for that worst case: 200K tokens total minus
// ~25K for system prompt + scaffolding minus 10% variance =
// ~150K safe token budget, which is ~30KB bytes at the failure
// expansion factor. Doubling map calls for short sessions is
// cheap (the map model is a fast one); failing on long sessions is not.
const (
	// DefaultChunkBytes: 30KB. Targets ~110K tokens at the worst
	// observed expansion; 60KB overflows the context.
	DefaultChunkBytes = 30 * 1024
)

// Chunk is one piece of a canonical record NDJSON transcript for the map pass.
// RecordStart and RecordEnd identify the inclusive/exclusive record range used
// by session_chunk_summaries for checkpoint reuse.
type Chunk struct {
	RecordStart int    // inclusive
	RecordEnd   int    // exclusive
	Bytes       []byte // payload sent to the model
}

// ChunkTranscript splits canonical record NDJSON at record boundaries without
// reserializing records, preserving the bytes used for checkpoint digests.
func ChunkTranscript(transcript []byte, targetBytes int) []Chunk {
	if targetBytes <= 0 {
		targetBytes = DefaultChunkBytes
	}
	if len(transcript) == 0 {
		return nil
	}
	return chunkJSONL(transcript, targetBytes)
}

func chunkJSONL(transcript []byte, targetBytes int) []Chunk {
	var chunks []Chunk
	startRecord, recordCount := 0, 0
	startOffset, endOffset, currentBytes := 0, 0, 0
	flush := func() {
		if currentBytes == 0 {
			return
		}
		payload := transcript[startOffset:endOffset:endOffset]
		if len(payload) != currentBytes {
			payload = bytes.Join(splitJSONLines(payload), []byte("\n"))
		}
		chunks = append(chunks, Chunk{
			RecordStart: startRecord,
			RecordEnd:   recordCount,
			Bytes:       payload,
		})
		currentBytes = 0
		startRecord = recordCount
	}
	for offset := 0; offset < len(transcript); {
		end := len(transcript)
		if newline := bytes.IndexByte(transcript[offset:], '\n'); newline >= 0 {
			end = offset + newline
		}
		line := transcript[offset:end]
		if len(bytes.TrimSpace(line)) > 0 {
			if currentBytes > 0 && currentBytes+len(line)+1 > targetBytes {
				flush()
			}
			if currentBytes == 0 {
				startOffset = offset
			} else {
				currentBytes++
			}
			currentBytes += len(line)
			endOffset = end
			recordCount++
		}
		offset = end + 1
	}
	flush()
	return chunks
}

// RecordPrefix returns the transcript through its first records records, as
// ChunkTranscript counts them. Chunking is greedy from the start, so chunking
// the prefix reproduces the chunks an earlier pass made when the transcript
// held only those records. ok is false when the transcript has fewer.
func RecordPrefix(transcript []byte, records int) (prefix []byte, ok bool) {
	if records <= 0 {
		return nil, records == 0
	}
	seen := 0
	for offset := 0; offset < len(transcript); {
		end := len(transcript)
		if newline := bytes.IndexByte(transcript[offset:], '\n'); newline >= 0 {
			end = offset + newline
		}
		if len(bytes.TrimSpace(transcript[offset:end])) > 0 {
			seen++
			if seen == records {
				return transcript[:end:end], true
			}
		}
		offset = end + 1
	}
	return nil, false
}

// splitJSONLines splits on \n; ignores blank lines.
func splitJSONLines(b []byte) [][]byte {
	if len(b) == 0 {
		return nil
	}
	var out [][]byte
	for len(b) > 0 {
		nl := bytes.IndexByte(b, '\n')
		if nl < 0 {
			if len(bytes.TrimSpace(b)) > 0 {
				out = append(out, b)
			}
			break
		}
		line := b[:nl]
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, line)
		}
		b = b[nl+1:]
	}
	return out
}
