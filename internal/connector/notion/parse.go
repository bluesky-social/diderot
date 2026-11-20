package notion

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MaxBodyBlocks is the number of blocks the body renderer walks
// before truncating. Beyond this, the markdown body grows tail-
// truncated and the title gets a "(partial — N blocks)" marker.
//
// 1000 is enough to cover the bulk of engineering docs (every
// paragraph is one block; a long RFC is in the 200-400 block
// range). Past 1000 we accept tail truncation and rely on
// `expand` (v2) to recover specific tail content the model asks
// for.
//
// IMPORTANT: this cap applies ONLY to the body renderer. The
// reconcile tree-walker (walker.go) walks every block to find
// nested child_page / child_database references regardless of
// position — see design doc § Recursion bounds for why those
// are intentionally different bounds.
const MaxBodyBlocks = 1000

// MaxBodyBytes is the rune-aware cap on the rendered body. 256 KiB
// matches the list-tool truncation pattern. Pages whose markdown
// bodies exceed this get truncated mid-rendering with a marker.
const MaxBodyBytes = 256 << 10

// RenderOptions tunes the body renderer.
type RenderOptions struct {
	// MaxBlocks overrides MaxBodyBlocks. Zero → MaxBodyBlocks.
	MaxBlocks int
	// MaxBytes overrides MaxBodyBytes. Zero → MaxBodyBytes.
	MaxBytes int
}

// RenderResult bundles the body markdown with metadata about how
// the rendering went — load-bearing for the title-marker logic
// (truncated bodies should be flagged so users know the embed
// covers only part of the page).
type RenderResult struct {
	Body string
	// BlocksConsumed counts the blocks the renderer actually
	// touched (recursive descents counted). Useful for telemetry.
	BlocksConsumed int
	// Truncated is true if either MaxBlocks or MaxBytes was hit
	// during rendering.
	Truncated bool
	// TruncationReason is "blocks" or "bytes" or empty.
	TruncationReason string
}

// childBlocksFn fetches a block's children. Pulled out as a
// callable so tests can inject canned responses without spinning
// an httptest server. Production wires Client.ListAllBlockChildren.
type childBlocksFn func(ctx context.Context, parentID string) ([]Block, error)

// Renderer walks a block tree in document order and emits a flat
// markdown body. Stateful (per-render) because numbered lists
// require a per-depth counter that resets when a non-numbered
// block appears.
type Renderer struct {
	fetchChildren childBlocksFn
	opts          RenderOptions

	// runtime state — reset per Render call.
	out         strings.Builder
	bytesUsed   int
	blocksDone  int
	truncated   bool
	truncReason string
}

// NewRenderer builds a Renderer. fetchChildren is required for
// any block tree containing has_children=true entries; pass a
// noop if you know the input is flat (tests).
func NewRenderer(fetchChildren childBlocksFn, opts RenderOptions) *Renderer {
	if opts.MaxBlocks <= 0 {
		opts.MaxBlocks = MaxBodyBlocks
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = MaxBodyBytes
	}
	return &Renderer{
		fetchChildren: fetchChildren,
		opts:          opts,
	}
}

// Render walks blocks in order and returns the rendered markdown
// body. ctx is honored — a cancelled ctx aborts mid-walk with the
// partial body in result.
func (r *Renderer) Render(ctx context.Context, blocks []Block) (RenderResult, error) {
	r.out.Reset()
	r.bytesUsed = 0
	r.blocksDone = 0
	r.truncated = false
	r.truncReason = ""

	if err := r.walk(ctx, blocks, 0); err != nil {
		// ctx cancellation propagates; the caller may still
		// inspect the partial body if useful.
		return r.result(), err
	}
	return r.result(), nil
}

