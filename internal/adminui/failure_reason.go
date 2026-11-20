package adminui

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"unicode"
)

// failureCause is why a piece of background work failed, in one plain line.
type failureCause struct {
	// Key groups failures of the same cause; it is stable and URL-safe.
	Key string
	// Message says the cause to an administrator.
	Message string
	// Detail is what the failure itself said, cut to a line, for a cause
	// Message can only call another error.
	Detail string
	// Retryable reports that trying again can work once whatever caused it
	// is fixed. A problem with the item itself, such as a file that is no
	// longer there, fails the same way every time.
	Retryable bool
}

// causeInterrupted is the key of work cut short as Diderot stopped.
const causeInterrupted = "interrupted"

// otherCauseMessage names a cause none of knownCauses matches. What the
// failure said stays out of it: that is a wrapped error only its author can
// read, and the page shows it with each item.
const otherCauseMessage = "Another error"

// knownCauses map what a failure's text contains to its cause, first match
// wins. The patterns are lower-case. A cause is retryable only when it lies
// outside the item (the provider, the network, credentials, storage); what
// is wrong with the file itself fails the same way every time. A rate limit
// comes first, because a provider can answer 429 with a message that sounds
// like a problem with the item, and a refusal comes before an empty answer,
// because a model that declines returns no text.
var knownCauses = []struct {
	cause    failureCause
	patterns []string
}{
	{failureCause{Key: "rate-limited", Message: "The model provider limited the request rate", Retryable: true},
		[]string{"status 429", "http 429", "rate limit", "rate_limit", "too many requests", "throttl", "ratelimited",
			"resource_exhausted", "resource exhausted", "quota exceeded", "exceeded your current quota"}},
	{failureCause{Key: causeInterrupted, Message: "Interrupted when Diderot restarted", Retryable: true},
		[]string{"context canceled", "code = canceled"}},
	// An agent session's own causes, before the file causes their texts
	// would otherwise match, and "nothing in it" and "too large" before
	// "could not be fetched": a fetch reports both.
	{failureCause{Key: "session-empty", Message: "The session has nothing in it to summarize"},
		[]string{"session revision has no records"}},
	{failureCause{Key: "session-too-large", Message: "The session is too large to summarize"},
		[]string{"session revision exceeds"}},
	{failureCause{Key: "session-fetch", Message: "The session's records could not be fetched", Retryable: true},
		[]string{"fetch revision ", "load pushed revision "}},
	{failureCause{Key: "not-found", Message: "File not found in Slack"},
		[]string{"file_not_found", "file_deleted"}},
	// Before the provider and network causes: the site a page links an
	// image from answers with the same statuses a model provider does, and
	// the worker has already retried what was only a bad moment.
	{failureCause{Key: "link-broken", Message: "The image could not be downloaded from where the page links to it"},
		[]string{"unfetchable image url", "notion bytes "}},
	{failureCause{Key: "copy-missing", Message: "The stored copy of the file is missing", Retryable: true},
		[]string{"no mirrored object", "(missing)"}},
	{failureCause{Key: "recitation", Message: "The model declined to transcribe the file because it resembles published text"},
		[]string{"=recitation"}},
	{failureCause{Key: "refused", Message: "The model declined the content"},
		[]string{"content_policy", "content policy", "content_filter", "content filter", "guardrail",
			"=safety", "=prohibited_content", "=blocklist", "=spii", `stop reason: "refusal"`}},
	// What a Word file with no text failed with before Diderot described the
	// pictures inside documents. Reading it again finds them, and settles a
	// file with neither text nor pictures instead of failing it, so nothing
	// fails this way any more.
	{failureCause{Key: "docx-pictures", Message: "The Word file has no text; retry to have its pictures described", Retryable: true},
		[]string{"docx contained no text"}},
	{failureCause{Key: "empty", Message: "The file contained no text"},
		[]string{"empty document", "empty pdf", "pdf has no pages", "has no sheets", "no non-empty cells",
			"empty spreadsheet", "returned zero bytes", "extractor returned empty text", "yielded no text"}},
	{failureCause{Key: "no-output", Message: "The model returned no text", Retryable: true},
		[]string{"empty caption", "empty text (finish_reason", "empty text (finishreason", "carried no choices",
			"returned empty summary", "empty narrative"}},
	// After a refusal and an empty answer, which stop a model the same way.
	{failureCause{Key: "bad-output", Message: "The model's answer could not be used", Retryable: true},
		[]string{"truncated at max_tokens", "decode final summary", "unexpected stop reason"}},
	{failureCause{Key: "unsupported", Message: "Not a file type Diderot can read"},
		[]string{"non-image mediatype", "unsupported media type", "unsupported file", "unsupported_file",
			"format or codec", "unsupported format", "unsupported codec", "unsupported document",
			"parser cannot read", "no extraction mechanism"}},
	// Before a protected file: a refused login can mention a password too.
	{failureCause{Key: "credentials", Message: "The provider refused the credentials", Retryable: true},
		[]string{"status 401", "status 403", "http 401", "http 403", "unauthorized", "forbidden",
			"invalid api key", "invalid_api_key", "authentication", "permission denied", "accessdenied"}},
	{failureCause{Key: "protected", Message: "The file is password-protected"},
		[]string{"password", "encrypted"}},
	{failureCause{Key: "malformed", Message: "The file is malformed or could not be decoded"},
		[]string{"invalid_image", "invalid image", "could not process image", "not represent a valid image",
			"image_parse_error", "unsupported image", "failed to decode", "decode image", "open pdf",
			"pdf page count", "open docx", "open xlsx", "open document part", "read document part",
			"parse document.xml", "corrupt", "malformed", "not a valid zip", "structurally invalid"}},
	{failureCause{Key: "file-too-large", Message: "The file is too large to read"},
		[]string{"-byte limit", "-byte cap", "bytes, over the", "zip entries, over the", "exceeds size limit",
			// The gateway refuses a request body over its limit, which only a
			// file sent whole reaches.
			"request_body_too_large"}},
	{failureCause{Key: "too-large", Message: "Request too large for the model"},
		[]string{"status 413", "http 413", "request too large", "request_too_large", "too large",
			"context_length_exceeded", "maximum context length", "too many tokens", "token limit",
			"prompt is too long"}},
	{failureCause{Key: "provider-error", Message: "The model provider failed to answer", Retryable: true},
		[]string{"status 500", "status 502", "status 503", "status 504", "status 529", "http 500", "http 502",
			"http 503", "http 504", "internal server error", "bad gateway", "service unavailable", "overloaded",
			"server_error", "code = unavailable", "code = internal"}},
	{failureCause{Key: "unreachable", Message: "The request timed out or the connection failed", Retryable: true},
		[]string{"deadline exceeded", "timed out", "timeout", "status 408", "http 408", "connection reset",
			"connection refused", "no such host", "unexpected eof", "broken pipe", "dial tcp", "tls handshake"}},
	{failureCause{Key: "storage", Message: "File storage failed", Retryable: true},
		[]string{"s3 put", "putobject", "getobject"}},
}

