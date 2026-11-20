package ask

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/retrieve"
)

// fakeAttachmentReader stands in for the event store and, like it, returns an
// attachment only when its event's scope is among the caller's.
type fakeAttachmentReader struct {
	eventScopes map[int64]string
	attachments map[string]events.Attachment // by AttachmentRef.String()
	blobs       map[string][]byte            // by storage key
	reads       int
	byteReads   int
}

func (f *fakeAttachmentReader) Attachment(_ context.Context, ref events.AttachmentRef, scopes []string) (events.Attachment, error) {
	f.reads++
	a, ok := f.attachments[ref.String()]
	if !ok || !events.ScopeAllowed(f.eventScopes[ref.EventID], scopes) {
		return events.Attachment{}, events.ErrAttachmentNotFound
	}
	return a, nil
}

func (f *fakeAttachmentReader) Bytes(_ context.Context, key string) ([]byte, error) {
	f.byteReads++
	data, ok := f.blobs[key]
	if !ok {
		return nil, fmt.Errorf("no object at %q", key)
	}
	return data, nil
}

func fakeAttachment(eventID int64, kind, fileID string, edit func(*events.Attachment)) events.Attachment {
	a := events.Attachment{AttachmentRef: events.AttachmentRef{EventID: eventID, Kind: kind, FileID: fileID}}
	edit(&a)
	return a
}

func callMCPTool(t *testing.T, server *httptest.Server, user, name string, arguments map[string]any) map[string]any {
	t.Helper()
	return resultObject(t, callRPC(t, server, user, "tools/call", name, map[string]any{
		"name": name, "arguments": arguments,
	}, true))
}

func toolErrorText(t *testing.T, result map[string]any) string {
	t.Helper()
	require.Equal(t, true, result["isError"], "want a tool error, got %v", result)
	return result["content"].([]any)[0].(map[string]any)["text"].(string)
}

// TestCorpusMCPListReadsAChannel: list hands its channels filter to the
// retriever with the caller's scopes, marks replies, and reports a channel
// the caller cannot read as the retriever's error, never as an empty list.
func TestCorpusMCPListReadsAChannel(t *testing.T) {
	retriever := &fakeCorpusRetriever{result: &retrieve.Result{NextCursor: "next", Hits: []retrieve.Hit{
		{EventID: 2, Source: "slack", ExternalID: "C1:200.1", ThreadID: "C1:100.1", ChannelName: "incidents", Content: "a reply"},
		{EventID: 1, Source: "slack", ExternalID: "C1:100.1", ThreadID: "C1:100.1", ChannelName: "incidents", Content: "the root"},
		{EventID: 3, Source: "slack", ExternalID: "C1:300.1", ChannelName: "incidents", Content: "standalone"},
	}}}
	server := newCorpusMCPHTTPServer(t, retriever, newFakeCorpusAskRunner())

	result := callMCPTool(t, server, "alex@example.com", "list", map[string]any{
		"channels": []string{"#incidents"}, "occurred_after": "2026-05-01T00:00:00Z", "occurred_before": "2026-05-02T00:00:00Z",
	})
	require.NotEqual(t, true, result["isError"])
	require.Equal(t, []string{"#incidents"}, retriever.listReq.Channels)
	require.Equal(t, []string{"public", "internal", "slack-channel:C1"}, retriever.listReq.AllowedScopes)
	require.Equal(t, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), retriever.listReq.OccurredAfter)

	structured := result["structuredContent"].(map[string]any)
	require.Equal(t, "next", structured["next_cursor"])
	hits := structured["hits"].([]any)
	reply, root, standalone := hits[0].(map[string]any), hits[1].(map[string]any), hits[2].(map[string]any)
	require.Equal(t, true, reply["is_reply"])
	require.Equal(t, "C1:100.1", reply["thread_id"])
	require.NotContains(t, root, "is_reply")
	require.Equal(t, "C1:100.1", root["thread_id"])
	require.NotContains(t, standalone, "is_reply")
	require.NotContains(t, standalone, "thread_id")

	retriever.listErr = fmt.Errorf("%w: %q is not a Slack channel you can read here", retrieve.ErrListChannelNotFound, "legal")
	denied := callMCPTool(t, server, "alex@example.com", "list", map[string]any{"channels": []string{"legal"}})
	require.Contains(t, toolErrorText(t, denied), `"legal" is not a Slack channel you can read here`)
}

