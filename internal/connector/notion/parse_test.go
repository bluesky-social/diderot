package notion

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// blockFromJSON is a test helper that decodes Notion block JSON
// into a Block (exercising the UnmarshalJSON catch-all).
func blockFromJSON(t *testing.T, src string) Block {
	t.Helper()
	var b Block
	if err := json.Unmarshal([]byte(src), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return b
}

// noChildren is the test stand-in for fetchChildren when the input
// has no has_children blocks.
func noChildren(_ context.Context, _ string) ([]Block, error) {
	t := []Block{}
	return t, nil
}

func TestRender_Headings(t *testing.T) {
	blocks := []Block{
		blockFromJSON(t, `{"object":"block","id":"1","type":"heading_1","has_children":false,"heading_1":{"rich_text":[{"plain_text":"Hello"}]}}`),
		blockFromJSON(t, `{"object":"block","id":"2","type":"heading_2","has_children":false,"heading_2":{"rich_text":[{"plain_text":"World"}]}}`),
		blockFromJSON(t, `{"object":"block","id":"3","type":"heading_3","has_children":false,"heading_3":{"rich_text":[{"plain_text":"Sub"}]}}`),
	}
	r := NewRenderer(noChildren, RenderOptions{})
	res, err := r.Render(context.Background(), blocks)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "# Hello\n\n## World\n\n### Sub\n\n"
	if res.Body != want {
		t.Errorf("body=%q\nwant %q", res.Body, want)
	}
	if res.Truncated {
		t.Error("Truncated=true, expected false")
	}
}

func TestRender_NumberedListResetsOnNonNumbered(t *testing.T) {
	blocks := []Block{
		blockFromJSON(t, `{"object":"block","id":"1","type":"numbered_list_item","has_children":false,"numbered_list_item":{"rich_text":[{"plain_text":"first"}]}}`),
		blockFromJSON(t, `{"object":"block","id":"2","type":"numbered_list_item","has_children":false,"numbered_list_item":{"rich_text":[{"plain_text":"second"}]}}`),
		blockFromJSON(t, `{"object":"block","id":"3","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"break"}]}}`),
		blockFromJSON(t, `{"object":"block","id":"4","type":"numbered_list_item","has_children":false,"numbered_list_item":{"rich_text":[{"plain_text":"fresh"}]}}`),
	}
	r := NewRenderer(noChildren, RenderOptions{})
	res, _ := r.Render(context.Background(), blocks)
	// First two: 1. and 2. Then a paragraph resets the counter.
	// Then "fresh" should be 1.
	want := "1. first\n2. second\nbreak\n\n1. fresh\n"
	if res.Body != want {
		t.Errorf("body=%q\nwant %q", res.Body, want)
	}
}

func TestRender_BulletedAndTodo(t *testing.T) {
	blocks := []Block{
		blockFromJSON(t, `{"object":"block","id":"1","type":"bulleted_list_item","has_children":false,"bulleted_list_item":{"rich_text":[{"plain_text":"foo"}]}}`),
		blockFromJSON(t, `{"object":"block","id":"2","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"task A"}],"checked":false}}`),
		blockFromJSON(t, `{"object":"block","id":"3","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"task B"}],"checked":true}}`),
	}
	r := NewRenderer(noChildren, RenderOptions{})
	res, _ := r.Render(context.Background(), blocks)
	want := "- foo\n- [ ] task A\n- [x] task B\n"
	if res.Body != want {
		t.Errorf("body=%q\nwant %q", res.Body, want)
	}
}

func TestRender_QuoteCalloutCodeDivider(t *testing.T) {
	blocks := []Block{
		blockFromJSON(t, `{"object":"block","id":"1","type":"quote","has_children":false,"quote":{"rich_text":[{"plain_text":"important"}]}}`),
		blockFromJSON(t, `{"object":"block","id":"2","type":"callout","has_children":false,"callout":{"rich_text":[{"plain_text":"note"}],"icon":{"type":"emoji","emoji":"💡"}}}`),
		blockFromJSON(t, `{"object":"block","id":"3","type":"code","has_children":false,"code":{"rich_text":[{"plain_text":"echo hi"}],"language":"bash"}}`),
		blockFromJSON(t, `{"object":"block","id":"4","type":"divider","has_children":false,"divider":{}}`),
	}
	r := NewRenderer(noChildren, RenderOptions{})
	res, _ := r.Render(context.Background(), blocks)
	if !strings.Contains(res.Body, "> important") {
		t.Errorf("missing quote: %q", res.Body)
	}
	if !strings.Contains(res.Body, "> 💡 note") {
		t.Errorf("missing callout w/ icon: %q", res.Body)
	}
	if !strings.Contains(res.Body, "```bash\necho hi\n```") {
		t.Errorf("missing code fence: %q", res.Body)
	}
	if !strings.Contains(res.Body, "---") {
		t.Errorf("missing divider: %q", res.Body)
	}
}

func TestRender_RichTextLinksAndCode(t *testing.T) {
	src := `{
		"object":"block","id":"1","type":"paragraph","has_children":false,
		"paragraph":{"rich_text":[
			{"plain_text":"see ","annotations":{"code":false}},
			{"plain_text":"docs","annotations":{"code":false},"text":{"content":"docs","link":{"url":"https://example.com/x"}}},
			{"plain_text":" or run ","annotations":{"code":false}},
			{"plain_text":"diderot","annotations":{"code":true}}
		]}
	}`
	blocks := []Block{blockFromJSON(t, src)}
	r := NewRenderer(noChildren, RenderOptions{})
	res, _ := r.Render(context.Background(), blocks)
	if !strings.Contains(res.Body, "see [docs](https://example.com/x) or run `diderot`") {
		t.Errorf("rich text didn't preserve link/code: %q", res.Body)
	}
}

func TestRender_NestedChildren(t *testing.T) {
	// Top-level bulleted list with one item that has children;
	// child is a paragraph "deep". Verify indent.
	parent := blockFromJSON(t, `{"object":"block","id":"P","type":"bulleted_list_item","has_children":true,"bulleted_list_item":{"rich_text":[{"plain_text":"top"}]}}`)
	child := blockFromJSON(t, `{"object":"block","id":"C","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"deep"}]}}`)
	fetch := func(_ context.Context, parentID string) ([]Block, error) {
		if parentID == "P" {
			return []Block{child}, nil
		}
		return nil, nil
	}
	r := NewRenderer(fetch, RenderOptions{})
	res, _ := r.Render(context.Background(), []Block{parent})
	want := "- top\n  deep\n\n"
	if res.Body != want {
		t.Errorf("body=%q\nwant %q", res.Body, want)
	}
}

func TestRender_Table(t *testing.T) {
	tbl := blockFromJSON(t, `{"object":"block","id":"T","type":"table","has_children":true,"table":{"table_width":2,"has_column_header":true}}`)
	row1 := blockFromJSON(t, `{"object":"block","id":"R1","type":"table_row","has_children":false,"table_row":{"cells":[[{"plain_text":"name"}],[{"plain_text":"value"}]]}}`)
	row2 := blockFromJSON(t, `{"object":"block","id":"R2","type":"table_row","has_children":false,"table_row":{"cells":[[{"plain_text":"foo"}],[{"plain_text":"42"}]]}}`)
	fetch := func(_ context.Context, _ string) ([]Block, error) {
		return []Block{row1, row2}, nil
	}
	r := NewRenderer(fetch, RenderOptions{})
	res, _ := r.Render(context.Background(), []Block{tbl})
	want := "| name | value |\n| --- | --- |\n| foo | 42 |\n\n"
	if res.Body != want {
		t.Errorf("body=%q\nwant %q", res.Body, want)
	}
}

func TestRender_MaxBlocksTruncates(t *testing.T) {
	blocks := make([]Block, 10)
	for i := range blocks {
		blocks[i] = blockFromJSON(t, `{"object":"block","id":"x","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"text"}]}}`)
	}
	r := NewRenderer(noChildren, RenderOptions{MaxBlocks: 3})
	res, _ := r.Render(context.Background(), blocks)
	if !res.Truncated || res.TruncationReason != "blocks" {
		t.Errorf("expected blocks truncation; got %+v", res)
	}
	if !strings.Contains(res.Body, "truncated at 3 blocks") {
		t.Errorf("missing truncation marker: %q", res.Body)
	}
	// Should have rendered exactly 3 paragraph lines before truncating.
	if got := strings.Count(res.Body, "text\n\n"); got != 3 {
		t.Errorf("rendered %d paragraphs, expected 3", got)
	}
}

func TestRender_MaxBytesTruncatesAtRuneBoundary(t *testing.T) {
	// Build a block with a multi-byte rune so we can verify the
	// rune-aware boundary truncation. Each "💡" is 4 bytes in
	// UTF-8.
	body := strings.Repeat("💡", 100) // 400 bytes
	src := `{"object":"block","id":"1","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"` + body + `"}]}}`
	blocks := []Block{blockFromJSON(t, src)}
	r := NewRenderer(noChildren, RenderOptions{MaxBytes: 50})
	res, _ := r.Render(context.Background(), blocks)
	// Output must be valid UTF-8 (no truncated rune mid-byte).
	if strings.ContainsRune(res.Body, '�') {
		t.Errorf("body contains replacement rune; truncation broke UTF-8")
	}
}

// TestRender_MaxBytesOnLastBlockMarksTruncated covers the case
// where MaxBytes is hit mid-rune INSIDE the final block of the
// walk. Without the late-mark in result(), checkLimits would
// never re-run and Truncated would silently stay false.
func TestRender_MaxBytesOnLastBlockMarksTruncated(t *testing.T) {
	emoji := strings.Repeat("💡", 80) // 320 bytes
	src := `{"object":"block","id":"only","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"` + emoji + `"}]}}`
	r := NewRenderer(noChildren, RenderOptions{MaxBytes: 99})
	res, _ := r.Render(context.Background(), []Block{blockFromJSON(t, src)})
	if !res.Truncated {
		t.Errorf("Truncated=false on last-block trim; want true\nbody: %q", res.Body)
	}
	if res.TruncationReason != "bytes" {
		t.Errorf("TruncationReason=%q, want 'bytes'", res.TruncationReason)
	}
	if !strings.Contains(res.Body, "truncated at 99 bytes") {
		t.Errorf("missing marker on last-block trim: %q", res.Body)
	}
}

// TestRender_MaxBytesTrimDoesNotLeakSubsequentBlocks: when appendRaw trims at a rune boundary leaving
// cut < remaining, bytesUsed must be pinned to MaxBytes (not
// += cut). Otherwise the next block sees bytesUsed < MaxBytes,
// checkLimits doesn't trip, and content past the truncation
// boundary leaks in — silent corruption that mixes early-block
// content with late-block content.
func TestRender_MaxBytesTrimDoesNotLeakSubsequentBlocks(t *testing.T) {
	// Block 1: a paragraph whose multi-byte body forces a
	// rune-boundary trim. Block 2: a UNIQUE recognizable string
	// that we must NOT see in the output.
	emoji := strings.Repeat("💡", 50) // 200 bytes
	src1 := `{"object":"block","id":"1","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"` + emoji + `"}]}}`
	src2 := `{"object":"block","id":"2","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"LATER_CONTENT_SENTINEL"}]}}`
	blocks := []Block{blockFromJSON(t, src1), blockFromJSON(t, src2)}
	// MaxBytes=99 forces a rune-boundary trim inside the emoji
	// run (each emoji is 4 bytes, so 99 bytes lands mid-rune).
	r := NewRenderer(noChildren, RenderOptions{MaxBytes: 99})
	res, _ := r.Render(context.Background(), blocks)
	if strings.Contains(res.Body, "LATER_CONTENT_SENTINEL") {
		t.Fatalf("post-truncation block leaked into output: %q", res.Body)
	}
	if !res.Truncated {
		t.Errorf("Truncated=false")
	}
}

// TestRender_MaxBytesTruncationMarkerVisible: when MaxBytes is hit, the truncation marker MUST be
// emitted, even though the byte budget is already exhausted.
// Otherwise consumers have no signal in the rendered body that
// truncation happened.
func TestRender_MaxBytesTruncationMarkerVisible(t *testing.T) {
	// Two blocks that overflow the budget — first fills it, second
	// triggers checkLimits.
	src := `{"object":"block","id":"1","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}]}}`
	blocks := []Block{
		blockFromJSON(t, src),
		blockFromJSON(t, src),
		blockFromJSON(t, src),
	}
	r := NewRenderer(noChildren, RenderOptions{MaxBytes: 60})
	res, _ := r.Render(context.Background(), blocks)
	if !res.Truncated || res.TruncationReason != "bytes" {
		t.Fatalf("expected bytes truncation; got %+v", res)
	}
	if !strings.Contains(res.Body, "truncated at 60 bytes") {
		t.Errorf("missing bytes truncation marker in body: %q", res.Body)
	}
}

func TestRender_ChildPageNotRecursed(t *testing.T) {
	// A child_page block with has_children=true should NOT recurse;
	// its title alone is rendered, and the child gets its own
	// event row elsewhere.
	parent := blockFromJSON(t, `{"object":"block","id":"P","type":"child_page","has_children":true,"child_page":{"title":"Subpage"}}`)
	fetchCalled := false
	fetch := func(_ context.Context, _ string) ([]Block, error) {
		fetchCalled = true
		return nil, nil
	}
	r := NewRenderer(fetch, RenderOptions{})
	res, _ := r.Render(context.Background(), []Block{parent})
	if !strings.Contains(res.Body, "**Subpage**") {
		t.Errorf("missing child_page title: %q", res.Body)
	}
	if fetchCalled {
		t.Errorf("fetch was called for child_page; should not recurse")
	}
}

func TestRender_ContextCancelStops(t *testing.T) {
	blocks := make([]Block, 100)
	for i := range blocks {
		blocks[i] = blockFromJSON(t, `{"object":"block","id":"x","type":"paragraph","has_children":false,"paragraph":{"rich_text":[{"plain_text":"x"}]}}`)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Render
	r := NewRenderer(noChildren, RenderOptions{})
	res, err := r.Render(ctx, blocks)
	if err == nil {
		t.Error("expected ctx error, got nil")
	}
	if res.BlocksConsumed > 1 {
		t.Errorf("rendered %d blocks before honoring ctx; want 0 or 1", res.BlocksConsumed)
	}
}

func TestPlainText_AnnotationsAndLinks(t *testing.T) {
	rt := []RichText{
		{PlainText: "hello "},
		{PlainText: "world", HRef: "https://x.example/"},
		{PlainText: " of ", Annotations: RichAnnotation{Code: true}},
		{PlainText: "code"},
	}
	got := plainText(rt)
	// "world" gets [text](href); " of " gets backticks.
	want := "hello [world](https://x.example/)` of `code"
	if got != want {
		t.Errorf("plainText=%q\nwant %q", got, want)
	}
}

func TestPlainText_Empty(t *testing.T) {
	if got := plainText(nil); got != "" {
		t.Errorf("plainText(nil)=%q, want empty", got)
	}
}
