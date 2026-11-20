package notion

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RootType string

const (
	RootTypeAuto       RootType = "auto"
	RootTypePage       RootType = "page"
	RootTypeDatabase   RootType = "database"
	RootTypeDataSource RootType = "data_source"
	RootTypeView       RootType = "view"
)

// RootRef is the canonical identity of a configured Notion resource.
type RootRef struct {
	Type     RootType
	ObjectID string
}

// Root is one authoritative starting point for a managed corpus walk.
type Root struct {
	ObjectType RootType
	ObjectID   string
	// PageID is a deprecated compatibility alias for ObjectID.
	PageID       string
	Label        string
	AddedAt      time.Time
	AddedBy      string
	LastWalkedAt *time.Time
	LastStatus   string
	LastError    string
}

type DiscoveryStatus struct {
	ObjectType RootType  `json:"object_type"`
	ObjectID   string    `json:"object_id"`
	ParentID   string    `json:"parent_id"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	FirstSeen  time.Time `json:"first_seen_at"`
	LastSeen   time.Time `json:"last_seen_at"`
	Covered    bool      `json:"covered"`
}

// RootsStore wraps the notion_roots table.
type RootsStore struct {
	pool  *pgxpool.Pool
	hider NotionHider
}

func (s *RootsStore) SetHider(hider NotionHider) { s.hider = hider }

// NewRootsStore wraps pool with the roots-table accessors.
func NewRootsStore(pool *pgxpool.Pool) *RootsStore {
	return &RootsStore{pool: pool}
}

// List returns all configured roots ordered by added_at ascending.
// Empty slice (not nil) on no rows.
func (s *RootsStore) List(ctx context.Context) ([]Root, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT object_type, object_id, label, added_at, added_by,
		       last_walked_at, last_walk_status, last_walk_error
		  FROM notion_roots
		 ORDER BY added_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("notion roots: query: %w", err)
	}
	defer rows.Close()
	out := []Root{}
	for rows.Next() {
		var r Root
		if err := rows.Scan(&r.ObjectType, &r.ObjectID, &r.Label, &r.AddedAt,
			&r.AddedBy, &r.LastWalkedAt, &r.LastStatus, &r.LastError); err != nil {
			return nil, fmt.Errorf("notion roots: scan: %w", err)
		}
		r.PageID = r.ObjectID
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListDiscoveries returns best-effort Search discoveries annotated with
// whether an active page or direct root proves that the data source is already
// covered. Uncovered rows are review candidates, not an ingestion queue.
func (s *RootsStore) ListDiscoveries(ctx context.Context) ([]DiscoveryStatus, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.object_type, d.object_id, d.parent_id, d.title, d.url,
		       d.first_seen_at, d.last_seen_at,
		       EXISTS (
		           SELECT 1 FROM notion_roots r WHERE r.object_id = d.object_id
		       ) OR EXISTS (
		           SELECT 1 FROM notion_page_state p
		            WHERE p.access_state = 'active'
		              AND p.source_metadata #>> '{parent,data_source_id}' = d.object_id
		       ) AS covered
		  FROM notion_discovered_resources d
		 ORDER BY covered ASC, d.last_seen_at DESC, d.title ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("notion discoveries: query: %w", err)
	}
	defer rows.Close()
	out := []DiscoveryStatus{}
	for rows.Next() {
		var item DiscoveryStatus
		if err := rows.Scan(&item.ObjectType, &item.ObjectID, &item.ParentID,
			&item.Title, &item.URL, &item.FirstSeen, &item.LastSeen, &item.Covered); err != nil {
			return nil, fmt.Errorf("notion discoveries: scan: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notion discoveries: rows: %w", err)
	}
	return out, nil
}

// Add inserts (or upserts) a root. Operator-supplied input is
// parsed via ParseRootInput which accepts either a Notion URL
// (with workspace + slug) or a bare UUID. Returns the canonical
// dashed UUID that was stored.
//
// Idempotent: re-adding an existing root updates the label /
// added_by but preserves added_at + last_walked_at.
func (s *RootsStore) Add(ctx context.Context, input, label, addedBy string) (string, error) {
	ref, err := ParseRootReference(input)
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO notion_roots (object_type, object_id, label, added_by)
		     VALUES ($1, $2, $3, $4)
		ON CONFLICT (object_type, object_id) DO UPDATE
		   SET label = EXCLUDED.label,
		       added_by = EXCLUDED.added_by
	`, ref.Type, ref.ObjectID, label, addedBy)
	if err != nil {
		return "", fmt.Errorf("notion roots: insert: %w", err)
	}
	return ref.ObjectID, nil
}