// classifyFailure reads a stored failure: a provider's JSON error, a wrapped
// Go error, or both. A failure no known cause matches is another error,
// grouped by what it said, normalized so IDs and numbers don't split a group
// and unrelated errors don't share one; retrying can get past it unless the
// worker marked it permanent.
func classifyFailure(raw string) failureCause {
	lower := strings.ToLower(raw)
	for _, known := range knownCauses {
		for _, pattern := range known.patterns {
			if strings.Contains(lower, pattern) {
				return known.cause
			}
		}
	}
	retryable := !strings.Contains(lower, "permanent:")
	detail := providerMessage(raw)
	if detail == "" {
		detail = unwrapError(raw)
	}
	detail = firstSentence(detail, 140)
	if detail == "" {
		return failureCause{Key: "other-unrecorded", Message: "Failed without a recorded reason", Retryable: retryable}
	}
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(normalizeFailure(detail)))
	if !retryable {
		// What a worker marked permanent is its own group: one group has one
		// answer to whether it can be retried.
		_, _ = digest.Write([]byte("\x00permanent"))
	}
	return failureCause{
		Key: fmt.Sprintf("other-%08x", digest.Sum32()), Message: otherCauseMessage, Detail: detail,
		Retryable: retryable,
	}
}

// providerMessage is the message of the JSON error body a failure carries,
// as OpenAI, Anthropic, and most gateways shape one, or "".
func providerMessage(raw string) string {
	start := strings.Index(raw, "{")
	if start < 0 {
		return ""
	}
	var body map[string]any
	if err := json.NewDecoder(strings.NewReader(raw[start:])).Decode(&body); err != nil {
		return ""
	}
	if nested, ok := body["error"].(map[string]any); ok {
		if message, ok := nested["message"].(string); ok && strings.TrimSpace(message) != "" {
			return message
		}
	}
	for _, key := range []string{"message", "error", "detail"} {
		if message, ok := body[key].(string); ok && strings.TrimSpace(message) != "" {
			return message
		}
	}
	return ""
}

// errorWrapper is one "context: " a Go error chain puts before its cause.
var errorWrapper = regexp.MustCompile(`^(?:(?:caption|extract|fetch|route|summarize|map chunk|s3 put|lock event|insert chunk|gate check|gate recheck) \S+|revision \S+ for \S+|transient|permanent|docs|files|llmclient|sessioningest|reduce|converse|sessionsummary gateway|gateway captioner|gateway extractor|request|embed[a-z ]*)\s*:\s*`)

// unwrapError drops the wrappers a Go error chain adds before its cause.
func unwrapError(raw string) string {
	text := strings.TrimSpace(raw)
	for {
		next := errorWrapper.ReplaceAllString(text, "")
		if next == text || next == "" {
			return text
		}
		text = next
	}
}

// firstSentence is text on one line, cut at its first sentence or at limit runes,
// and capitalized.
func firstSentence(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if end := strings.Index(text, ". "); end > 0 {
		text = text[:end]
	}
	text = strings.TrimSuffix(compactText(text, limit), ".")
	runes := []rune(text)
	if len(runes) == 0 {
		return ""
	}
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// failureVaries matches an ID or a number. The hexadecimal IDs come first: a
// UUID or a digest is letters between its digits, which blanking the digits
// alone would leave different in every failure. They match in either case:
// a detail that begins with one has had its first letter capitalized.
var failureVaries = regexp.MustCompile(`(?i:\b[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\b|\b[0-9a-f]{16,}\b)|[0-9]+|\b[A-Z][A-Z0-9]{7,}\b|\b[a-z]+_[A-Za-z0-9]{8,}\b`)

// normalizeFailure blanks what differs between failures of one cause:
// numbers and IDs.
func normalizeFailure(message string) string {
	return strings.ToLower(failureVaries.ReplaceAllString(message, "#"))
}