func TestListToolSchemaAndDescriptionTeachChannelReads(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(listSchema(testSources), &schema))
	require.Contains(t, schema.Properties, "channels")
	for _, want := range []string{"To read a Slack or Discord channel", "`channels`", "`next_cursor`", "`thread_roots_only`", "`is_reply: true`"} {
		require.Contains(t, listToolDescription, want)
	}
}

// TestCorpusMCPExpandListsAttachments: expand describes each file of the
// content it returns and gives the ones Diderot holds an id for the
// attachment tool. Ask's own expand has no such tool and gets no ids.
func TestCorpusMCPExpandListsAttachments(t *testing.T) {
	longCaption := strings.Repeat("c", maxListedCaptionRunes+50)
	attachments := []events.Attachment{
		fakeAttachment(7, events.AttachmentImage, "F1", func(a *events.Attachment) {
			a.Name, a.MediaType, a.Bytes, a.Caption = "graph.png", "image/png", 2048, "a latency graph"
		}),
		fakeAttachment(8, events.AttachmentDocument, "F2", func(a *events.Attachment) {
			a.Name, a.MediaType, a.Bytes = "contract.pdf", "application/pdf", 90000
		}),
		fakeAttachment(8, "", "F3", func(a *events.Attachment) { a.Name, a.MediaType, a.Caption = "notes.txt", "text/plain", longCaption }),
	}
	expander := &fakeExpander{results: map[int64]ask_fakeResult{
		7: {res: ExpandResult{Source: "slack", Content: "the thread", Attachments: attachments}},
	}}
	scopes := &fakeScopeLookup{scopes: map[int64]string{7: "slack-channel:C1"}}
	server := newCorpusMCPHTTPServerWithExpander(t, &fakeCorpusRetriever{}, expander, scopes,
		func() CorpusAskLease { return CorpusAskLease{} }, newMemoryCorpusTaskBackend())

	structured := callMCPTool(t, server, "alex@example.com", "expand", map[string]any{"event_id": 7})["structuredContent"].(map[string]any)
	listed := structured["attachments"].([]any)
	require.Len(t, listed, 3)
	require.Equal(t, map[string]any{
		"id": "image:7:F1", "event_id": float64(7), "name": "graph.png", "type": "image/png",
		"size": float64(2048), "caption": "a latency graph",
	}, listed[0])
	require.Equal(t, map[string]any{
		"id": "document:8:F2", "event_id": float64(8), "name": "contract.pdf", "type": "application/pdf", "size": float64(90000),
	}, listed[1])
	unheld := listed[2].(map[string]any)
	require.NotContains(t, unheld, "id", "a file Diderot does not hold has no id to fetch")
	require.Len(t, []rune(unheld["caption"].(string)), maxListedCaptionRunes+1)

	native, err := invokeNativeExpand(t, NewEinoExpandTool(expander, scopes, testSources), []string{"slack-channel:C1"}, `{"event_id":7}`)
	require.NoError(t, err)
	require.Len(t, native.Attachments, 3)
	for _, a := range native.Attachments {
		require.Empty(t, a.ID)
	}
	require.Equal(t, "a latency graph", native.Attachments[0].Caption)
}

// TestListedAttachmentsListsEveryFileAndBoundsCaptions: however many files a
// thread carries, each is listed with its id, so each can be fetched; only
// the captions are bounded.
func TestListedAttachmentsListsEveryFileAndBoundsCaptions(t *testing.T) {
	caption := strings.Repeat("c", maxListedCaptionRunes)
	withCaptions := listedCaptionBudgetRunes / maxListedCaptionRunes
	many := make([]events.Attachment, withCaptions+200)
	for i := range many {
		many[i] = fakeAttachment(int64(i+1), events.AttachmentImage, "F", func(a *events.Attachment) { a.Caption = caption })
	}
	listed := listedAttachments(many)
	require.Len(t, listed, len(many))
	for i, info := range listed {
		require.Equal(t, many[i].String(), info.ID)
		if i < withCaptions {
			require.Equal(t, caption, info.Caption, i)
		} else {
			require.Empty(t, info.Caption, i)
		}
	}
	require.Nil(t, listedAttachments(nil))
}

func newAttachmentServer(t *testing.T, reader *fakeAttachmentReader) *httptest.Server {
	t.Helper()
	// The scope lookup is the tool's first gate; the reader's own check is
	// the store's second.
	return newCorpusMCPHTTPServerWithAttachments(t, &fakeCorpusRetriever{}, &fakeExpander{},
		&fakeScopeLookup{scopes: reader.eventScopes}, reader, &fakeExactReferenceResolver{},
		func() CorpusAskLease { return CorpusAskLease{} }, newMemoryCorpusTaskBackend())
}

