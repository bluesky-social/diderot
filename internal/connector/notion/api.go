package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// uuidShape matches the canonical dashed Notion UUID. Only inputs
// that match this strip dashes when interpolated into the API
// path; freeform fixtures pass through unmodified.
var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Notion's wire types are vast and mostly typed-interface (a
// `paragraph` block has a `paragraph` field; a `heading_1` block
// has a `heading_1` field). We unmarshal the loose top-level
// shape and use json.RawMessage for the per-type body, deferring
// the parse into the renderer (parse.go).

// Page is the shape returned by /pages/{id} and data-source query results. Many fields
// are technically optional in the API — guard accesses on the
// caller side.
type Page struct {
	Object         string                     `json:"object"` // "page"
	ID             string                     `json:"id"`
	CreatedTime    time.Time                  `json:"created_time"`
	LastEditedTime time.Time                  `json:"last_edited_time"`
	CreatedBy      *PartialUser               `json:"created_by,omitempty"`
	LastEditedBy   *PartialUser               `json:"last_edited_by,omitempty"`
	Archived       bool                       `json:"archived"`
	InTrash        bool                       `json:"in_trash"`
	Properties     map[string]json.RawMessage `json:"properties"`
	Parent         Parent                     `json:"parent"`
	URL            string                     `json:"url"`
}

// Database is the shape returned by /databases/{id} and /search
// results when object="database". Title is an array of rich-text
// fragments — flatten via plainText (parse.go).
type Database struct {
	Object         string                     `json:"object"` // "database"
	ID             string                     `json:"id"`
	CreatedTime    time.Time                  `json:"created_time"`
	LastEditedTime time.Time                  `json:"last_edited_time"`
	CreatedBy      *PartialUser               `json:"created_by,omitempty"`
	LastEditedBy   *PartialUser               `json:"last_edited_by,omitempty"`
	Archived       bool                       `json:"archived"`
	InTrash        bool                       `json:"in_trash"`
	Title          []RichText                 `json:"title"`
	Description    []RichText                 `json:"description"`
	Properties     map[string]json.RawMessage `json:"properties"`
	DataSources    []DataSourceRef            `json:"data_sources"`
	Parent         Parent                     `json:"parent"`
	URL            string                     `json:"url"`
}

// Parent identifies the page or database a Page is nested under.
// Type discriminates: "page_id" / "database_id" / "workspace" /
// "block_id".
type Parent struct {
	Type         string `json:"type"`
	PageID       string `json:"page_id,omitempty"`
	DatabaseID   string `json:"database_id,omitempty"`
	DataSourceID string `json:"data_source_id,omitempty"`
	BlockID      string `json:"block_id,omitempty"`
	Workspace    bool   `json:"workspace,omitempty"`
}

// DataSourceRef is the compact entry embedded in a database container.
type DataSourceRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DataSource owns a database schema and its row pages in the 2026 API.
type DataSource struct {
	Object         string                     `json:"object"`
	ID             string                     `json:"id"`
	CreatedTime    time.Time                  `json:"created_time"`
	LastEditedTime time.Time                  `json:"last_edited_time"`
	CreatedBy      *PartialUser               `json:"created_by,omitempty"`
	LastEditedBy   *PartialUser               `json:"last_edited_by,omitempty"`
	InTrash        bool                       `json:"in_trash"`
	Title          []RichText                 `json:"title"`
	Description    []RichText                 `json:"description"`
	Properties     map[string]json.RawMessage `json:"properties"`
	Parent         Parent                     `json:"parent"`
	DatabaseParent Parent                     `json:"database_parent"`
	URL            string                     `json:"url"`
}

