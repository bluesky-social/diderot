package sessioningest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/bluesky-social/diderot/internal/sessionsummary"
)

// Summarizer is the repository-native renderer that turns a validated
// Bsky Session v1 revision into searchable summary content. The worker
// invokes it BEFORE the atomic publish: it produces the body/embedding
// text (and, when a real model is available, persists canonical
// per-record-range chunk summaries + the reduce summary). It never
// returns a flattened raw transcript as if it were a summary — the
// transcript is always transformed into repository-native summary
// content.
type Summarizer interface {
	Summarize(ctx context.Context, doc *sessionDocument, id Identity, revision string, rewriteEpoch int64) (SummaryResult, error)
}

type SummaryResult struct {
	Narrative     string
	EmbeddingText string
	Title         string
	TitleModel    string
	ContextHint   string
	Tags          []string
	ModelID       string
	InputTokens   int32
	OutputTokens  int32
	Chunks        []SummaryRecord
	// InputDigest fingerprints logical map/reduce inputs. It deliberately
	// excludes session revision/envelope metadata and the recovery-only
	// reduce input cap, retaining already-verified successful summaries. It
	// names the summary of these chunks under these prompts and models,
	// whether one reduce made it or it was folded from an earlier summary.
	InputDigest string
	Reused      bool
	// ReduceEntries is how many summaries the reduce call read: every chunk's,
	// or the earlier session summary plus the chunks it had not read.
	ReduceEntries int
}

// summaryInputVersion must change when tool schemas or rendering semantics
// change beyond the inputs fingerprinted below. The recovery-only reduce cap
// deliberately preserves previously successful summaries and embeddings.
const summaryInputVersion = "sessioningest-summary-input-v1"
const mapInputVersion = "sessioningest-map-input-v2"

// The model IDs every summary stored before input digests was generated
// with. Such a summary is adopted without inference only while these are
// still the map and reduce models (and the prompts match
// sessionsummary.IsLegacyCacheCompatible); they are compared, never
// requested or used as a default.
const (
	legacySummaryMapModel    = "claude-haiku-4-5:api"
	legacySummaryReduceModel = "claude-sonnet-5:api"
)

const defaultChunkConcurrency = 20

// sessionsummarySummarizer is the native map/reduce pipeline. It uses the
// shared chunker, prompt, and model-call helpers over the Bsky record body,
// persists canonical-identity
// per-record-range chunk summaries and the reduce summary (sessioningest
// store, keyed on (owner, client, session_id)), and returns the reduce
// narrative + embedding text so the atomic corpus publish can render the
// real summary — not a raw transcript.
type sessionsummarySummarizer struct {
	sis              *Store // sessioningest store (canonical chunk/summary persistence)
	prompts          sessionsummary.PromptReleaseReader
	client           sessionsummary.ToolClient
	chunkBytes       int
	mapModel         string
	reduceModel      string
	chunkConcurrency int
	modelCalls       *semaphore.Weighted
}

// NewSessionsummarySummarizer wires the real map/reduce summarizer.
// mapModel/reduceModel are model gateway model IDs (empty reduce reuses
// map). chunkBytes targets chunk size (0 = package default).
func NewSessionsummarySummarizer(sis *Store, prompts sessionsummary.PromptReleaseReader, client sessionsummary.ToolClient, mapModel, reduceModel string, chunkBytes, chunkConcurrency int) Summarizer {
	if reduceModel == "" {
		reduceModel = mapModel
	}
	if chunkConcurrency <= 0 {
		chunkConcurrency = defaultChunkConcurrency
	}
	return &sessionsummarySummarizer{
		sis:              sis,
		prompts:          prompts,
		client:           client,
		chunkBytes:       chunkBytes,
		mapModel:         mapModel,
		reduceModel:      reduceModel,
		chunkConcurrency: chunkConcurrency,
		modelCalls:       semaphore.NewWeighted(int64(chunkConcurrency)),
	}
}

// chunkRecords splits the parser's bounded, allowlisted record NDJSON into
// shared chunk views. RecordStart/RecordEnd cover the original record indices.
func (s *sessionsummarySummarizer) chunkRecords(doc *sessionDocument, chunkBytes int) []sessionsummary.Chunk {
	if chunkBytes <= 0 {
		chunkBytes = sessionsummary.DefaultChunkBytes
	}
	chunks := sessionsummary.ChunkTranscript(doc.Transcript, chunkBytes)
	// The chunker yields RecordStart/RecordEnd as offsets; when it falls back
	// to whole-blob (empty/invalid), pin the range to the full document.
	if len(chunks) == 0 {
		chunks = []sessionsummary.Chunk{{RecordStart: 0, RecordEnd: doc.RecordCount}}
	}
	return chunks
}

