package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// DiscoveredResource is coverage-audit metadata. Discovery never authorizes
// ingestion; only a successful path from a managed root can do that.
type DiscoveredResource struct {
	ObjectType RootType
	ObjectID   string
	ParentID   string
	Title      string
	URL        string
	EditedAt   time.Time
}

type DiscoveryBatch struct {
	Resources  []DiscoveredResource
	NextCursor string
	Done       bool
}

// DiscoverDataSourcesPage reads one best-effort Search page. Notion documents
// Search as eventually consistent and non-exhaustive, so this result feeds an
// audit catalog only and is never passed to the event ingester.
func (w *Walker) DiscoverDataSourcesPage(ctx context.Context, cutoff time.Time, cursor string) (DiscoveryBatch, error) {
	res, err := w.Client.Search(ctx, SearchParams{
		FilterObject: "data_source", SortDirection: "descending",
		StartCursor: cursor, PageSize: 100,
	})
	if err != nil {
		return DiscoveryBatch{}, err
	}

	batch := DiscoveryBatch{NextCursor: res.NextCursor}
	for _, raw := range res.Results {
		var ds DataSource
		if err := json.Unmarshal(raw, &ds); err != nil {
			return DiscoveryBatch{}, fmt.Errorf("notion discovery: decode data source: %w", err)
		}
		if ds.Object != "data_source" || ds.ID == "" {
			return DiscoveryBatch{}, fmt.Errorf("notion discovery: unexpected object %q id=%q", ds.Object, ds.ID)
		}
		if !cutoff.IsZero() && ds.LastEditedTime.Before(cutoff) {
			batch.Done = true
			break
		}
		id := ds.ID
		if canonical, ok := canonicalize(id); ok {
			id = canonical
		}
		parentID := ds.Parent.DatabaseID
		if parentID == "" {
			parentID = ds.DatabaseParent.PageID
		}
		batch.Resources = append(batch.Resources, DiscoveredResource{
			ObjectType: RootTypeDataSource,
			ObjectID:   id,
			ParentID:   parentID,
			Title:      plainText(ds.Title),
			URL:        ds.URL,
			EditedAt:   ds.LastEditedTime,
		})
	}
	if batch.Done || !res.HasMore {
		batch.Done = true
		return batch, nil
	}
	if res.NextCursor == "" {
		return DiscoveryBatch{}, fmt.Errorf("notion discovery: has_more without next_cursor")
	}
	return batch, nil
}
