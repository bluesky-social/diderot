package files

// shared.go exports the source-agnostic pieces of this package for
// reuse by sibling attachment pipelines — today
// internal/attachments/docs, which extracts text from document
// attachments (PDF/docx/xlsx/...) the way this package captions image
// attachments.
//
// The two workers are deliberately separate: this one is image-shaped
// throughout (VLM caption prompt, Gemini's exact content-type
// allowlist, a too-small-dimension pre-filter, a 1:1 chunk FK), and
// parameterizing it to serve two content models would make both
// harder to reason about. What genuinely IS shared — byte fetching,
// the S3 mirror, secret redaction, error classification, and the
// content-addressed key layout — lives here and is used verbatim by
// both, so there's exactly one implementation of each.

import "context"

// Fetcher resolves source-specific bytes for one attachment. Same
// contract as SourceFetcher (which this package's caption worker
// uses); named without the "Image" because document pipelines use the
// identical call. Slack's fetcher is content-type agnostic — it
// resolves url_private via files.info and GETs it — so one
// implementation serves both.
type Fetcher interface {
	Fetch(ctx context.Context, sourceFileID string) (data []byte, contentType string, err error)
}

// Redactor is the exported view of the gitleaks-backed secret
// scrubber. Extracted document text needs the same treatment captions
// get: a shared PDF can carry a token or a private key just as a
// screenshot of a .env file can, and once it's in event_chunks it's
// embedded and retrievable.
type Redactor interface {
	// Redact replaces detected secrets with "[REDACTED:<rule-id>]" and
	// returns the rule ids that fired (never the secret values).
	Redact(s string) (redacted string, findings []string)
}

// NewRedactor returns the process-wide Redactor over gitleaks' default ruleset
// (~150 regexes). Initialization is guarded and cached, so image and document
// workers cannot race gitleaks' package-global config parser. On an error
// there is no Redactor, and the caller must not index text without one.
func NewRedactor() (Redactor, error) {
	r, err := newSecretRedactor()
	if err != nil {
		return nil, err
	}
	return r, nil
}

// SlackChannelNotBlocked is the claim-query condition that leaves an
// attachment unclaimed while an administrator's block is on its message's
// Slack channel: a block stops Diderot reading the channel, and captioning
// or extracting an attachment reads it from Slack. The row stays pending,
// spending none of its failure budget, and is claimed after an unblock.
// eventID is the SQL expression for the attachment's event id.
func SlackChannelNotBlocked(eventID string) string {
	return `NOT EXISTS (SELECT 1 FROM slack_message_details blocked_message
	                      JOIN slack_ingest_blacklist blocked ON blocked.channel_id = blocked_message.channel_id
	                     WHERE blocked_message.event_id = ` + eventID + `)`
}

// SHA256Hex returns the lowercase hex sha256 of data — the content
// address used to build storage keys.
func SHA256Hex(data []byte) string { return sha256Hex(data) }

// StorageKeyFor returns the canonical S3 key for a (source, sha256)
// pair: "<source>/<sha256[0:2]>/<sha256><ext>". Two-character prefix
// sharding keeps ListBucket usable. Shared so image and document
// bytes land in one consistent layout in the same bucket.
func StorageKeyFor(source, sha256Hex, ext string) string {
	return storageKeyFor(source, sha256Hex, ext)
}

// ClassifyHTTPStatus wraps an error synthesized from an HTTP status as
// *TransientError for 429 and 5xx, so a source's fetcher classifies
// its own client's answers the way the workers expect.
func ClassifyHTTPStatus(status int, err error) error { return classifyHTTPStatus(status, err) }

// IsNetworkError reports whether err is a connection that didn't
// make it or didn't answer in time, which is transient whichever
// service was on the other end.
func IsNetworkError(err error) bool { return isNetworkError(err) }

// ClassifyAWSError wraps an AWS SDK failure as *TransientError where
// the cause is recognizable (throttling, 5xx, network), so callers'
// retry loops make the right call.
func ClassifyAWSError(err error) error { return classifyAWSError(err) }