// Summarize runs the real map/reduce pipeline and returns the summary
// body/embedding. It persists per-range chunks and the reduce summary
// through the sessioningest store (canonical identity).
func (s *sessionsummarySummarizer) Summarize(ctx context.Context, doc *sessionDocument, id Identity, revision string, rewriteEpoch int64) (SummaryResult, error) {
	if s.prompts == nil || s.client == nil || s.sis == nil {
		return SummaryResult{}, errors.New("sessioningest: sessionsummary summarizer not fully wired")
	}
	release, err := s.prompts.Current(ctx)
	if err != nil {
		return SummaryResult{}, fmt.Errorf("sessioningest: load active prompt release: %w", err)
	}
	bundle, err := sessionsummary.PromptBundleFromCatalog(release.Bundle.Render(promptcatalog.Values()))
	if err != nil {
		return SummaryResult{}, fmt.Errorf("sessioningest: resolve summary prompts from release %d: %w", release.ID, err)
	}

	chunks := s.chunkRecords(doc, s.chunkBytes)
	if len(chunks) == 0 {
		return SummaryResult{}, errors.New("sessioningest: no chunks produced for summary")
	}

	mapOpts := sessionsummary.CallOptions{ModelID: s.mapModel, MaxTokens: 1024}
	reduceOpts := sessionsummary.CallOptions{ModelID: s.reduceModel, MaxTokens: 4096}
	mapDigest := summaryMapInputDigest(bundle, "session_summary", mapOpts)
	inputDigest := summaryInputDigest(bundle, "session_summary", mapOpts, reduceOpts, chunks)
	cached, ok, err := s.sis.LoadReusableSummary(ctx, id, inputDigest, doc.RecordCount, chunks, s.mapModel)
	if err != nil {
		return SummaryResult{}, fmt.Errorf("sessioningest: load reusable summary: %w", err)
	}
	if ok {
		cached.InputDigest = inputDigest
		cached.Reused = true
		return cached, nil
	}
	// Rows generated before input digests were introduced can be adopted
	// without inference only for the one historical prompt/model bundle we
	// can prove. The store then verifies the recomputed sanitized bytes,
	// exact chunk ranges/hashes, model IDs, and complete coverage while
	// stamping both digest layers transactionally.
	if sessionsummary.IsLegacyCacheCompatible(bundle) &&
		s.mapModel == legacySummaryMapModel && s.reduceModel == legacySummaryReduceModel {
		cached, ok, err = s.sis.AdoptLegacySummary(ctx, id, inputDigest, mapDigest,
			doc.RecordCount, chunks, s.mapModel)
		if err != nil {
			return SummaryResult{}, fmt.Errorf("sessioningest: adopt legacy summary: %w", err)
		}
		if ok {
			cached.Reused = true
			return cached, nil
		}
	}

	var (
		reduceInputs  = make([]sessionsummary.ChunkSummary, len(chunks))
		totalIn       int
		totalOut      int
		usedModel     = s.mapModel
		summaryChunks = make([]SummaryRecord, len(chunks))
	)
	reusedMap, err := s.sis.LoadReusableMapChunks(ctx, id, mapDigest, doc.RecordCount, chunks, s.mapModel)
	if err != nil {
		return SummaryResult{}, fmt.Errorf("sessioningest: load reusable map chunks: %w", err)
	}
	// The map model reads each chunk in its lean rendering. That rendering is
	// deliberately outside the cache identity, which is the chunk's full
	// bytes: a chunk mapped before the rendering existed, or under other
	// rules, stays a valid summary of the same records and is never mapped
	// again for it.
	var lean sessionsummary.LeanContext
	if len(reusedMap) < len(chunks) {
		lean = sessionsummary.NewLeanContext(doc.Transcript)
	}
	chunkUsage := make([]sessionsummary.Usage, len(chunks))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(s.chunkConcurrency)
	for index, chunk := range chunks {
		if groupCtx.Err() != nil {
			break
		}
		if cached, ok := reusedMap[index]; ok {
			summaryChunks[index] = cached
			reduceInputs[index] = sessionsummary.ChunkSummary{
				Evidence: sessionsummary.StringList(cached.Evidence),
				Summary:  cached.Summary,
			}
			continue
		}
		group.Go(func() error {
			if err := s.modelCalls.Acquire(groupCtx, 1); err != nil {
				return err
			}
			defer s.modelCalls.Release(1)
			mapped, usage, err := sessionsummary.MapCall(groupCtx, s.client, bundle, mapOpts,
				lean.Render(chunk), index+1, len(chunks))
			if err != nil {
				return fmt.Errorf("sessioningest: map chunk %d/%d: %w", index+1, len(chunks), err)
			}
			record := SummaryRecord{
				Identity:    id,
				RecordStart: chunk.RecordStart,
				RecordEnd:   chunk.RecordEnd,
				ContentHash: shaHex(chunk.Bytes),
				MapDigest:   mapDigest,
				Evidence:    []string(mapped.Evidence),
				Summary:     mapped.Summary,
				ModelID:     s.mapModel,
			}
			if err := s.sis.CheckpointMapChunk(groupCtx, record, revision, rewriteEpoch); err != nil {
				return err
			}
			reduceInputs[index] = mapped
			chunkUsage[index] = usage
			summaryChunks[index] = record
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return SummaryResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return SummaryResult{}, err
	}
	for _, usage := range chunkUsage {
		totalIn += usage.InputTokens
		totalOut += usage.OutputTokens
	}

	folded, ok, err := s.foldEarlierSummary(ctx, id, bundle, mapOpts, reduceOpts, doc, chunks, reduceInputs)
	if err != nil {
		return SummaryResult{}, err
	}
	if ok {
		reduceInputs = folded
	}
	if err := s.modelCalls.Acquire(ctx, 1); err != nil {
		return SummaryResult{}, err
	}
	final, usage, err := sessionsummary.ReduceCall(ctx, s.client, bundle, reduceOpts, reduceInputs)
	s.modelCalls.Release(1)
	if err != nil {
		return SummaryResult{}, fmt.Errorf("sessioningest: reduce: %w", err)
	}
	totalIn += usage.InputTokens
	totalOut += usage.OutputTokens

	return SummaryResult{
		Narrative:     final.Narrative,
		EmbeddingText: final.EmbeddingText,
		ContextHint:   final.ContextHint,
		Tags:          final.Tags.Flatten(),
		ModelID:       usedModel,
		InputTokens:   int32(totalIn),
		OutputTokens:  int32(totalOut),
		Chunks:        summaryChunks,
		InputDigest:   inputDigest,
		ReduceEntries: len(reduceInputs),
	}, nil
}

// foldEarlierSummary returns the reduce input that updates the session's
// published summary instead of rebuilding it: that summary, standing in for
// the chunks it was made from, then only the chunks it has not read (the
// grown last chunk and new ones). Each chunk summary therefore reaches the
// reduce model about once, however often a growing session is published.
//
// The published summary qualifies only when the current prompts and models
// made it from exactly the chunks that the current transcript's first
// records produce. Content changed earlier in the session, a rewind, or a
// prompt, model or chunk-size change reduces over every chunk instead.
func (s *sessionsummarySummarizer) foldEarlierSummary(ctx context.Context, id Identity, bundle sessionsummary.PromptBundle, mapOpts, reduceOpts sessionsummary.CallOptions, doc *sessionDocument, chunks []sessionsummary.Chunk, summaries []sessionsummary.ChunkSummary) ([]sessionsummary.ChunkSummary, bool, error) {
	earlier, ok, err := s.sis.LoadEarlierSummary(ctx, id)
	if err != nil {
		return nil, false, fmt.Errorf("sessioningest: load earlier summary: %w", err)
	}
	if !ok || strings.TrimSpace(earlier.Narrative) == "" || earlier.InputDigest == "" ||
		earlier.ModelID != s.mapModel || !validReusableSummaryChunks(earlier.Chunks, earlier.RecordCount) {
		return nil, false, nil
	}
	prefix, ok := sessionsummary.RecordPrefix(doc.Transcript, earlier.RecordCount)
	if !ok {
		return nil, false, nil
	}
	covered := sessionsummary.ChunkTranscript(prefix, s.chunkBytes)
	if len(covered) != len(earlier.Chunks) || len(covered) > len(chunks) {
		return nil, false, nil
	}
	for index, chunk := range covered {
		stored := earlier.Chunks[index]
		if stored.RecordStart != chunk.RecordStart || stored.RecordEnd != chunk.RecordEnd ||
			stored.ContentHash != shaHex(chunk.Bytes) {
			return nil, false, nil
		}
		current := chunks[index]
		if current.RecordStart != chunk.RecordStart ||
			(index < len(covered)-1 && current.RecordEnd != chunk.RecordEnd) {
			return nil, false, nil
		}
	}
	if summaryInputDigest(bundle, "session_summary", mapOpts, reduceOpts, covered) != earlier.InputDigest {
		return nil, false, nil
	}
	unread := len(covered)
	if chunks[unread-1].RecordEnd != covered[unread-1].RecordEnd {
		unread--
	}
	if unread == len(chunks) {
		return nil, false, nil
	}
	// A summary written before reduce output was repaired may still hold
	// its fields inside its narrative; fold it in repaired.
	text, _, _ := sessionsummary.RepairStoredSummary(sessionsummary.StoredSummary{
		Narrative: earlier.Narrative, EmbeddingText: earlier.EmbeddingText,
		ContextHint: earlier.ContextHint, Tags: earlier.Tags,
	})
	// The earlier retrieval fields, tags included, go along as its evidence:
	// the reduce writes them afresh and the chunks behind them are not sent.
	var evidence sessionsummary.StringList
	for _, field := range []string{text.ContextHint, text.EmbeddingText, strings.Join(text.Tags, ", ")} {
		if strings.TrimSpace(field) != "" {
			evidence = append(evidence, field)
		}
	}
	folded := make([]sessionsummary.ChunkSummary, 0, 1+len(chunks)-unread)
	// The label is always a range, "1-1" for one chunk, so the earlier
	// summary is never mistaken for a chunk of its own.
	folded = append(folded, sessionsummary.ChunkSummary{
		Label: fmt.Sprintf("1-%d", len(covered)), Evidence: evidence, Summary: text.Narrative,
	})
	for index := unread; index < len(chunks); index++ {
		entry := summaries[index]
		entry.Label = strconv.Itoa(index + 1)
		folded = append(folded, entry)
	}
	return folded, true, nil
}

func summaryMapInputDigest(bundle sessionsummary.PromptBundle, kind string, mapOpts sessionsummary.CallOptions) string {
	h := sha256.New()
	writeDigestField(h, []byte(mapInputVersion))
	writeDigestField(h, []byte(kind))
	writeDigestField(h, []byte(bundle.MapSystem))
	writeDigestField(h, []byte(mapOpts.ModelID))
	writeDigestInt(h, mapOpts.MaxTokens)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func summaryInputDigest(bundle sessionsummary.PromptBundle, kind string, mapOpts, reduceOpts sessionsummary.CallOptions, chunks []sessionsummary.Chunk) string {
	h := sha256.New()
	writeDigestField(h, []byte(summaryInputVersion))
	writeDigestField(h, []byte(kind))
	writeDigestField(h, []byte(bundle.MapSystem))
	writeDigestField(h, []byte(bundle.ReduceSystem))
	writeDigestStrings(h, bundle.LanguageEnum)
	writeDigestStrings(h, bundle.ActivityEnum)
	writeDigestField(h, []byte(mapOpts.ModelID))
	writeDigestInt(h, mapOpts.MaxTokens)
	writeDigestField(h, []byte(reduceOpts.ModelID))
	writeDigestInt(h, reduceOpts.MaxTokens)
	writeDigestInt(h, int64(len(chunks)))
	for i, chunk := range chunks {
		writeDigestInt(h, int64(i))
		writeDigestInt(h, int64(chunk.RecordStart))
		writeDigestInt(h, int64(chunk.RecordEnd))
		writeDigestField(h, chunk.Bytes)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func writeDigestStrings(h hash.Hash, values []string) {
	writeDigestInt(h, int64(len(values)))
	for _, value := range values {
		writeDigestField(h, []byte(value))
	}
}

func writeDigestField(h hash.Hash, value []byte) {
	writeDigestInt(h, int64(len(value)))
	_, _ = h.Write(value)
}

func writeDigestInt(h hash.Hash, value int64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(value))
	_, _ = h.Write(buf[:])
}
