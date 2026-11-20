package docs

import "context"

// PageCheckpoint is one completed sparse PDF page.
type PageCheckpoint struct {
	PageNumber int
	Markdown   string
	CostUSD    float64
	Truncated  bool
}

// PageCheckpointStore is scoped to one document and source hash.
type PageCheckpointStore interface {
	Load(ctx context.Context, modelID string) ([]PageCheckpoint, error)
	Save(ctx context.Context, modelID string, page PageCheckpoint) (PageCheckpoint, error)
}

// CheckpointingExtractor resumes completed pages.
type CheckpointingExtractor interface {
	ExtractWithCheckpoints(
		ctx context.Context,
		docFormat, filename string,
		data []byte,
		checkpoints PageCheckpointStore,
	) (*Result, error)
}
