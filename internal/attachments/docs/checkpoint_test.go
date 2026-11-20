package docs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/attachments/files"
)

type memoryPageCheckpoints struct {
	byModel map[string]map[int]PageCheckpoint
}

func (m *memoryPageCheckpoints) Load(
	_ context.Context, modelID string,
) ([]PageCheckpoint, error) {
	var out []PageCheckpoint
	for _, page := range m.byModel[modelID] {
		out = append(out, page)
	}
	return out, nil
}

func (m *memoryPageCheckpoints) Save(
	_ context.Context, modelID string, page PageCheckpoint,
) (PageCheckpoint, error) {
	if m.byModel == nil {
		m.byModel = make(map[string]map[int]PageCheckpoint)
	}
	if m.byModel[modelID] == nil {
		m.byModel[modelID] = make(map[int]PageCheckpoint)
	}
	if saved, ok := m.byModel[modelID][page.PageNumber]; ok {
		return saved, nil
	}
	m.byModel[modelID][page.PageNumber] = page
	return page, nil
}

func TestPDFCheckpointResumeStartsAtNextPage(t *testing.T) {
	pdf := buildTestPDF(t, []string{
		densePage("Alpha clause."),
		"scan",
		densePage("Gamma clause."),
		"scan",
	})
	checkpoints := &memoryPageCheckpoints{}
	firstModel := &failAfterExtractor{
		failAfter:    1,
		err:          &files.TransientError{Underlying: errors.New("worker interrupted")},
		inputTokens:  100,
		outputTokens: 20,
	}

	_, err := testPDFParser(t, firstModel).ExtractWithCheckpoints(
		context.Background(), "pdf", "resume.pdf", pdf, checkpoints,
	)
	if err == nil || !files.IsTransient(err) {
		t.Fatalf("first extraction error = %v, want transient", err)
	}
	if firstModel.calls != 2 {
		t.Fatalf("first model calls = %d, want 2 (one success, one interruption)", firstModel.calls)
	}
	if got := len(checkpoints.byModel["fake-model"]); got != 1 {
		t.Fatalf("durable pages after interruption = %d, want 1", got)
	}
	secondModel := &sequenceExtractor{}
	second, err := testPDFParser(t, secondModel).ExtractWithCheckpoints(
		context.Background(), "pdf", "resume.pdf", pdf, checkpoints,
	)
	if err != nil {
		t.Fatalf("resumed extraction: %v", err)
	}
	if secondModel.calls != 1 {
		t.Fatalf("model calls after restart = %d, want 1; completed page was re-billed", secondModel.calls)
	}
	if second.PagesExtracted != second.PageCount || second.PageCount != 4 {
		t.Fatalf("resumed coverage = %d/%d, want 4/4", second.PagesExtracted, second.PageCount)
	}
	for _, want := range []string{"MODEL-OK-1", "MODEL-CALL-1"} {
		if !strings.Contains(second.Markdown, want) {
			t.Errorf("resumed markdown missing %q", want)
		}
	}
	if got := len(checkpoints.byModel["fake-model"]); got != 2 {
		t.Errorf("durable pages after resume = %d, want 2", got)
	}
}

type costlyExtractor struct {
	modelID      string
	inputTokens  int
	outputTokens int
	calls        int
}

type identifiedExtractor struct {
	sequenceExtractor
	identity string
}

func (e *identifiedExtractor) CheckpointIdentity() string { return e.identity }

func TestPDFCheckpointIdentityInvalidatesSameModelPages(t *testing.T) {
	pdf := buildTestPDF(t, []string{"scan", "scan"})
	checkpoints := &memoryPageCheckpoints{}
	first := &identifiedExtractor{identity: "fake-model@prompt-v1"}
	if _, err := testPDFParser(t, first).ExtractWithCheckpoints(
		context.Background(), "pdf", "prompt-change.pdf", pdf, checkpoints,
	); err != nil {
		t.Fatal(err)
	}
	if first.calls != 2 {
		t.Fatalf("first extraction calls = %d, want 2", first.calls)
	}
	second := &identifiedExtractor{identity: "fake-model@prompt-v2"}
	if _, err := testPDFParser(t, second).ExtractWithCheckpoints(
		context.Background(), "pdf", "prompt-change.pdf", pdf, checkpoints,
	); err != nil {
		t.Fatal(err)
	}
	if second.calls != 2 {
		t.Fatalf("changed prompt identity reused old pages; calls = %d, want 2", second.calls)
	}
}

func (e *costlyExtractor) ModelID() string { return e.modelID }

func (e *costlyExtractor) Extract(
	_ context.Context, _, _ string, _ []byte,
) (*Result, error) {
	e.calls++
	return &Result{
		Markdown:       fmt.Sprintf("--- PAGE 1 ---\nCOSTLY-%d", e.calls),
		PageCount:      1,
		PagesExtracted: 1,
		ModelID:        e.modelID,
		InputTokens:    e.inputTokens,
		OutputTokens:   e.outputTokens,
	}, nil
}

func TestPDFCostFuseStopsBeforeNextPage(t *testing.T) {
	pdf := buildTestPDF(t, []string{
		densePage("Alpha clause."),
		"scan",
		densePage("Gamma clause."),
		"scan",
	})
	model := &costlyExtractor{
		modelID:     testModelID,
		inputTokens: 4_000_001, // just over $10 at $2.50/M input
	}
	checkpoints := &memoryPageCheckpoints{}

	res, err := testPDFParser(t, model).ExtractWithCheckpoints(
		context.Background(), "pdf", "cost-fuse.pdf", pdf, checkpoints,
	)
	if err != nil {
		t.Fatalf("ExtractWithCheckpoints: %v", err)
	}
	if model.calls != 1 {
		t.Fatalf("model calls = %d, want 1; fuse started another page", model.calls)
	}
	if !res.CostLimited {
		t.Fatal("CostLimited = false after crossing the per-document fuse")
	}
	if res.CostUSD < maxDocumentCostUSD {
		t.Errorf("CostUSD = %.6f, want >= %.2f", res.CostUSD, maxDocumentCostUSD)
	}
	if res.PagesExtracted != 3 || res.PageCount != 4 {
		t.Errorf("coverage = %d/%d, want 3/4", res.PagesExtracted, res.PageCount)
	}
	if !strings.Contains(res.Markdown, "COSTLY-1") {
		t.Fatal("the paid page that crossed the fuse was discarded")
	}
	if got := len(checkpoints.byModel[testModelID]); got != 1 {
		t.Errorf("durable pages = %d, want 1", got)
	}
}

// TestExtractionCostOfAnUnlistedModel: a model the rate table lacks is still
// priced, at $2.50 and $15 per million tokens, so the fuse still trips.
func TestExtractionCostOfAnUnlistedModel(t *testing.T) {
	if got := extractionCostUSD("future-model", 1_000_000, 1_000_000); got != 17.5 {
		t.Errorf("unlisted model cost = %v, want 17.5", got)
	}
}