// View can point at a data source owned by another database. That indirection
// is how linked databases are represented in the current API.
type View struct {
	Object       string `json:"object"`
	ID           string `json:"id"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	Parent       Parent `json:"parent"`
	DataSourceID string `json:"data_source_id"`
	URL          string `json:"url"`
}

// PartialUser is the abbreviated form Notion ships in created_by /
// last_edited_by fields. The full /users/{id} call returns a User
// with email and friends.
type PartialUser struct {
	Object string `json:"object"` // "user"
	ID     string `json:"id"`
}

// User is the full /users/{id} response. Notion's bots and humans
// share the same wire type but differ in Type ("person" vs "bot")
// and in which sub-object is populated.
type User struct {
	Object    string `json:"object"` // "user"
	ID        string `json:"id"`
	Type      string `json:"type"` // "person" | "bot"
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	Person    *struct {
		Email string `json:"email"`
	} `json:"person,omitempty"`
	Bot *struct {
		Owner *struct {
			Type string `json:"type"`
		} `json:"owner,omitempty"`
	} `json:"bot,omitempty"`
}

// Email returns the user's email if available, else empty.
// Bots never have email; persons may have email withheld depending
// on the integration's scope.
func (u *User) Email() string {
	if u == nil || u.Person == nil {
		return ""
	}
	return u.Person.Email
}

// IsBot reports whether u is a bot user. Used to pick the
// canonical-actor prefix (notion-bot:<id> vs notion:<id>).
func (u *User) IsBot() bool {
	return u != nil && u.Type == "bot"
}

// RichText is one inline-text fragment (one annotation run, one
// equation, one mention). Notion concatenates these into the
// rendered paragraph; the parser flattens them via plainText.
type RichText struct {
	Type        string         `json:"type"` // "text" | "mention" | "equation"
	PlainText   string         `json:"plain_text"`
	HRef        string         `json:"href"`
	Annotations RichAnnotation `json:"annotations"`

	Text     *RichTextInner    `json:"text,omitempty"`
	Equation *RichEquationBody `json:"equation,omitempty"`
	Mention  *RichMentionBody  `json:"mention,omitempty"`
}

type RichAnnotation struct {
	Bold          bool   `json:"bold"`
	Italic        bool   `json:"italic"`
	Strikethrough bool   `json:"strikethrough"`
	Underline     bool   `json:"underline"`
	Code          bool   `json:"code"`
	Color         string `json:"color"`
}

type RichTextInner struct {
	Content string `json:"content"`
	Link    *struct {
		URL string `json:"url"`
	} `json:"link,omitempty"`
}

type RichEquationBody struct {
	Expression string `json:"expression"`
}

type RichMentionBody struct {
	Type string          `json:"type"` // "user" | "page" | "database" | "date" | "link_preview" | …
	Raw  json.RawMessage `json:"-"`    // populated by custom unmarshal if we need it
}

// Block is the loose top-level shape returned by
// /blocks/{id}/children. The per-type body lives in a same-named
// field which we keep as RawMessage for the renderer to interpret.
//
// We capture every field the parser uses (Type, ID, HasChildren,
// payload). Other fields we ignore are still present on the wire
// and unmarshaled into the catch-all RawMessage map for forward
// compat.
type Block struct {
	Object         string    `json:"object"` // "block"
	ID             string    `json:"id"`
	Type           string    `json:"type"`
	HasChildren    bool      `json:"has_children"`
	Archived       bool      `json:"archived"`
	InTrash        bool      `json:"in_trash"`
	CreatedTime    time.Time `json:"created_time"`
	LastEditedTime time.Time `json:"last_edited_time"`

	// Payload is the type-specific body. The key into this map is
	// always equal to Type (e.g. for a paragraph block Payload
	// contains key "paragraph"); we preserve it as RawMessage so
	// the renderer can pick whatever fields it needs without
	// pre-typing every possible shape here.
	Payload map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON populates Block + Payload from the loose Notion
// shape. Any field at top level that's not part of Block proper
// (i.e. all the type-specific bodies) lands in Payload.
func (b *Block) UnmarshalJSON(data []byte) error {
	type alias Block
	var top struct {
		alias
		// Catch-all for the per-type body fields (paragraph,
		// heading_1, etc.). json.Unmarshal can't natively split
		// known + unknown fields, so we decode into a map first
		// and then split.
	}
	// First pass: known fields.
	if err := json.Unmarshal(data, &top.alias); err != nil {
		return err
	}
	*b = Block(top.alias)
	// Second pass: catch-all map.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	// Strip the known top-level keys; whatever's left is the
	// per-type body the renderer wants. Preserving everything
	// else (including unknown future fields) keeps Block forward-
	// compatible.
	known := map[string]struct{}{
		"object":           {},
		"id":               {},
		"type":             {},
		"has_children":     {},
		"archived":         {},
		"created_time":     {},
		"last_edited_time": {},
		"created_by":       {},
		"last_edited_by":   {},
		"parent":           {},
		"in_trash":         {},
	}
	payload := make(map[string]json.RawMessage, len(raw))
	for k, v := range raw {
		if _, ok := known[k]; ok {
			continue
		}
		payload[k] = v
	}
	b.Payload = payload
	return nil
}

// SearchParams configures the POST /search call.
type SearchParams struct {
	// Query, when non-empty, is a substring filter Notion applies
	// to titles. Leave empty for best-effort discovery across accessible
	// objects; this is never treated as an exhaustive corpus listing.
	Query string
	// FilterObject restricts to "page" or "data_source". Production
	// discovery uses data_source; pages are ingested only from roots.
	FilterObject string
	// SortDirection is "ascending" or "descending" of
	// last_edited_time. Use "descending" so the watermark check
	// can short-circuit pagination once results fall below it.
	SortDirection string
	// StartCursor is the pagination cursor returned by the
	// previous page; empty starts at the first page.
	StartCursor string
	// PageSize is 1..100; zero defaults to 100 (Notion's max).
	PageSize int
}

// SearchResult is one page of /search results. Sort is
// last_edited_time DESC by request, so consumers can stop
// paginating when a result's last_edited_time falls below their
// watermark.
type SearchResult struct {
	Object     string            `json:"object"` // "list"
	Results    []json.RawMessage `json:"results"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

// PageMarkdown is the native rendered representation introduced in the
// current API. Truncated or unknown blocks require structured-block fallback.
type PageMarkdown struct {
	Object          string   `json:"object"`
	ID              string   `json:"id"`
	Markdown        string   `json:"markdown"`
	Truncated       bool     `json:"truncated"`
	UnknownBlockIDs []string `json:"unknown_block_ids"`
}

// Search calls POST /search and returns one page. Caller
// re-invokes with NextCursor until HasMore is false (or until
// they've seen everything they care about — incremental sync stops
// when results pass the watermark).
func (c *Client) Search(ctx context.Context, p SearchParams) (*SearchResult, error) {
	body := map[string]any{}
	if p.Query != "" {
		body["query"] = p.Query
	}
	if p.FilterObject != "" {
		body["filter"] = map[string]string{
			"property": "object",
			"value":    p.FilterObject,
		}
	}
	if p.SortDirection != "" {
		body["sort"] = map[string]string{
			"direction": p.SortDirection,
			"timestamp": "last_edited_time",
		}
	}
	if p.StartCursor != "" {
		body["start_cursor"] = p.StartCursor
	}
	if p.PageSize > 0 {
		body["page_size"] = p.PageSize
	}
	var out SearchResult
	if err := c.Do(ctx, http.MethodPost, "search", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPage fetches a single page by ID. 404 if deleted/inaccessible.
func (c *Client) GetPage(ctx context.Context, id string) (*Page, error) {
	var out Page
	path := "pages/" + url.PathEscape(stripDashes(id))
	// Notion accepts both dashed + undashed UUIDs. Dashed is the
	// canonical form we store; the API handles either, but we
	// normalize to undashed for the URL path to match Notion's own
	// convention in their docs.
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDatabase fetches a single database object by ID. Used for
// LRU-cache-warming the database title.
func (c *Client) GetDatabase(ctx context.Context, id string) (*Database, error) {
	var out Database
	path := "databases/" + url.PathEscape(stripDashes(id))
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetDataSource fetches schema metadata for a current-API data source.
func (c *Client) GetDataSource(ctx context.Context, id string) (*DataSource, error) {
	var out DataSource
	path := "data_sources/" + url.PathEscape(stripDashes(id))
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetView resolves a view to its parent database and backing data source.
func (c *Client) GetView(ctx context.Context, id string) (*View, error) {
	var out View
	path := "views/" + url.PathEscape(stripDashes(id))
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ViewsPage is one page of GET /views. List results are partial view objects;
// use GetView when the backing data_source_id is needed.
type ViewsPage struct {
	Object     string `json:"object"`
	Results    []View `json:"results"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// ListViews returns views attached to either a database or a data source.
func (c *Client) ListViews(ctx context.Context, databaseID, dataSourceID, cursor string, pageSize int) (*ViewsPage, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	q := url.Values{}
	q.Set("page_size", fmt.Sprintf("%d", pageSize))
	if databaseID != "" {
		q.Set("database_id", stripDashes(databaseID))
	}
	if dataSourceID != "" {
		q.Set("data_source_id", stripDashes(dataSourceID))
	}
	if cursor != "" {
		q.Set("start_cursor", cursor)
	}
	var out ViewsPage
	if err := c.Do(ctx, http.MethodGet, "views?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAllViewsForDatabase exhausts the stable view listing for a database.
func (c *Client) ListAllViewsForDatabase(ctx context.Context, databaseID string) ([]View, error) {
	var all []View
	cursor := ""
	for {
		page, err := c.ListViews(ctx, databaseID, "", cursor, 100)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if !page.HasMore {
			return all, nil
		}
		if page.NextCursor == "" {
			return nil, errors.New("notion: list views has_more without next_cursor")
		}
		cursor = page.NextCursor
	}
}

// GetPageMarkdown retrieves Notion's native enhanced Markdown rendering.
func (c *Client) GetPageMarkdown(ctx context.Context, id string) (*PageMarkdown, error) {
	var out PageMarkdown
	path := "pages/" + url.PathEscape(stripDashes(id)) + "/markdown"
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetUser fetches a full User object by ID. Used to resolve
// last_edited_by to email or display name.
func (c *Client) GetUser(ctx context.Context, id string) (*User, error) {
	var out User
	path := "users/" + url.PathEscape(stripDashes(id))
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSelf returns the bot identity attached to the current credential.
func (c *Client) GetSelf(ctx context.Context) (*User, error) {
	var out User
	if err := c.Do(ctx, http.MethodGet, "users/me", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BlockChildrenPage is one page of /blocks/{id}/children.
type BlockChildrenPage struct {
	Object     string  `json:"object"`
	Results    []Block `json:"results"`
	HasMore    bool    `json:"has_more"`
	NextCursor string  `json:"next_cursor"`
}

// GetBlockChildren returns one page of children. Caller
// re-invokes with NextCursor until exhausted; ListAllBlockChildren
// is the convenience wrapper that does that.
func (c *Client) GetBlockChildren(ctx context.Context, id, cursor string, pageSize int) (*BlockChildrenPage, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	q := url.Values{}
	q.Set("page_size", fmt.Sprintf("%d", pageSize))
	if cursor != "" {
		q.Set("start_cursor", cursor)
	}
	path := fmt.Sprintf("blocks/%s/children?%s",
		url.PathEscape(stripDashes(id)), q.Encode())
	var out BlockChildrenPage
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAllBlockChildren paginates through every child of a block /
// page, returning them in document order. Bounded externally by
// the caller via ctx; an unbounded blockcount is acceptable for
// the structural tree-walk path (see design doc § Recursion bounds
// — the body-extraction renderer enforces its own 1000-block cap
// at render time).
func (c *Client) ListAllBlockChildren(ctx context.Context, id string) ([]Block, error) {
	var all []Block
	cursor := ""
	for {
		page, err := c.GetBlockChildren(ctx, id, cursor, 100)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if !page.HasMore {
			return all, nil
		}
		if page.NextCursor == "" {
			return nil, errors.New("notion: block children has_more without next_cursor")
		}
		cursor = page.NextCursor
	}
}

// GetBlock retrieves a single block by ID. The response body is
// the same shape as one element of GetBlockChildren.Results — a
// Block envelope plus the type-specific payload — so the caller
// re-uses Block here.
//
// Used by the image-ingest fetcher: image blocks' file URLs are
// signed and expire (~1h), so the worker re-resolves the URL via
// blocks.retrieve every time it goes to fetch bytes. Going through
// this method (rather than caching the URL on event_images) means
// the source-of-truth for image bytes stays in Notion and we never
// chase a rotted signed URL.
func (c *Client) GetBlock(ctx context.Context, id string) (*Block, error) {
	path := fmt.Sprintf("blocks/%s", url.PathEscape(stripDashes(id)))
	var out Block
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// QueryDataSourcePage is one page of /data_sources/{id}/query.
type QueryDataSourcePage struct {
	Object     string            `json:"object"`
	Results    []json.RawMessage `json:"results"` // each is a Page object
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

// QueryDataSource returns one page of data-source rows without an additional
// filter. Database containers themselves are not queryable in the 2026 API.
func (c *Client) QueryDataSource(ctx context.Context, id, cursor string, pageSize int) (*QueryDataSourcePage, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	body := map[string]any{
		"page_size": pageSize,
	}
	if cursor != "" {
		body["start_cursor"] = cursor
	}
	path := fmt.Sprintf("data_sources/%s/query", url.PathEscape(stripDashes(id)))
	var out QueryDataSourcePage
	if err := c.Do(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// stripDashes converts "abcd1234-ef56-..." to "abcd1234ef56..." for
// path interpolation. Notion's API accepts either, but the docs +
// tooling consistently use the undashed form in URLs.
//
// Only kicks in for inputs that look like a UUID (32 hex chars
// with the canonical 8-4-4-4-12 dash split). Anything else passes
// through unchanged — important for tests that use non-UUID ids
// as fixtures, and defensive against a caller passing a free-form
// string we don't recognize.
func stripDashes(s string) string {
	if !uuidShape.MatchString(s) {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			out = append(out, s[i])
		}
	}
	return string(out)
}