// TestCorpusMCPAttachmentReturnsImagesAndDocumentText covers what the tool
// returns for each state of an attachment.
func TestCorpusMCPAttachmentReturnsImagesAndDocumentText(t *testing.T) {
	png := []byte("\x89PNG not really")
	longText := strings.Repeat("0123456789", ExpandContentBudget/10+1000)
	reader := &fakeAttachmentReader{
		eventScopes: map[int64]string{7: "slack-channel:C1"},
		blobs:       map[string][]byte{"k/small": png, "k/grown": make([]byte, maxAttachmentImageBytes+1)},
		attachments: map[string]events.Attachment{},
	}
	add := func(kind, fileID string, edit func(*events.Attachment)) {
		a := fakeAttachment(7, kind, fileID, edit)
		reader.attachments[a.String()] = a
	}
	add(events.AttachmentImage, "small", func(a *events.Attachment) {
		a.MediaType, a.Bytes, a.StorageKey, a.Caption = "image/png", int64(len(png)), "k/small", "a latency graph"
	})
	add(events.AttachmentImage, "huge", func(a *events.Attachment) {
		a.MediaType, a.Bytes, a.StorageKey, a.Caption = "image/png", maxAttachmentImageBytes+1, "k/huge", "a huge poster"
	})
	// The recorded size is stale; the stored object is over the limit.
	add(events.AttachmentImage, "grown", func(a *events.Attachment) {
		a.MediaType, a.Bytes, a.StorageKey = "image/png", 10, "k/grown"
	})
	add(events.AttachmentImage, "pending", func(a *events.Attachment) { a.MediaType = "image/png" })
	add(events.AttachmentDocument, "doc", func(a *events.Attachment) {
		a.Name, a.MediaType, a.Extracted, a.Text = "contract.pdf", "application/pdf", true, "Clause 1. The parties agree."
	})
	add(events.AttachmentDocument, "long", func(a *events.Attachment) {
		a.Name, a.Extracted, a.Text, a.IncompleteReason = "long.pdf", true, longText, "cost_limit"
	})
	add(events.AttachmentDocument, "unextracted", func(a *events.Attachment) { a.Name = "new.pdf" })
	add(events.AttachmentDocument, "rejected", func(a *events.Attachment) {
		a.Name, a.Extracted, a.RejectedReason = "scan.pdf", true, "no readable text"
	})
	// A notion-style file id carries colons of its own.
	add(events.AttachmentImage, "block:abc", func(a *events.Attachment) { a.MediaType, a.StorageKey = "image/png", "k/small" })
	server := newAttachmentServer(t, reader)
	fetch := func(id string, more map[string]any) map[string]any {
		arguments := map[string]any{"id": id}
		for k, v := range more {
			arguments[k] = v
		}
		return callMCPTool(t, server, "alex@example.com", "attachment", arguments)
	}

	small := fetch("image:7:small", nil)
	require.NotEqual(t, true, small["isError"])
	content := small["content"].([]any)
	require.Len(t, content, 2)
	image := content[1].(map[string]any)
	require.Equal(t, "image", image["type"])
	require.Equal(t, "image/png", image["mimeType"])
	require.Equal(t, base64.StdEncoding.EncodeToString(png), image["data"])
	require.Equal(t, "a latency graph", small["structuredContent"].(map[string]any)["caption"])
	require.NotContains(t, small["structuredContent"].(map[string]any), "note")

	require.Len(t, fetch("image:7:block:abc", nil)["content"].([]any), 2)

	before := reader.byteReads
	huge := fetch("image:7:huge", nil)
	require.Len(t, huge["content"].([]any), 1, "an oversized image returns no image content")
	require.Contains(t, huge["structuredContent"].(map[string]any)["note"], "over the 4 MB limit")
	require.Equal(t, "a huge poster", huge["structuredContent"].(map[string]any)["caption"])
	require.Equal(t, before, reader.byteReads, "an image recorded as oversized is never read")

	grown := fetch("image:7:grown", nil)
	require.Len(t, grown["content"].([]any), 1)
	require.Contains(t, grown["structuredContent"].(map[string]any)["note"], "over the 4 MB limit")

	pending := fetch("image:7:pending", nil)
	require.Len(t, pending["content"].([]any), 1)
	require.Contains(t, pending["structuredContent"].(map[string]any)["note"], "not stored")

	doc := fetch("document:7:doc", nil)["structuredContent"].(map[string]any)
	require.Equal(t, "Clause 1. The parties agree.", doc["text"])
	require.Equal(t, "contract.pdf", doc["name"])
	require.NotContains(t, doc, "next_offset")

	var read strings.Builder
	offset, pages := 0, 0
	for {
		page := fetch("document:7:long", map[string]any{"offset": offset})["structuredContent"].(map[string]any)
		require.Contains(t, page["note"], "partial (cost_limit)")
		read.WriteString(page["text"].(string))
		pages++
		next, more := page["next_offset"].(float64)
		if !more {
			break
		}
		require.Greater(t, int(next), offset)
		offset = int(next)
	}
	require.Equal(t, 2, pages)
	require.Equal(t, longText, read.String(), "paging returns the whole text exactly once")
	require.Contains(t, toolErrorText(t, fetch("document:7:long", map[string]any{"offset": len(longText) + 1})), "offset must be 0 or a next_offset")

	require.Contains(t, fetch("document:7:unextracted", nil)["structuredContent"].(map[string]any)["note"], "not been extracted yet")
	require.Contains(t, fetch("document:7:rejected", nil)["structuredContent"].(map[string]any)["note"], "no readable text")
}

