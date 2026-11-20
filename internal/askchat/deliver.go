package askchat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/bluesky-social/diderot/internal/ask"
)

// Deliver posts an answer into conv as one or more chunks:
//
//  1. An empty reply becomes a placeholder, so something is posted.
//  2. The reply is sanitized, and the incomplete-answer notice for
//     incomplete is appended before splitting, so every chunk stays within
//     the surface's limit.
//  3. The reply is split at the surface's ChunkLimit, preferring line
//     breaks.
//  4. Each chunk is posted in order; the first error stops it, and the
//     chunks that landed stay. Each posted chunk is registered for
//     feedback (best effort).
//
// With a marker, the delivery is idempotent: each chunk carries the marker
// (numbered after the first), and a chunk an earlier attempt already posted
// is found by it (MarkedDelivery) and not posted again. It returns the first
// chunk's message ID.
func (e *Engine) Deliver(ctx context.Context, conv Conversation, sessionID string, assistantTurn int, reply string, incomplete ask.IncompleteReason, marker string) (string, error) {
	if reply == "" {
		reply = "_(empty response)_"
	}
	reply = e.Surface.Sanitize(reply)
	if notice := ask.IncompleteNotice(incomplete); notice != "" {
		reply += "\n_(" + notice + ")_"
	}
	chunkLimit := e.Surface.ChunkLimit()
	if marker != "" {
		chunkLimit -= len(marker) + 32
		if chunkLimit < 256 {
			return "", errors.New("answer delivery marker is too long")
		}
	}
	chunks := Split(reply, chunkLimit)
	var delivered map[string]string
	if marker != "" {
		finder, ok := e.Surface.(MarkedDelivery)
		if !ok {
			return "", errors.New("this surface can't deliver an answer by marker")
		}
		var err error
		if delivered, err = finder.DeliveredChunks(ctx, conv, marker); err != nil {
			return "", err
		}
	}
	var firstID string
	for i, chunk := range chunks {
		var messageID string
		if marker != "" {
			chunkMarker := ChunkMarker(marker, i)
			chunk += "\n\n" + chunkMarker
			messageID = delivered[chunkMarker]
		}
		if messageID == "" {
			var err error
			messageID, err = e.Surface.PostChunk(ctx, conv, chunk, ChunkOptions{Last: i == len(chunks)-1, Marked: marker != ""})
			if err != nil {
				return firstID, fmt.Errorf("post answer chunk %d/%d: %w", i+1, len(chunks), err)
			}
			if messageID == "" && marker != "" {
				return firstID, errors.New("the post response has no message ID")
			}
		}
		if firstID == "" {
			firstID = messageID
		}
		if messageID != "" && sessionID != "" && e.Answers != nil {
			if err := e.Answers.Insert(ctx, conv.Place, messageID, sessionID, assistantTurn, i); err != nil {
				e.logger().Warn("ask: register answer message failed",
					"channel", conv.Place, "msg_ts", messageID,
					"session_id", sessionID, "turn", assistantTurn,
					"chunk", i, "err", err)
			}
		}
	}
	return firstID, nil
}

// ChunkMarker is the marker of an answer's index'th chunk.
func ChunkMarker(marker string, index int) string {
	if index == 0 {
		return marker
	}
	return fmt.Sprintf("%s chunk %d", marker, index+1)
}

// DeliverArtifacts attaches a turn's artifacts to conv, each at most once:
// a delivery already recorded for surface (the surface's name, or another
// delivery's, such as a scheduled report's) and conv is skipped, and each
// attempt is recorded.
func (e *Engine) DeliverArtifacts(ctx context.Context, conv Conversation, surface string, sessionID uuid.UUID, assistantTurn int, artifacts []ask.ArtifactDescriptor) error {
	if len(artifacts) == 0 {
		return nil
	}
	router, ok := e.Ask.(ArtifactRouter)
	if !ok {
		return errors.New("ask artifact delivery is unavailable")
	}
	target := conv.Place + ":" + conv.Thread
	for _, artifact := range artifacts {
		delivered, err := router.ArtifactDeliveryExists(ctx, sessionID, assistantTurn, artifact.ID, surface, target)
		if err != nil {
			return fmt.Errorf("check panel delivery %s: %w", artifact.ID, err)
		}
		if delivered {
			continue
		}
		record, data, err := router.ReadArtifact(ctx, sessionID, artifact.ID)
		if err != nil {
			_ = router.RecordArtifactDelivery(ctx, sessionID, assistantTurn, artifact.ID, surface, target, "", "failed", err.Error())
			return fmt.Errorf("read panel %s: %w", artifact.ID, err)
		}
		fileID, err := e.Surface.Upload(ctx, conv, File{
			Name: "grafana-panel-" + artifact.ID.String() + ".png", Title: artifact.Title, AltText: artifact.AltText, Data: data,
		})
		if err != nil {
			_ = router.RecordArtifactDelivery(ctx, sessionID, assistantTurn, artifact.ID, surface, target, "", "failed", err.Error())
			return fmt.Errorf("upload panel %s: %w", artifact.ID, err)
		}
		if record.ContentHash == "" || fileID == "" {
			err = errors.New("the panel upload returned no file ID")
			_ = router.RecordArtifactDelivery(ctx, sessionID, assistantTurn, artifact.ID, surface, target, "", "failed", err.Error())
			return err
		}
		if err := router.RecordArtifactDelivery(ctx, sessionID, assistantTurn, artifact.ID, surface, target, fileID, "delivered", ""); err != nil {
			return fmt.Errorf("record panel delivery %s: %w", artifact.ID, err)
		}
	}
	return nil
}

// Split breaks s into chunks of up to limit UTF-8 bytes, preferring to cut
// at line breaks and otherwise at a rune boundary:
//
//   - Empty input returns []string{""}; callers decide what to post.
//   - len(s) ≤ limit, or limit ≤ 0, returns s whole.
//   - Each chunk ends at the last '\n' at or before the limit, or, with
//     none, at the limit walked back to a rune start (or the limit itself
//     on a run of malformed continuation bytes, rather than loop).
//   - Exactly one '\n' is dropped between chunks, so a paragraph break
//     ("foo\n\nbar") or a blank line in a code block survives as a leading
//     blank line of the next chunk.
//
// It is not code-fence-aware: a cut inside a fenced block leaves one chunk
// rendered as code that should not be.
func Split(s string, limit int) []string {
	if s == "" {
		return []string{""}
	}
	if limit <= 0 || len(s) <= limit {
		return []string{s}
	}
	var out []string
	rest := s
	for len(rest) > 0 {
		if len(rest) <= limit {
			out = append(out, rest)
			break
		}
		cut := strings.LastIndexByte(rest[:limit], '\n')
		if cut <= 0 {
			cut = limit
			for cut > 0 && !utf8.RuneStart(rest[cut]) {
				cut--
			}
			if cut == 0 {
				cut = limit
			}
		}
		out = append(out, rest[:cut])
		rest = strings.TrimPrefix(rest[cut:], "\n")
	}
	return out
}
