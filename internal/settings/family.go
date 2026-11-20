package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrExists means a row of that name is already saved.
var ErrExists = errors.New("settings: a row of that name already exists")

// Family is rows an administrator adds and removes, beyond the registered
// connectors: Ask's tool connections, and the model providers. Each is a
// connector_settings row
// named Prefix plus the row's own name, saved, tested, turned on and off,
// cached and audited like a registered connector's, by the Definition that
// Define returns for that row. Supervisors never run them: what uses them
// reads them from the cache.
type Family struct {
	Prefix string
	Define func(row string) Definition
}

// has reports whether row is one of the family's.
func (f *Family) has(row string) bool {
	return f != nil && len(row) > len(f.Prefix) && strings.HasPrefix(row, f.Prefix)
}

// WithFamily adds a family of rows. Call it before Load. No family's prefix
// may start another's.
func (s *Store) WithFamily(family Family) *Store {
	for _, other := range s.families {
		if strings.HasPrefix(family.Prefix, other.Prefix) || strings.HasPrefix(other.Prefix, family.Prefix) {
			panic(fmt.Sprintf("settings: family prefixes %q and %q overlap", family.Prefix, other.Prefix))
		}
	}
	s.families = append(s.families, &family)
	return s
}

// familyOf returns the family row belongs to, or nil.
func (s *Store) familyOf(row string) *Family {
	for _, family := range s.families {
		if family.has(row) {
			return family
		}
	}
	return nil
}

// familyWith returns the family whose prefix is prefix, or nil.
func (s *Store) familyWith(prefix string) *Family {
	for _, family := range s.families {
		if family.Prefix == prefix {
			return family
		}
	}
	return nil
}

// refreshFamily caches the families' rows that versions lists and
// changed, and drops the cached rows it no longer lists.
func (s *Store) refreshFamily(ctx context.Context, versions map[string]int64, all bool) error {
	for name, version := range versions {
		family := s.familyOf(name)
		if family == nil {
			continue
		}
		if entry, ok := s.cached(name); ok && !all && entry.row.Version == version {
			continue
		}
		row, err := readRow(ctx, s.pool, name, "")
		if errors.Is(err, ErrUnknownConnector) {
			// Removed since the versions were listed.
			continue
		}
		if err != nil {
			return err
		}
		s.storeConnector(family.Define(name), row)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.connectors {
		if _, listed := versions[name]; s.familyOf(name) != nil && !listed {
			delete(s.connectors, name)
		}
	}
	return nil
}

// FamilyRow is one of the family's rows as the cache holds it.
type FamilyRow struct {
	Row Row
	// Resolved is its settings with their secrets, while it is enabled and
	// they could be read; Err is why they could not.
	Resolved Resolved
	Err      error
}

// FamilyRows returns the cached rows of the family whose prefix is prefix,
// in name order.
func (s *Store) FamilyRows(prefix string) []FamilyRow {
	family := s.familyWith(prefix)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var rows []FamilyRow
	for name, entry := range s.connectors {
		if family.has(name) {
			rows = append(rows, FamilyRow{Row: entry.row, Resolved: entry.resolved, Err: entry.err})
		}
	}
	slices.SortFunc(rows, func(a, b FamilyRow) int { return strings.Compare(a.Row.Connector, b.Row.Connector) })
	return rows
}

// ListFamily reads the rows of the family whose prefix is prefix from the
// database, in name order.
func (s *Store) ListFamily(ctx context.Context, prefix string) ([]View, error) {
	family := s.familyWith(prefix)
	if family == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT connector FROM connector_settings
		 WHERE starts_with(connector, $1) AND connector <> $1
		 ORDER BY connector`, family.Prefix)
	if err != nil {
		return nil, fmt.Errorf("settings: list %s rows: %w", family.Prefix, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("settings: list %s rows: %w", family.Prefix, err)
	}
	views := make([]View, 0, len(names))
	for _, name := range names {
		view, err := s.View(ctx, name)
		if errors.Is(err, ErrUnknownConnector) {
			continue
		}
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// Create adds the family's row name, off, with settings, which are checked
// as a save checks them. It returns ErrExists when the name is taken. A new
// row's version starts at the time it is created, in microseconds, so a row
// made again under a removed row's name is newer than every version of the
// removed one, which a replica may still cache.
func (s *Store) Create(ctx context.Context, actor Actor, name string, settings map[string]any) (View, error) {
	family := s.familyOf(name)
	if family == nil {
		return View{}, fmt.Errorf("%w %q", ErrUnknownConnector, name)
	}
	def := family.Define(name)
	actor, err := normalizeActor(actor)
	if err != nil {
		return View{}, err
	}
	values, err := parseValues(def, settings, normalizeValues(def, nil))
	if err != nil {
		return View{}, err
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return View{}, fmt.Errorf("settings: encode %s: %w", name, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return View{}, fmt.Errorf("settings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		INSERT INTO connector_settings (connector, settings, secrets, version, updated_by, updated_at)
		VALUES ($1, $2, '{}', (extract(epoch FROM clock_timestamp()) * 1000000)::bigint, $3, now())
		ON CONFLICT (connector) DO NOTHING`, name, encoded, actor.Email)
	if err != nil {
		return View{}, fmt.Errorf("settings: create %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return View{}, ErrExists
	}
	row, err := readRow(ctx, tx, name, "")
	if err != nil {
		return View{}, err
	}
	details, err := json.Marshal(map[string]any{
		"version": row.Version, "changes": changedFields(map[string]any{}, normalizeValues(def, row.Settings)),
	})
	if err != nil {
		return View{}, fmt.Errorf("settings: encode audit: %w", err)
	}
	if err := audit(ctx, tx, actor, "connectors."+name+".create", name, details); err != nil {
		return View{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return View{}, fmt.Errorf("settings: commit: %w", err)
	}
	s.storeConnector(def, row)
	return s.view(def, row), nil
}

// Delete removes the family's row name, with its saved secrets, when its
// version is still version.
func (s *Store) Delete(ctx context.Context, actor Actor, name string, version int64) error {
	if s.familyOf(name) == nil {
		return fmt.Errorf("%w %q", ErrUnknownConnector, name)
	}
	actor, err := normalizeActor(actor)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("settings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := readRow(ctx, tx, name, "FOR UPDATE")
	if err != nil {
		return err
	}
	if current.Version != version {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `DELETE FROM connector_settings WHERE connector = $1`, name); err != nil {
		return fmt.Errorf("settings: delete %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM source_state WHERE source = $1`, name); err != nil {
		return fmt.Errorf("settings: delete %s state: %w", name, err)
	}
	details, err := json.Marshal(map[string]any{"version": current.Version, "enabled": current.Enabled})
	if err != nil {
		return fmt.Errorf("settings: encode audit: %w", err)
	}
	if err := audit(ctx, tx, actor, "connectors."+name+".delete", name, details); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("settings: commit: %w", err)
	}
	s.mu.Lock()
	delete(s.connectors, name)
	s.mu.Unlock()
	return nil
}