// TestCorpusMCPAttachmentIsScopedToCaller pins the attachment tool to the
// parent event's visibility: an attachment of an event outside the caller's
// scopes reads exactly like one that does not exist, and nothing is fetched
// for it.
func TestCorpusMCPAttachmentIsScopedToCaller(t *testing.T) {
	secret := []byte("private screenshot")
	reader := &fakeAttachmentReader{
		// The test server grants every caller slack-channel:C1 only.
		eventScopes: map[int64]string{7: "slack-channel:C1", 9: "slack-channel:CPRIVATE", 10: events.ScopeUnstamped},
		blobs:       map[string][]byte{"k/visible": []byte("ok"), "k/secret": secret},
		attachments: map[string]events.Attachment{},
	}
	for eventID, key := range map[int64]string{7: "k/visible", 9: "k/secret", 10: "k/secret"} {
		image := fakeAttachment(eventID, events.AttachmentImage, "F1", func(a *events.Attachment) {
			a.MediaType, a.StorageKey, a.Caption = "image/png", key, "caption of "+key
		})
		doc := fakeAttachment(eventID, events.AttachmentDocument, "F2", func(a *events.Attachment) {
			a.Extracted, a.Text = true, "text of "+key
		})
		reader.attachments[image.String()], reader.attachments[doc.String()] = image, doc
	}
	server := newAttachmentServer(t, reader)
	fetch := func(user, id string) map[string]any {
		return callMCPTool(t, server, user, "attachment", map[string]any{"id": id})
	}

	require.NotEqual(t, true, fetch("alex@example.com", "image:7:F1")["isError"])
	reads, byteReads := reader.reads, reader.byteReads

	missing := toolErrorText(t, fetch("alex@example.com", "image:11:F1"))
	require.Equal(t, `attachment: no attachment with id "image:11:F1" accessible from this context`, missing)
	for _, id := range []string{"image:9:F1", "document:9:F2", "image:10:F1", "document:10:F2"} {
		result := fetch("alex@example.com", id)
		require.Equal(t, strings.ReplaceAll(missing, "image:11:F1", id), toolErrorText(t, result),
			"a hidden attachment must read exactly like a missing one")
		raw, err := json.Marshal(result)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "k/secret")
		require.NotContains(t, string(raw), base64.StdEncoding.EncodeToString(secret))
	}
	require.Equal(t, reads, reader.reads, "the event's scope is checked before the attachment is read")
	require.Equal(t, byteReads, reader.byteReads)

	// An attachment the event does not have, on an event the caller can read.
	require.Equal(t, strings.ReplaceAll(missing, "image:11:F1", "image:7:nope"),
		toolErrorText(t, fetch("alex@example.com", "image:7:nope")))

	require.Contains(t, toolErrorText(t, fetch("", "image:7:F1")), "verified user identity is required")
	for _, id := range []string{"", "F1", "image:x:F1", "video:7:F1", "image:7:", "image:-7:F1"} {
		require.Contains(t, toolErrorText(t, fetch("alex@example.com", id)), "is not one an expand result lists", id)
	}
}