func (r *Renderer) result() RenderResult {
	// Late-mark for the trim-on-last-block case: if appendRaw
	// trimmed at a rune boundary INSIDE the final block of the
	// walk, checkLimits never gets a chance to run again to
	// flag truncation. Catch that here so the consumer always
	// sees Truncated=true when the body was actually truncated.
	// A test that triggers the cap mid-walk does not cover this
	// case; one that trims inside the last block does.
	if r.bytesUsed >= r.opts.MaxBytes && !r.truncated {
		r.truncated = true
		r.truncReason = "bytes"
		fmt.Fprintf(&r.out, "\n\n[...content truncated at %d bytes...]\n",
			r.opts.MaxBytes)
	}
	return RenderResult{
		Body:             r.out.String(),
		BlocksConsumed:   r.blocksDone,
		Truncated:        r.truncated,
		TruncationReason: r.truncReason,
	}
}

// walk renders a slice of sibling blocks at the given indent depth.
// Counter for numbered_list_item is local to this call and resets
// on any non-numbered block, mirroring notion-to-md's
// modifyNumberedListObject. Each nested level gets its own counter
// (next walk frame).
func (r *Renderer) walk(ctx context.Context, blocks []Block, depth int) error {
	numberedCounter := 0
	for i := range blocks {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if blocks[i].Archived || blocks[i].InTrash {
			continue
		}
		if r.checkLimits() {
			return nil
		}
		b := &blocks[i]
		r.blocksDone++
		// Numbered-list ordering: increment when current block is
		// numbered_list_item, reset to 0 otherwise. The count
		// passed to renderBlock is 1-based: 1, 2, 3, ...
		if b.Type == "numbered_list_item" {
			numberedCounter++
		} else {
			numberedCounter = 0
		}
		if err := r.renderBlock(ctx, b, depth, numberedCounter); err != nil {
			return err
		}
	}
	return nil
}

// checkLimits returns true if either limit has been reached. Sets
// the truncation flags as a side-effect.
//
// Note: the truncation marker is written via WriteString directly,
// bypassing the MaxBytes-enforcing appendRaw — otherwise, when the
// byte cap is the trigger, appendRaw would refuse the write
// (remaining <= 0) and the marker would silently disappear, leaving
// the consumer no signal in the rendered body that truncation
// happened. The marker itself is small (~50 bytes), so the slight
// overflow past MaxBytes is acceptable in exchange for the
// guarantee that truncation is visible.
func (r *Renderer) checkLimits() bool {
	if r.blocksDone >= r.opts.MaxBlocks {
		if !r.truncated {
			r.truncated = true
			r.truncReason = "blocks"
			fmt.Fprintf(&r.out, "\n\n[...content truncated at %d blocks...]\n",
				r.opts.MaxBlocks)
		}
		return true
	}
	if r.bytesUsed >= r.opts.MaxBytes {
		if !r.truncated {
			r.truncated = true
			r.truncReason = "bytes"
			fmt.Fprintf(&r.out, "\n\n[...content truncated at %d bytes...]\n",
				r.opts.MaxBytes)
		}
		return true
	}
	return false
}

// appendRaw writes s to the output with byte-cap enforcement. If
// the addition would exceed MaxBytes, we stop at the rune boundary
// nearest the cap so the output stays valid UTF-8. Returns false
// if the cap was hit (so callers can stop rendering further).
func (r *Renderer) appendRaw(s string) bool {
	if s == "" {
		return true
	}
	remaining := r.opts.MaxBytes - r.bytesUsed
	if remaining <= 0 {
		return false
	}
	if len(s) <= remaining {
		r.out.WriteString(s)
		r.bytesUsed += len(s)
		return true
	}
	// Truncate at the nearest rune boundary <= remaining. We
	// pin bytesUsed to MaxBytes (rather than += cut) so the
	// next checkLimits call trips on the cap. Otherwise, cut
	// being strictly less than remaining (rune-boundary trim)
	// would leave bytesUsed < MaxBytes and subsequent blocks
	// would be appended past the truncation point — silent
	// data shuffling, the worst kind of bug.
	cut := remaining
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	r.out.WriteString(s[:cut])
	r.bytesUsed = r.opts.MaxBytes
	return false
}

