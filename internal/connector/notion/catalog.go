package notion

import (
	"context"
	"fmt"
)

// CatalogResolver understands the database/container/data-source/view split
// in the current Notion API. It deliberately resolves linked views to their
// backing data sources so a database embedded under a managed root is
// crawlable even when the container's data_sources array is empty.
type CatalogResolver struct {
	Client *Client
}

// ResolveDatabase returns every distinct data source directly owned by or
// linked into a database container. A complete result requires both the
// database metadata and its view listing to succeed.
func (r *CatalogResolver) ResolveDatabase(ctx context.Context, databaseID string) ([]*DataSource, error) {
	if r == nil || r.Client == nil {
		return nil, fmt.Errorf("notion catalog: nil client")
	}
	db, err := r.Client.GetDatabase(ctx, databaseID)
	if err != nil {
		return nil, fmt.Errorf("notion catalog: get database %s: %w", databaseID, err)
	}
	if db.InTrash {
		return nil, fmt.Errorf("notion catalog: database %s is in trash", databaseID)
	}

	ids := make(map[string]struct{}, len(db.DataSources))
	ordered := make([]string, 0, len(db.DataSources))
	add := func(id string) {
		if id == "" {
			return
		}
		if canonical, ok := canonicalize(id); ok {
			id = canonical
		}
		if _, exists := ids[id]; exists {
			return
		}
		ids[id] = struct{}{}
		ordered = append(ordered, id)
	}
	for _, ref := range db.DataSources {
		add(ref.ID)
	}

	views, err := r.Client.ListAllViewsForDatabase(ctx, db.ID)
	if err != nil {
		return nil, fmt.Errorf("notion catalog: list views for database %s: %w", databaseID, err)
	}
	for _, partial := range views {
		view, err := r.Client.GetView(ctx, partial.ID)
		if err != nil {
			if IsNotFound(err) || IsForbidden(err) {
				continue
			}
			return nil, fmt.Errorf("notion catalog: get view %s: %w", partial.ID, err)
		}
		add(view.DataSourceID)
	}

	out := make([]*DataSource, 0, len(ordered))
	for _, id := range ordered {
		ds, err := r.Client.GetDataSource(ctx, id)
		if err != nil {
			if IsNotFound(err) || IsForbidden(err) {
				continue
			}
			return nil, fmt.Errorf("notion catalog: get data source %s: %w", id, err)
		}
		if ds.InTrash {
			continue
		}
		out = append(out, ds)
	}
	return out, nil
}

// ResolveView retrieves a view and its backing data source.
func (r *CatalogResolver) ResolveView(ctx context.Context, viewID string) (*View, *DataSource, error) {
	if r == nil || r.Client == nil {
		return nil, nil, fmt.Errorf("notion catalog: nil client")
	}
	view, err := r.Client.GetView(ctx, viewID)
	if err != nil {
		return nil, nil, fmt.Errorf("notion catalog: get view %s: %w", viewID, err)
	}
	if view.DataSourceID == "" {
		return nil, nil, fmt.Errorf("notion catalog: view %s has no data source", viewID)
	}
	ds, err := r.Client.GetDataSource(ctx, view.DataSourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("notion catalog: get data source %s: %w", view.DataSourceID, err)
	}
	if ds.InTrash {
		return nil, nil, fmt.Errorf("notion catalog: data source %s is in trash", ds.ID)
	}
	return view, ds, nil
}