// TestReadAttachmentRechecksScopeInTheStore: the store's own scope check
// stands even if the event's scope changes between the two checks.
func TestReadAttachmentRechecksScopeInTheStore(t *testing.T) {
	a := fakeAttachment(7, events.AttachmentDocument, "F2", func(a *events.Attachment) { a.Extracted, a.Text = true, "secret" })
	reader := &fakeAttachmentReader{
		eventScopes: map[int64]string{7: "slack-channel:CPRIVATE"},
		attachments: map[string]events.Attachment{a.String(): a},
	}
	ctx := WithAllowedScopes(t.Context(), []string{"public", "slack-channel:C1"})
	stale := &fakeScopeLookup{scopes: map[int64]string{7: "slack-channel:C1"}}
	_, _, err := readAttachment(ctx, reader, stale, attachmentInput{ID: a.String()})
	require.EqualError(t, err, `no attachment with id "document:7:F2" accessible from this context`)
	require.Equal(t, 1, reader.reads)
}

func startMCPAsk(t *testing.T, server *httptest.Server, user string, arguments map[string]any) map[string]any {
	t.Helper()
	return callMCPTool(t, server, user, "ask", arguments)
}

func awaitMCPTask(t *testing.T, server *httptest.Server, user, taskID string) map[string]any {
	t.Helper()
	var completed map[string]any
	require.Eventually(t, func() bool {
		completed = resultObject(t, callRPC(t, server, user, "tasks/get", taskID, map[string]any{"taskId": taskID}, true))
		return completed["status"] == "completed"
	}, 2*time.Second, 10*time.Millisecond)
	return completed["result"].(map[string]any)
}

