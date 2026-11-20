package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PropertyPreface renders a database row's property bag as a
// one-line preface that gets prepended to the page body before
// chunking, so the embedding pipeline sees the row's typed
// metadata as part of the document.
//
// Output shape (per design doc § Database row pages):
//
//	Database: <db title> · Status: <value> · Owner: <names> · Tags: <a, b> · Created: <date>
//
// The DB title is resolved via DirCache; if missing/inaccessible,
// the "Database: ..." prefix is omitted (just renders the property
// list). People-typed properties are resolved through the user
// cache to surface display names rather than UUIDs.
//
// Properties not in the v1 type coverage list (formula, relation,
// rollup) are rendered as `<unsupported: <type>>` placeholders so
// truncation is visible without extra API calls.
//
// Returns empty string when there are no recognizable properties
// (e.g., a non-database page). Never returns an error — failures
// to resolve sub-references degrade to omitted fields.
func PropertyPreface(ctx context.Context, page *Page, dir *DirCache) string {
	if page == nil {
		return ""
	}
	parts := make([]string, 0, len(page.Properties)+1)

	// Database name — only relevant for rows whose parent is a
	// database. dir is optional; if nil, we skip the lookup
	// entirely (used by tests that don't want network).
	if (page.Parent.Type == "database_id" || page.Parent.Type == "data_source_id") && page.Parent.DatabaseID != "" && dir != nil {
		if title, err := dir.DatabaseTitle(ctx, page.Parent.DatabaseID); err == nil && title != "" {
			parts = append(parts, "Database: "+title)
		}
	}

	// Sort the property keys for deterministic preface output.
	// Unsorted maps would yield a different preface byte-for-byte
	// on every render, which (a) busts the events.Upsert no-op
	// short-circuit on un-edited pages and (b) makes test
	// assertions tedious.
	keys := make([]string, 0, len(page.Properties))
	for k := range page.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, name := range keys {
		raw := page.Properties[name]
		val := renderProperty(ctx, name, raw, dir)
		if val == "" {
			continue
		}
		parts = append(parts, name+": "+val)
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// renderProperty dispatches by the property's "type" discriminator
// and renders it to a human-readable string. Coverage matches the
// design doc's v1 list:
//
//	title, rich_text, select, multi_select, status, people, date,
//	number, checkbox, email, phone_number, url
//
// Other types render as "<unsupported: <type>>" (or omit when
// they're rarely interesting, like formula).
//
// "title" is the page's primary title — usually rendered separately
// by the caller, so we explicitly skip it here to avoid duplicating
// the page title into the preface line.
func renderProperty(ctx context.Context, _ string, raw json.RawMessage, dir *DirCache) string {
	// Decode just enough to discriminate.
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return ""
	}
	switch head.Type {
	case "title":
		// Skip — caller renders the page title separately.
		return ""

	case "rich_text":
		var p struct {
			RichText []RichText `json:"rich_text"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		return strings.TrimSpace(plainText(p.RichText))

	case "select":
		var p struct {
			Select *struct {
				Name string `json:"name"`
			} `json:"select"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		if p.Select == nil {
			return ""
		}
		return p.Select.Name

	case "status":
		var p struct {
			Status *struct {
				Name string `json:"name"`
			} `json:"status"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		if p.Status == nil {
			return ""
		}
		return p.Status.Name

	case "multi_select":
		var p struct {
			MultiSelect []struct {
				Name string `json:"name"`
			} `json:"multi_select"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		names := make([]string, 0, len(p.MultiSelect))
		for _, s := range p.MultiSelect {
			if s.Name != "" {
				names = append(names, s.Name)
			}
		}
		return strings.Join(names, ", ")

	case "people":
		var p struct {
			People []PartialUser `json:"people"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		names := make([]string, 0, len(p.People))
		for _, u := range p.People {
			name := userDisplay(ctx, u.ID, dir)
			if name != "" {
				names = append(names, name)
			}
		}
		return strings.Join(names, ", ")

	case "date":
		var p struct {
			Date *struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"date"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		if p.Date == nil {
			return ""
		}
		start := formatDate(p.Date.Start)
		if p.Date.End == "" {
			return start
		}
		end := formatDate(p.Date.End)
		return start + ".." + end

	case "number":
		var p struct {
			Number *float64 `json:"number"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		if p.Number == nil {
			return ""
		}
		// strconv.FormatFloat(_, 'f', -1, 64) emits the shortest
		// decimal representation that round-trips to the input —
		// crucially, NEVER scientific notation. %g (and %v, which
		// is %g for floats) flip to scientific around 1e6, which
		// would render "$1.2M" as "1.234567e+06" — illegible in
		// the embedding preface. %v is no fix (it is %g for
		// floats); only formatting as a decimal is.
		return strconv.FormatFloat(*p.Number, 'f', -1, 64)

	case "checkbox":
		var p struct {
			Checkbox bool `json:"checkbox"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		if p.Checkbox {
			return "true"
		}
		return "false"

	case "email":
		var p struct {
			Email string `json:"email"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		return p.Email

	case "phone_number":
		var p struct {
			PhoneNumber string `json:"phone_number"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		return p.PhoneNumber

	case "url":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		return p.URL

	case "formula", "rollup", "relation":
		// Defer per design doc § Database row pages — these need
		// extra API calls and rarely carry load-bearing search
		// signal. Skip silently rather than render a placeholder
		// that pollutes every row's preface.
		return ""

	case "created_time":
		var p struct {
			CreatedTime string `json:"created_time"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		return formatDate(p.CreatedTime)

	case "last_edited_time":
		var p struct {
			LastEditedTime string `json:"last_edited_time"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return ""
		}
		return formatDate(p.LastEditedTime)

	case "created_by", "last_edited_by":
		// Already captured at the page level (occurred_at +
		// actor); skip here to avoid duplication.
		return ""

	default:
		return fmt.Sprintf("<unsupported: %s>", head.Type)
	}
}

// userDisplay resolves a user UUID to a display name. Tries the
// directory cache; if the user object is missing/cached-empty,
// falls back to the bare UUID. Bots are rendered with a "[bot]"
// suffix so the preface clearly attributes the action.
func userDisplay(ctx context.Context, id string, dir *DirCache) string {
	if dir == nil || id == "" {
		return id
	}
	u, err := dir.User(ctx, id)
	if err != nil || u == nil {
		return id
	}
	name := u.Name
	if name == "" {
		name = id
	}
	if u.IsBot() {
		return name + " [bot]"
	}
	return name
}

// formatDate normalizes Notion's date strings (RFC-3339 or
// YYYY-MM-DD) into YYYY-MM-DD for stable, embed-friendly output.
// Falls back to the raw string on parse failure.
func formatDate(s string) string {
	if s == "" {
		return ""
	}
	// Notion returns either "2026-04-12" or "2026-04-12T15:00:00.000Z".
	// We normalize to date-only because the embedding doesn't need
	// per-second precision in the preface line.
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return s
}

// PageTitle extracts the page's "title" property as plaintext.
// Same convention as notion-to-md / Onyx: walk properties to find
// the one whose type is "title", join its rich-text fragments.
//
// For database rows, the property is conventionally named "Name"
// or "Title" but the discriminator is the type, not the name —
// so we scan all properties.
func PageTitle(page *Page) string {
	if page == nil {
		return ""
	}
	for _, raw := range page.Properties {
		var head struct {
			Type  string     `json:"type"`
			Title []RichText `json:"title"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			continue
		}
		if head.Type == "title" {
			return strings.TrimSpace(plainText(head.Title))
		}
	}
	return ""
}