// renderBlock dispatches by block type, emits the markdown line(s)
// for the block, and recurses into has_children. depth controls
// indent; numberedIdx is the 1-based ordinal for
// numbered_list_item (zero for everything else).
func (r *Renderer) renderBlock(ctx context.Context, b *Block, depth, numberedIdx int) error {
	indent := strings.Repeat("  ", depth)
	prefix := indent
	switch b.Type {
	case "paragraph":
		text := r.payloadRichText(b, "paragraph")
		if text != "" {
			r.appendRaw(prefix + text + "\n\n")
		}

	case "heading_1":
		text := r.payloadRichText(b, "heading_1")
		if text != "" {
			r.appendRaw(prefix + "# " + text + "\n\n")
		}

	case "heading_2":
		text := r.payloadRichText(b, "heading_2")
		if text != "" {
			r.appendRaw(prefix + "## " + text + "\n\n")
		}

	case "heading_3":
		text := r.payloadRichText(b, "heading_3")
		if text != "" {
			r.appendRaw(prefix + "### " + text + "\n\n")
		}

	case "bulleted_list_item":
		text := r.payloadRichText(b, "bulleted_list_item")
		r.appendRaw(prefix + "- " + text + "\n")

	case "numbered_list_item":
		text := r.payloadRichText(b, "numbered_list_item")
		r.appendRaw(fmt.Sprintf("%s%d. %s\n", prefix, numberedIdx, text))

	case "to_do":
		text, checked := r.payloadTodo(b)
		mark := "[ ]"
		if checked {
			mark = "[x]"
		}
		r.appendRaw(fmt.Sprintf("%s- %s %s\n", prefix, mark, text))

	case "toggle":
		text := r.payloadRichText(b, "toggle")
		if text != "" {
			r.appendRaw(prefix + text + "\n")
		}

	case "quote":
		text := r.payloadRichText(b, "quote")
		if text != "" {
			r.appendRaw(prefix + "> " + text + "\n\n")
		}

	case "callout":
		text := r.payloadRichText(b, "callout")
		icon := r.payloadCalloutIcon(b)
		head := strings.TrimSpace(icon + " " + text)
		if head != "" {
			r.appendRaw(prefix + "> " + head + "\n\n")
		}

	case "code":
		code, lang := r.payloadCode(b)
		// Use indent prefix on each line of the code block.
		r.appendRaw(prefix + "```" + lang + "\n")
		// Code body is multi-line; indent each line.
		for _, line := range strings.Split(code, "\n") {
			r.appendRaw(prefix + line + "\n")
		}
		r.appendRaw(prefix + "```\n\n")

	case "divider":
		r.appendRaw(prefix + "---\n\n")

	case "image", "file", "video", "pdf", "audio":
		caption, link := r.payloadFileLike(b, b.Type)
		if link != "" {
			alt := caption
			if alt == "" {
				alt = b.Type
			}
			r.appendRaw(fmt.Sprintf("%s![%s](%s)\n\n", prefix, alt, link))
		}

	case "bookmark", "embed", "link_preview":
		_, link := r.payloadFileLike(b, b.Type)
		if link != "" {
			r.appendRaw(fmt.Sprintf("%s[%s](%s)\n\n", prefix, link, link))
		}

	case "child_page":
		title := r.payloadChildPageTitle(b)
		if title == "" {
			title = "(untitled child page)"
		}
		// Inline as a heading-bold so the embedding sees the title;
		// the child page will get its own event row, no need to
		// recurse into it here.
		r.appendRaw(prefix + "**" + title + "**\n\n")
		// Skip recursion intentionally — child_page is an event row
		// of its own.
		return nil

	case "child_database":
		title := r.payloadChildDatabaseTitle(b)
		if title == "" {
			title = "(untitled child database)"
		}
		r.appendRaw(prefix + "**" + title + "**\n\n")
		// Same — children of child_database are separate event
		// rows.
		return nil

	case "table":
		// Tables: each child is a table_row block. We fetch the
		// children inline (the renderer needs the row content to
		// render the markdown table). If has_children is false,
		// nothing to render.
		if !b.HasChildren || r.fetchChildren == nil {
			return nil
		}
		rows, err := r.fetchChildren(ctx, b.ID)
		if err != nil {
			return fmt.Errorf("fetch table children (block=%s): %w", b.ID, err)
		}
		r.renderTable(prefix, rows)
		// Table children handled inline; don't recurse generically
		// below.
		return nil

	case "equation", "breadcrumb", "table_of_contents", "synced_block",
		"column_list", "column", "unsupported":
		// Silently omit. Equations are rare in workspace prose;
		// breadcrumb / table_of_contents are nav noise; synced_blocks /
		// columns are layout artifacts whose interesting content is
		// already represented by their children — recurse.
		// Fall through to recurse_children below for layout-only
		// containers.
		if b.Type == "synced_block" || b.Type == "column_list" || b.Type == "column" {
			break
		}
		return nil

	default:
		// Unknown block type — render its rich_text if any so we
		// don't drop content silently. Forward-compat for new
		// block types Notion may introduce.
		text := r.payloadRichText(b, b.Type)
		if text != "" {
			r.appendRaw(prefix + text + "\n\n")
		}
	}

	// Recurse into children (skipped above for child_page,
	// child_database, table — see returns).
	if b.HasChildren && r.fetchChildren != nil {
		children, err := r.fetchChildren(ctx, b.ID)
		if err != nil {
			return fmt.Errorf("fetch children (block=%s, type=%s): %w",
				b.ID, b.Type, err)
		}
		if err := r.walk(ctx, children, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// renderTable emits a markdown table from a slice of table_row
// blocks. Header detection is skipped (the design doesn't require
// it); the first row is treated as the header for markdown
// validity. If there are zero rows, emits nothing.
func (r *Renderer) renderTable(prefix string, rows []Block) {
	if len(rows) == 0 {
		return
	}
	// Each table_row block has a "table_row" payload with a
	// "cells" field that's an array of arrays of rich-text.
	cellsPerRow := make([][]string, 0, len(rows))
	maxCols := 0
	for i := range rows {
		if rows[i].Type != "table_row" {
			continue
		}
		raw, ok := rows[i].Payload["table_row"]
		if !ok {
			continue
		}
		var body struct {
			Cells [][]RichText `json:"cells"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			continue
		}
		row := make([]string, len(body.Cells))
		for j, cell := range body.Cells {
			row[j] = strings.ReplaceAll(plainText(cell), "|", `\|`)
		}
		cellsPerRow = append(cellsPerRow, row)
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}
	if len(cellsPerRow) == 0 || maxCols == 0 {
		return
	}
	// Header row.
	r.appendRaw(prefix + "| " + strings.Join(padCols(cellsPerRow[0], maxCols), " | ") + " |\n")
	// Separator.
	sep := make([]string, maxCols)
	for i := range sep {
		sep[i] = "---"
	}
	r.appendRaw(prefix + "| " + strings.Join(sep, " | ") + " |\n")
	// Body rows.
	for _, row := range cellsPerRow[1:] {
		r.appendRaw(prefix + "| " + strings.Join(padCols(row, maxCols), " | ") + " |\n")
	}
	r.appendRaw("\n")
}

// padCols pads row to width with empty cells. Markdown tables
// require uniform column counts; Notion lets rows have ragged
// widths.
func padCols(row []string, width int) []string {
	if len(row) >= width {
		return row
	}
	out := make([]string, width)
	copy(out, row)
	return out
}

// payloadRichText extracts the "rich_text" array from b's payload
// under key, flattens annotations, and returns the result.
// Returns empty string when the field is missing or empty (most
// non-text blocks).
func (r *Renderer) payloadRichText(b *Block, key string) string {
	raw, ok := b.Payload[key]
	if !ok {
		return ""
	}
	var body struct {
		RichText []RichText `json:"rich_text"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return plainText(body.RichText)
}

// payloadTodo extracts the to_do block's text + checked state.
func (r *Renderer) payloadTodo(b *Block) (string, bool) {
	raw, ok := b.Payload["to_do"]
	if !ok {
		return "", false
	}
	var body struct {
		RichText []RichText `json:"rich_text"`
		Checked  bool       `json:"checked"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", false
	}
	return plainText(body.RichText), body.Checked
}

// payloadCode extracts the code block's text + language.
func (r *Renderer) payloadCode(b *Block) (string, string) {
	raw, ok := b.Payload["code"]
	if !ok {
		return "", ""
	}
	var body struct {
		RichText []RichText `json:"rich_text"`
		Language string     `json:"language"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", ""
	}
	return plainText(body.RichText), body.Language
}

// payloadCalloutIcon returns the callout's icon character if it's
// an emoji, else empty string. Notion icon types are
// "emoji" | "external" | "file"; we only meaningfully render emoji.
func (r *Renderer) payloadCalloutIcon(b *Block) string {
	raw, ok := b.Payload["callout"]
	if !ok {
		return ""
	}
	var body struct {
		Icon *struct {
			Type  string `json:"type"`
			Emoji string `json:"emoji"`
		} `json:"icon"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	if body.Icon != nil && body.Icon.Type == "emoji" {
		return body.Icon.Emoji
	}
	return ""
}

// payloadFileLike extracts the URL from a file/image/embed/bookmark
// block. caption is the rich-text caption flattened to plaintext;
// link is the URL (file.url or external.url).
func (r *Renderer) payloadFileLike(b *Block, key string) (string, string) {
	raw, ok := b.Payload[key]
	if !ok {
		return "", ""
	}
	var body struct {
		Caption []RichText `json:"caption"`
		Type    string     `json:"type"`
		File    *struct {
			URL string `json:"url"`
		} `json:"file,omitempty"`
		External *struct {
			URL string `json:"url"`
		} `json:"external,omitempty"`
		// bookmark / embed / link_preview have a top-level url.
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", ""
	}
	caption := plainText(body.Caption)
	switch {
	case body.URL != "":
		return caption, body.URL
	case body.File != nil && body.File.URL != "":
		return caption, body.File.URL
	case body.External != nil && body.External.URL != "":
		return caption, body.External.URL
	}
	return caption, ""
}

// payloadChildPageTitle extracts a child_page block's title.
func (r *Renderer) payloadChildPageTitle(b *Block) string {
	raw, ok := b.Payload["child_page"]
	if !ok {
		return ""
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return body.Title
}

// payloadChildDatabaseTitle extracts a child_database block's title.
func (r *Renderer) payloadChildDatabaseTitle(b *Block) string {
	raw, ok := b.Payload["child_database"]
	if !ok {
		return ""
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return body.Title
}

// plainText concatenates rich-text fragments into plaintext,
// preserving link annotations as `[text](url)` so the embedding
// pipeline sees the link target. Inline code annotation gets
// backticks. Bold / italic / strikethrough are dropped — they
// don't carry semantic value for retrieval.
func plainText(rt []RichText) string {
	if len(rt) == 0 {
		return ""
	}
	var sb strings.Builder
	for i := range rt {
		t := &rt[i]
		s := t.PlainText
		if t.Annotations.Code {
			s = "`" + s + "`"
		}
		// Prefer the inner Text.Link.URL over t.HRef when both
		// present; inner is the explicit user-visible link, HRef
		// is the resolved target which can include redirect noise.
		var link string
		if t.Text != nil && t.Text.Link != nil {
			link = t.Text.Link.URL
		} else if t.HRef != "" {
			link = t.HRef
		}
		if link != "" {
			s = "[" + s + "](" + link + ")"
		}
		sb.WriteString(s)
	}
	return sb.String()
}