// TestCorpusMCPAskContinuesOnlyTheCallersOwnSession: session_id continues a
// conversation its owner started, and is refused, before any task or turn
// starts, for anyone else, for an unknown session, and for a malformed id,
// all with one message.
func TestCorpusMCPAskContinuesOnlyTheCallersOwnSession(t *testing.T) {
	runner := newFakeCorpusAskRunner()
	close(runner.release)
	server := newCorpusMCPHTTPServer(t, &fakeCorpusRetriever{}, runner)

	first := startMCPAsk(t, server, "alex@example.com", map[string]any{"question": "what changed?"})
	require.Equal(t, "task", first["resultType"])
	result := awaitMCPTask(t, server, "alex@example.com", first["taskId"].(string))
	sessionID := result["structuredContent"].(map[string]any)["session_id"].(string)
	require.Equal(t, fakeCreatedSession.String(), sessionID)

	followUp := startMCPAsk(t, server, "alex@example.com", map[string]any{"question": "and before that?", "session_id": sessionID})
	require.Equal(t, "task", followUp["resultType"])
	result = awaitMCPTask(t, server, "alex@example.com", followUp["taskId"].(string))
	require.Equal(t, sessionID, result["structuredContent"].(map[string]any)["session_id"])
	runner.mu.Lock()
	require.Equal(t, 1, runner.created, "a continued conversation creates no session")
	require.Equal(t, fakeCreatedSession, runner.request.SessionID)
	require.Equal(t, "and before that?", runner.request.UserInput)
	require.Equal(t, "alex@example.com", runner.request.Asker.Email)
	runner.mu.Unlock()

	unknown := uuid.NewString()
	refusal := toolErrorText(t, startMCPAsk(t, server, "alex@example.com", map[string]any{"question": "q", "session_id": unknown}))
	require.Equal(t, fmt.Sprintf("session_id %q is not an ask session of yours; omit it to start a new conversation", unknown), refusal)
	for user, id := range map[string]string{"mallory@example.com": sessionID, "alex@example.com": "not-a-uuid"} {
		got := toolErrorText(t, startMCPAsk(t, server, user, map[string]any{"question": "q", "session_id": id}))
		require.Equal(t, strings.ReplaceAll(refusal, unknown, id), got)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	require.Equal(t, "and before that?", runner.request.UserInput, "a refused continuation runs no turn")
	require.Equal(t, 1, runner.created)
	require.Zero(t, runner.held)
}

// TestCorpusMCPAskArtifactsAreMCPResources: an ask answer's images come back
// as resource links the session's owner reads with resources/read, and no one
// else can.
func TestCorpusMCPAskArtifactsAreMCPResources(t *testing.T) {
	artifactID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	png := []byte("\x89PNG panel")
	runner := newFakeCorpusAskRunner()
	runner.result = TurnResult{Reply: "see the panel", Artifacts: []ArtifactDescriptor{{
		ID: artifactID, MediaType: "image/png", ByteSize: int64(len(png)), Width: 800, Height: 400,
		Title: "p99 latency", AltText: "latency rising after the deploy",
		URL: "/v1/ask/" + fakeCreatedSession.String() + "/artifacts/" + artifactID.String(),
	}}}
	runner.artifacts = map[string][]byte{fakeCreatedSession.String() + "/" + artifactID.String(): png}
	close(runner.release)
	server := newCorpusMCPHTTPServer(t, &fakeCorpusRetriever{}, runner)

	created := startMCPAsk(t, server, "alex@example.com", map[string]any{"question": "show me latency"})
	result := awaitMCPTask(t, server, "alex@example.com", created["taskId"].(string))
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "/v1/", "an MCP client cannot fetch /v1 URLs")

	uri := "diderot://ask/" + fakeCreatedSession.String() + "/artifacts/" + artifactID.String()
	require.Equal(t, []any{map[string]any{
		"uri": uri, "media_type": "image/png", "byte_size": float64(len(png)), "width": float64(800), "height": float64(400),
		"title": "p99 latency", "alt_text": "latency rising after the deploy",
	}}, result["structuredContent"].(map[string]any)["artifacts"])
	content := result["content"].([]any)
	require.Len(t, content, 2)
	link := content[1].(map[string]any)
	require.Equal(t, "resource_link", link["type"])
	require.Equal(t, uri, link["uri"])
	require.Equal(t, "image/png", link["mimeType"])
	require.Equal(t, "p99 latency", link["title"])

	templates := resultObject(t, callRPC(t, server, "", "resources/templates/list", "", map[string]any{}, true))
	require.Equal(t, askArtifactURITemplate, templates["resourceTemplates"].([]any)[0].(map[string]any)["uriTemplate"])

	read := func(user, uri string) rpcResponse {
		return callRPC(t, server, user, "resources/read", uri, map[string]any{"uri": uri}, true)
	}
	contents := resultObject(t, read("alex@example.com", uri))["contents"].([]any)
	require.Len(t, contents, 1)
	blob := contents[0].(map[string]any)
	require.Equal(t, uri, blob["uri"])
	require.Equal(t, "image/png", blob["mimeType"])
	require.Equal(t, base64.StdEncoding.EncodeToString(png), blob["blob"])

	otherArtifact := "diderot://ask/" + fakeCreatedSession.String() + "/artifacts/" + uuid.NewString()
	for name, attempt := range map[string]rpcResponse{
		"another caller":      read("mallory@example.com", uri),
		"no identity":         read("", uri),
		"unknown artifact":    read("alex@example.com", otherArtifact),
		"unknown session":     read("alex@example.com", "diderot://ask/"+uuid.NewString()+"/artifacts/"+artifactID.String()),
		"malformed artifact":  read("alex@example.com", "diderot://ask/"+fakeCreatedSession.String()+"/artifacts/nope"),
		"unregistered scheme": read("alex@example.com", "file:///etc/passwd"),
	} {
		require.NotNil(t, attempt.Error, name)
		require.Equal(t, "Resource not found", attempt.Error.Message, name)
		require.Empty(t, attempt.Result, name)
	}
}

func TestAskArtifactURIRoundTrips(t *testing.T) {
	session, artifact := uuid.New(), uuid.New()
	gotSession, gotArtifact, ok := parseAskArtifactURI(askArtifactURI(session, artifact))
	require.True(t, ok)
	require.Equal(t, session, gotSession)
	require.Equal(t, artifact, gotArtifact)
	for _, uri := range []string{"", "diderot://ask/", "diderot://ask/" + session.String(), "https://ask/" + session.String() + "/artifacts/" + artifact.String()} {
		_, _, ok := parseAskArtifactURI(uri)
		require.False(t, ok, uri)
	}
}

func TestParseAttachmentRefRoundTrips(t *testing.T) {
	for _, ref := range []events.AttachmentRef{
		{EventID: 7, Kind: events.AttachmentImage, FileID: "F0ABC"},
		{EventID: 12345678901, Kind: events.AttachmentDocument, FileID: "block-id:content:key"},
	} {
		got, err := events.ParseAttachmentRef(ref.String())
		require.NoError(t, err)
		require.Equal(t, ref, got)
	}
}