// Remove deletes a root. Accepts the same UUID-or-URL input shape
// as Add. Returns true if a row was deleted.
func (s *RootsStore) Remove(ctx context.Context, input string) (bool, error) {
	ref, err := ParseRootReference(input)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("notion roots: begin remove: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var postCommit []func(context.Context)
	var ignored int
	lockErr := tx.QueryRow(ctx, `
		SELECT 1 FROM notion_roots
		 WHERE object_type = $1 AND object_id = $2 FOR UPDATE
	`, ref.Type, ref.ObjectID).Scan(&ignored)
	switch {
	case lockErr == nil:
	case errors.Is(lockErr, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("notion roots: lock remove target: %w", lockErr)
	}

	rows, err := tx.Query(ctx, `
		SELECT DISTINCT candidate.page_id
		  FROM notion_page_roots candidate
		 WHERE candidate.root_object_type = $1 AND candidate.root_object_id = $2
		   AND NOT EXISTS (
		       SELECT 1 FROM notion_page_roots other
		        WHERE other.page_id = candidate.page_id
		          AND (other.root_object_type, other.root_object_id) <> ($1, $2))
	`, ref.Type, ref.ObjectID)
	if err != nil {
		return false, fmt.Errorf("notion roots: find orphaned pages: %w", err)
	}
	var orphaned []string
	for rows.Next() {
		var pageID string
		if err := rows.Scan(&pageID); err != nil {
			rows.Close()
			return false, fmt.Errorf("notion roots: scan orphaned page: %w", err)
		}
		orphaned = append(orphaned, pageID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("notion roots: orphaned pages: %w", err)
	}
	rows.Close()
	if len(orphaned) > 0 && s.hider == nil {
		return false, fmt.Errorf("notion roots: removal would expose %d orphaned pages without a tombstone writer", len(orphaned))
	}
	for _, pageID := range orphaned {
		post, err := s.hider.HideNotionTx(ctx, tx, pageID)
		if err != nil {
			return false, fmt.Errorf("notion roots: hide page %s: %w", pageID, err)
		}
		postCommit = append(postCommit, post)
		if _, err := tx.Exec(ctx, `
			UPDATE notion_page_state SET access_state = 'removed' WHERE page_id = $1
		`, pageID); err != nil {
			return false, fmt.Errorf("notion roots: mark page %s removed: %w", pageID, err)
		}
	}
	var tag pgconn.CommandTag
	tag, err = tx.Exec(ctx, `DELETE FROM notion_roots WHERE object_type = $1 AND object_id = $2`, ref.Type, ref.ObjectID)
	if err != nil {
		return false, fmt.Errorf("notion roots: delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("notion roots: commit remove: %w", err)
	}
	for _, post := range postCommit {
		post(ctx)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkWalked stamps last_walked_at on a root after a successful tree walk.
// A concurrently removed root is a successful no-op; storage errors remain
// actionable because they can also indicate a broader database failure.
func (s *RootsStore) MarkWalkedRef(ctx context.Context, ref RootRef, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE notion_roots
		   SET last_walked_at = $3,
		       last_walk_status = 'complete',
		       last_walk_error = ''
		 WHERE object_type = $1 AND object_id = $2
	`, ref.Type, ref.ObjectID, at)
	if err != nil {
		return fmt.Errorf("notion roots: mark walked: %w", err)
	}
	return nil
}

// MarkWalked preserves the page-ID-only API for older callers. Migrated roots
// are stored as auto, so this targets that compatibility type.
func (s *RootsStore) MarkWalked(ctx context.Context, objectID string, at time.Time) error {
	return s.MarkWalkedRef(ctx, RootRef{Type: RootTypeAuto, ObjectID: objectID}, at)
}

// MarkFailed records a failed root preflight/walk without advancing its last
// successful completion timestamp.
func (s *RootsStore) MarkFailed(ctx context.Context, ref RootRef, walkErr error) error {
	message := ""
	if walkErr != nil {
		message = walkErr.Error()
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE notion_roots
		   SET last_walk_status = 'failed', last_walk_error = $3
		 WHERE object_type = $1 AND object_id = $2
	`, ref.Type, ref.ObjectID, message)
	if err != nil {
		return fmt.Errorf("notion roots: mark failed: %w", err)
	}
	return nil
}

// uuidRE matches a Notion UUID with or without dashes. Notion
// UUIDs are 32 hex chars; the dashed form has the canonical 8-4-
// 4-4-12 split.
var uuidRE = regexp.MustCompile(`^([0-9a-fA-F]{8})-?([0-9a-fA-F]{4})-?([0-9a-fA-F]{4})-?([0-9a-fA-F]{4})-?([0-9a-fA-F]{12})$`)

// ParseRootInput accepts either:
//
//   - a Notion page URL:
//     "https://www.notion.so/some-workspace/Some-Page-abcd1234ef567890abcd1234567890ab"
//   - a Notion page URL with no workspace prefix:
//     "https://notion.so/Some-Page-abcd1234ef567890abcd1234567890ab"
//   - a bare UUID, dashed or undashed
//
// and returns the canonical dashed UUID. Errors on shapes that
// don't contain a recognizable UUID.
func ParseRootReference(s string) (RootRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return RootRef{}, &ValidationError{Message: "empty input"}
	}

	for _, candidate := range []struct {
		prefix string
		kind   RootType
	}{
		{"page:", RootTypePage},
		{"database:", RootTypeDatabase},
		{"data_source:", RootTypeDataSource},
		{"view:", RootTypeView},
		{"auto:", RootTypeAuto},
	} {
		if strings.HasPrefix(strings.ToLower(s), candidate.prefix) {
			id, ok := canonicalize(strings.TrimSpace(s[len(candidate.prefix):]))
			if !ok {
				return RootRef{}, &ValidationError{Message: fmt.Sprintf("invalid %s UUID", candidate.kind)}
			}
			return RootRef{Type: candidate.kind, ObjectID: id}, nil
		}
	}

	// URL? Look for the trailing UUID component.
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		u, err := url.Parse(s)
		if err != nil {
			return RootRef{}, &ValidationError{Message: fmt.Sprintf("parse URL: %v", err)}
		}
		host := strings.ToLower(u.Hostname())
		if host != "notion.so" && host != "www.notion.so" && host != "app.notion.com" {
			return RootRef{}, &ValidationError{Message: fmt.Sprintf("unsupported Notion host %q", host)}
		}
		if v := u.Query().Get("v"); v != "" {
			if dashed, ok := canonicalize(v); ok {
				return RootRef{Type: RootTypeView, ObjectID: dashed}, nil
			}
			return RootRef{}, &ValidationError{Message: "invalid view UUID in v parameter"}
		}
		if p := u.Query().Get("p"); p != "" {
			if dashed, ok := canonicalize(p); ok {
				return RootRef{Type: RootTypePage, ObjectID: dashed}, nil
			}
			return RootRef{}, &ValidationError{Message: "invalid page UUID in p parameter"}
		}
		// The page slug is the last path segment, of shape
		// "Some-Title-<32hex>". Strip the title part.
		segs := strings.Split(strings.TrimRight(u.Path, "/"), "/")
		if len(segs) == 0 {
			return RootRef{}, &ValidationError{Message: "empty URL path"}
		}
		last := segs[len(segs)-1]
		// Trailing 32 hex chars is the UUID. Most Notion page
		// URLs end "Title-<32hex>".
		if len(last) >= 32 {
			tail := last[len(last)-32:]
			if dashed, ok := canonicalize(tail); ok {
				return RootRef{Type: RootTypeAuto, ObjectID: dashed}, nil
			}
		}
		// Some links use the ?p=<uuid> form (block deep-links,
		// search-result URLs). uuidRE is anchored ^...$ so a
		// substring scan won't help; use the parsed query.
		return RootRef{}, &ValidationError{Message: fmt.Sprintf("no UUID found in URL %q", s)}
	}

	// Bare UUID, dashed or not.
	if dashed, ok := canonicalize(s); ok {
		return RootRef{Type: RootTypeAuto, ObjectID: dashed}, nil
	}
	return RootRef{}, &ValidationError{Message: fmt.Sprintf("input %q is not a Notion URL or UUID", s)}
}

// ParseRootInput is retained for callers that need only the canonical ID.
func ParseRootInput(s string) (string, error) {
	ref, err := ParseRootReference(s)
	return ref.ObjectID, err
}

// ValidationError distinguishes operator input errors from storage failures.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return "notion roots: " + e.Message }

func IsValidationError(err error) bool {
	var target *ValidationError
	return errors.As(err, &target)
}

// canonicalize accepts a 32-hex-char or canonically-dashed UUID
// and returns the dashed form.
func canonicalize(s string) (string, bool) {
	m := uuidRE.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return strings.ToLower(strings.Join(m[1:], "-")), true
}
