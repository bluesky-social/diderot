package adminui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifyFailureSaysTheCauseInAPlainLine(t *testing.T) {
	for _, test := range []struct {
		raw, key, message string
		retryable         bool
	}{
		{`caption F0A1: gateway captioner: request: llmclient: status 400: {"error":{"message":"You uploaded an unsupported image. Please make sure your image is valid.","type":"invalid_request_error","code":"invalid_image_format"}}`,
			"malformed", "The file is malformed or could not be decoded", false},
		{`caption F0A2: gateway captioner: request: llmclient: status 400: {"type":"error","error":{"type":"invalid_request_error","message":"Could not process image"}}`,
			"malformed", "The file is malformed or could not be decoded", false},
		{"extract F0C1: permanent: docs: open docx: zip: not a valid zip file", "malformed", "The file is malformed or could not be decoded", false},
		{"extract F0C2: docs: open part: zip: not a valid zip file", "malformed", "The file is malformed or could not be decoded", false},
		{`caption F0A8: gateway captioner: request: llmclient: status 400: {"error":{"message":"The provided file uses a format or codec that is not supported.","type":"invalid_request_error"}}`,
			"unsupported", "Not a file type Diderot can read", false},
		{`caption F0A9: gateway captioner: non-image mediaType "video/mp4"`, "unsupported", "Not a file type Diderot can read", false},
		{`permanent: unsupported media type "application/zip" for archive.zip`, "unsupported", "Not a file type Diderot can read", false},
		{`extract F0D1: permanent: docs: SpreadsheetParser cannot read "xls" (excelize is OOXML-only; legacy .xls is BIFF and out of scope)`,
			"unsupported", "Not a file type Diderot can read", false},
		{`permanent: unsupported media type "application/zip" for quota-recitation.zip`, "unsupported", "Not a file type Diderot can read", false},
		{`llmclient: status 400: {"error":{"message":"You exceeded your current quota, please check your plan and billing details."}}`,
			"rate-limited", "The model provider limited the request rate", true},
		{`route F0D2: permanent: docs: no extraction mechanism for format "odt"`, "unsupported", "Not a file type Diderot can read", false},
		{"Vertex extract: vertex empty text (finishReason=RECITATION)", "recitation",
			"The model declined to transcribe the file because it resembles published text", false},
		{"Vertex extract: vertex empty text (finishReason=SAFETY)", "refused", "The model declined the content", false},
		{"caption F0E1: gateway captioner: empty text (finish_reason=content_filter)", "refused", "The model declined the content", false},
		{"Vertex extract: vertex empty text (finishReason=MAX_TOKENS)", "no-output", "The model returned no text", true},
		{"caption F0E2: gateway captioner: empty text (finish_reason=stop)", "no-output", "The model returned no text", true},
		{"caption F0E3: transient: gateway captioner: empty text (finish_reason=)", "no-output", "The model returned no text", true},
		{"extract F0E4: docs: gateway response carried no choices", "no-output", "The model returned no text", true},
		{"fetch slack/F0A3: permanent: slack files.info: file_not_found", "not-found", "File not found in Slack", false},
		{"fetch agent_session/sha256:ab: agent_session fetcher: no mirrored object at images/ab.*", "copy-missing",
			"The stored copy of the file is missing", true},
		{`fetch notion/0f0e0d0c-0b0a-4908-8706-050403020100:a1b2c3d4e5f6: notion bytes 0f0e0d0c-0b0a-4908-8706-050403020100: status 400`,
			"link-broken", "The image could not be downloaded from where the page links to it", false},
		{`fetch notion/0f0e0d0c-0b0a-4908-8706-050403020100:a1b2c3d4e5f6: notion bytes 0f0e0d0c-0b0a-4908-8706-050403020100: status 403`,
			"link-broken", "The image could not be downloaded from where the page links to it", false},
		{`fetch notion/0f0e0d0c-0b0a-4908-8706-050403020100:a1b2c3d4e5f6: transient: notion bytes 0f0e0d0c-0b0a-4908-8706-050403020100: status 502`,
			"link-broken", "The image could not be downloaded from where the page links to it", false},
		{`fetch notion/0f0e0d0c-0b0a-4908-8706-050403020100:a1b2c3d4e5f6: transient: notion bytes 0f0e0d0c-0b0a-4908-8706-050403020100: Get "https://images.example.test/a.jpg": ssrf: resolve images.example.test: lookup images.example.test on 10.0.0.10:53: no such host`,
			"link-broken", "The image could not be downloaded from where the page links to it", false},
		{`fetch notion/0f0e0d0c-0b0a-4908-8706-050403020100:a1b2c3d4e5f6: permanent: notion: block 0f0e0d0c-0b0a-4908-8706-050403020100 has an unfetchable image url "/images/getting-started-1.png"`,
			"link-broken", "The image could not be downloaded from where the page links to it", false},
		{"extract F0A4: permanent: docs: empty document", "empty", "The file contained no text", false},
		{"revision sha256:5f4e3d2c for 0f0e0d0c-0b0a-4908-8706-050403020100: session revision has no records",
			"session-empty", "The session has nothing in it to summarize", false},
		{"revision sha256:5f4e3d2c for 0f0e0d0c-0b0a-4908-8706-050403020100: session revision exceeds byte cap: feed reports 99992082 content bytes, cap 67108864",
			"session-too-large", "The session is too large to summarize", false},
		{"fetch revision sha256:5f4e3d2c for 0f0e0d0c-0b0a-4908-8706-050403020100: session revision exceeds byte cap: session revision response exceeds 67108864-byte cap",
			"session-too-large", "The session is too large to summarize", false},
		{"fetch revision sha256:5f4e3d2c for 0f0e0d0c-0b0a-4908-8706-050403020100: session feed http 503 at https://sessions.example.test/api/v1/sessions/0f0e0d0c/records",
			"session-fetch", "The session's records could not be fetched", true},
		{`sessioningest: summarize 0f0e0d0c-0b0a-4908-8706-050403020100: sessioningest: reduce: unexpected stop reason: "refusal"`,
			"refused", "The model declined the content", false},
		{"sessioningest: summarize 0f0e0d0c-0b0a-4908-8706-050403020100: sessioningest: reduce: reduce returned empty summary (narrative 0 chars)",
			"no-output", "The model returned no text", true},
		{"sessioningest: summarize 0f0e0d0c-0b0a-4908-8706-050403020100: sessioningest: map chunk 908/1085: response truncated at max_tokens (1024)",
			"bad-output", "The model's answer could not be used", true},
		{"sessioningest: summarize 0f0e0d0c-0b0a-4908-8706-050403020100: sessioningest: reduce: decode final summary: json: cannot unmarshal string into Go struct field FinalSummary.tags of type sessionsummary.TagSet",
			"bad-output", "The model's answer could not be used", true},
		{`sessioningest: summarize 0f0e0d0c-0b0a-4908-8706-050403020100: sessioningest: reduce: unexpected stop reason: "end_turn"`,
			"bad-output", "The model's answer could not be used", true},
		{"validate revision sha256:5f4e3d2c for 0f0e0d0c-0b0a-4908-8706-050403020100: session document is structurally invalid: record 3 has no role",
			"malformed", "The file is malformed or could not be decoded", false},
		{"sessioningest: summarize 0f0e0d0c-0b0a-4908-8706-050403020100: sessioningest: map chunk 3/9: converse: sessionsummary gateway: status 503: upstream call failed",
			"provider-error", "The model provider failed to answer", true},
		{"extract 5f4e3d2c1b0a99887766554433221100ffeeddccbbaa99887766554433221100: permanent: docs: docx contained no text", "docx-pictures",
			"The Word file has no text; retry to have its pictures described", true},
		{"extract F0F1: permanent: docs: pdf yielded no text (12 pages, 12 sparse, model configured: false)", "empty",
			"The file contained no text", false},
		{"extract F0F2: permanent: docs: xlsx contained no non-empty cells", "empty", "The file contained no text", false},
		{"extract F0F3: permanent: source returned zero bytes", "empty", "The file contained no text", false},
		{"extract F0A7: permanent: docs: open pdf: password required", "protected", "The file is password-protected", false},
		{"extract F0G1: permanent: docs: 73400320 bytes exceeds the 52428800-byte limit", "file-too-large",
			"The file is too large to read", false},
		{"extract F0G2: permanent: docs: word/document.xml declares 900000000 bytes, over the 104857600 cap", "file-too-large",
			"The file is too large to read", false},
		{"extract F0G3: permanent: docs: docx has 70000 zip entries, over the 10000 cap", "file-too-large",
			"The file is too large to read", false},
		{"fetch zendesk/5f4e3d2c1b0a99887766554433221100ffeeddccbbaa99887766554433221100: permanent: zendesk: attachment exceeds size limit",
			"file-too-large", "The file is too large to read", false},
		{`caption F0G4: gateway captioner: status 413: {"error":{"message":"LLM request body exceeded the buffer limit","type":"invalid_request_error","code":"request_body_too_large"}}`,
			"file-too-large", "The file is too large to read", false},
		{`embed batch: status 413: {"error":{"message":"Request too large"}}`, "too-large", "Request too large for the model", false},
		{`caption F0A5: gateway captioner: request: llmclient: status 429: {"error":{"message":"Rate limit reached","code":"rate_limit_exceeded"}}`,
			"rate-limited", "The model provider limited the request rate", true},
		{`llmclient: status 429: {"error":{"message":"Request too large for gpt-5.4 on tokens per min (TPM): Limit 30000, Requested 45000."}}`,
			"rate-limited", "The model provider limited the request rate", true},
		{`extract F0H1: transient: docs: gateway returned HTTP 429: {"error":{"message":"Unsupported file while throttled"}}`,
			"rate-limited", "The model provider limited the request rate", true},
		{"llmclient: status 401: Unauthorized", "credentials", "The provider refused the credentials", true},
		{`extract F0H2: permanent: docs: gateway auth failed with HTTP 403: {"error":"forbidden"}`, "credentials",
			"The provider refused the credentials", true},
		{"lock event: failed SASL auth: FATAL: password authentication failed for user \"diderot\"", "credentials",
			"The provider refused the credentials", true},
		{`extract F0H3: transient: docs: gateway returned HTTP 503: upstream connect error`, "provider-error",
			"The model provider failed to answer", true},
		{`caption F0H4: transient: gateway captioner: request: llmclient: status 529: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			"provider-error", "The model provider failed to answer", true},
		{"caption F0A6: gateway captioner: request: context deadline exceeded", "unreachable", "The request timed out or the connection failed", true},
		{`caption F0H5: transient: gateway captioner: request: Post "https://gateway.example.test/v1/chat/completions": dial tcp 10.0.0.1:443: connect: connection refused`,
			"unreachable", "The request timed out or the connection failed", true},
		{"extract F0H6: transient: docs: pdfium interrupted rendering page 3: context canceled", "interrupted",
			"Interrupted when Diderot restarted", true},
		{"s3 put images/ab.png: transient: PutObject bucket/images/ab.png: operation error S3: PutObject, exceeded maximum number of attempts",
			"storage", "File storage failed", true},
	} {
		got := classifyFailure(test.raw)
		assert.Equal(t, test.key, got.Key, test.raw)
		assert.Equal(t, test.message, got.Message, test.raw)
		assert.Equal(t, test.retryable, got.Retryable, test.raw)
	}
}

// A cause is retryable only when it lies outside the item: a new cause is
// added here on purpose, with which side it is on.
func TestKnownCausesRetryOnlyWhatLiesOutsideTheItem(t *testing.T) {
	retryable := map[string]bool{
		"rate-limited": true, "interrupted": true, "copy-missing": true, "no-output": true, "credentials": true,
		"session-fetch": true, "bad-output": true, "docx-pictures": true, "session-empty": false, "session-too-large": false,
		"provider-error": true, "unreachable": true, "storage": true,
		"not-found": false, "link-broken": false, "recitation": false, "refused": false, "empty": false, "unsupported": false,
		"protected": false, "malformed": false, "file-too-large": false, "too-large": false,
	}
	seen := map[string]bool{}
	for _, known := range knownCauses {
		want, ok := retryable[known.cause.Key]
		require.True(t, ok, "cause %q is not in this test", known.cause.Key)
		require.False(t, seen[known.cause.Key], "cause %q is listed twice", known.cause.Key)
		seen[known.cause.Key] = true
		assert.Equal(t, want, known.cause.Retryable, known.cause.Key)
		assert.False(t, strings.HasSuffix(known.cause.Message, "."), known.cause.Key)
		for _, pattern := range known.patterns {
			assert.Equal(t, strings.ToLower(pattern), pattern, "patterns are matched against lower-case text")
		}
	}
	assert.Len(t, seen, len(retryable))
}

func TestClassifyFailureCallsAnUnknownCauseAnotherError(t *testing.T) {
	first := classifyFailure(`caption F0B1: gateway captioner: request: llmclient: status 400: {"error":{"message":"Invalid 'messages[0].content[1]': request req_8a7d6c5b4a3f has 3 images. Use at most 1."}}`)
	second := classifyFailure(`caption F0B2: gateway captioner: request: llmclient: status 400: {"error":{"message":"Invalid 'messages[0].content[1]': request req_1b2c3d4e5f6a has 4 images. Use at most 1."}}`)
	assert.Equal(t, "Another error", first.Message, "what the failure said is not a title")
	assert.Equal(t, "Invalid 'messages[0].content[1]': request req_8a7d6c5b4a3f has 3 images", first.Detail,
		"the provider's message, to its first sentence")
	assert.Equal(t, first.Key, second.Key, "IDs and numbers don't split a cause")
	assert.True(t, strings.HasPrefix(first.Key, "other-"))
	assert.True(t, first.Retryable)

	wrapped := classifyFailure("Vertex extract: vertex returned a part of kind 7")
	assert.Equal(t, "Another error", wrapped.Message, "a wrapped internal error is never the title")
	assert.Equal(t, "Vertex extract: vertex returned a part of kind 7", wrapped.Detail)
	assert.NotEqual(t, first.Key, wrapped.Key, "unrelated unknown errors are separate groups, each with its own retry")
	assert.True(t, wrapped.Retryable)
	assert.Equal(t, wrapped.Key, classifyFailure("Vertex extract: vertex returned a part of kind 12").Key)

	// A UUID or a digest is letters between its digits: it must not make
	// every failure of one unknown cause its own group.
	assert.Equal(t,
		classifyFailure("fetch linear/0f0e0d0c-0b0a-4908-8706-050403020100: attachment 0f0e0d0c-0b0a-4908-8706-050403020100 is not an upload").Key,
		classifyFailure("fetch linear/a9b8c7d6-e5f4-4a3b-8c2d-1e0f9a8b7c6d: attachment a9b8c7d6-e5f4-4a3b-8c2d-1e0f9a8b7c6d is not an upload").Key)
	assert.Equal(t,
		classifyFailure("a9b8c7d6-e5f4-4a3b-8c2d-1e0f9a8b7c6d is not an upload").Key,
		classifyFailure("0f0e0d0c-0b0a-4908-8706-050403020100 is not an upload").Key,
		"an ID that opens the detail, where its first letter is capitalized, is still an ID")
	assert.Equal(t,
		classifyFailure("commit: object 5f4e3d2c1b0a99887766554433221100ffeeddccbbaa99887766554433221100 changed while it was read").Key,
		classifyFailure("commit: object 00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff changed while it was read").Key)

	plain := classifyFailure("extract F0C3: docs: parent event vanished mid-extraction")
	permanent := classifyFailure("extract F0C4: permanent: docs: parent event vanished mid-extraction")
	assert.Equal(t, "Parent event vanished mid-extraction", plain.Detail, "a Go error loses its wrappers")
	assert.Equal(t, plain.Detail, permanent.Detail)
	assert.NotEqual(t, plain.Key, permanent.Key, "one group has one answer to whether it can be retried")
	assert.True(t, plain.Retryable)
	assert.False(t, permanent.Retryable, "what a worker marked permanent fails the same way on a retry")

	summary := classifyFailure(`sessioningest: summarize 0f0e0d0c-0b0a-4908-8706-050403020100: sessioningest: checkpoint map chunk: ERROR: new row violates check constraint "c" (SQLSTATE 23514)`)
	assert.Equal(t, `Checkpoint map chunk: ERROR: new row violates check constraint "c" (SQLSTATE 23514)`, summary.Detail,
		"a session summary's error loses its wrappers too")

	unrecorded := classifyFailure("")
	assert.Equal(t, "Failed without a recorded reason", unrecorded.Message)
	assert.Empty(t, unrecorded.Detail)
}

// The ui-dev Failures page shows what the classifier would say of each
// failure it lists, so the page reviewed there is the page a live server draws.
func TestUIDevFailuresAgreeWithTheClassifier(t *testing.T) {
	fixture, err := loadDevFixture("testdata/ui-dev.json")
	require.NoError(t, err)
	var page failuresData
	require.NoError(t, json.Unmarshal(fixture.Pages["operations_failures"], &page))
	require.NotEmpty(t, page.Items)
	other := false
	for _, item := range page.Items {
		assert.Equal(t, classifyFailure(item.Raw), item.Cause, item.Raw)
		other = other || item.Cause.Message == otherCauseMessage
	}
	assert.True(t, other, "the fixture has a cause no known cause matches")
}
