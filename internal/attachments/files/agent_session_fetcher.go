package files

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AgentSessionFetcher is the SourceFetcher registered for
// events.source="agent_session". It reads the mirrored bytes back
// from S3 by sha256. The retired transcript pre-pass mirrored these historical
// assets at extraction time (unlike Slack, there is no url_private to re-fetch
// on demand).
//
// The source_image_id encoding scheme is "sha256:<hex>", where the suffix is
// the hash of the decoded image bytes. This fetcher decodes the prefix,
// reconstructs the
// content-addressed S3 key, and returns the bytes + Content-Type.
//
// Replaces the older agentSessionFetcherStub that returned
// ErrAgentSessionNotImplemented — the bytes ARE available now,
// via the historical S3 mirror.
type AgentSessionFetcher struct {
	s3 S3Mirror
}

// NewAgentSessionFetcher wires a fetcher against the shared S3
// mirror containing assets written before the native-session cutover.
func NewAgentSessionFetcher(s3 S3Mirror) *AgentSessionFetcher {
	return &AgentSessionFetcher{s3: s3}
}

// FetchImage resolves a "sha256:<hex>" source_image_id to the
// mirrored S3 object. Returns the bytes + Content-Type from the
// object metadata.
//
// The key format matches the retired writer's storage key:
// "agent_session/<sha256[0:2]>/<sha256>". Extension is not stored
// on the source_image_id, so resolution is media-type-driven: mediaType
// is the event_images.media_type the pre-pass recorded when it wrote
// the object, and agentSessionExtensionFor deliberately mirrors that
// writer's exact mapping. The exact key is tried first, which matters
// when the credentials have GetObject but not ListBucket: S3 then answers
// a nonexistent key with 403 AccessDenied rather than NoSuchKey. An
// ordered extension probe would therefore abort on the first missing
// .png and never reach a real .jpg. Going straight to the right key
// avoids that entirely.
//
// The trailing extension probe remains only as a safety net for rows
// whose media_type is stale or mismatched, and it advances strictly on
// genuine NoSuchKey ("(missing)") errors — never on 403/AccessDenied,
// because masking a real authorization failure as "object not there"
// is unsafe.
func (f *AgentSessionFetcher) FetchImage(ctx context.Context, sourceImageID, mediaType string) ([]byte, string, error) {
	if f.s3 == nil {
		return nil, "", errors.New("agent_session fetcher: S3 mirror not configured")
	}
	sha, ok := strings.CutPrefix(sourceImageID, "sha256:")
	if !ok {
		return nil, "", fmt.Errorf("agent_session fetcher: source_image_id %q does not have sha256: prefix", sourceImageID)
	}
	if len(sha) < 2 {
		return nil, "", fmt.Errorf("agent_session fetcher: sha256 too short in %q", sourceImageID)
	}
	// Preferred key is the exact media-type-derived one, then the
	// common extensions as a fallback. Content-addressed storage
	// means a given hash lives at exactly one key, so the media-type
	// key is authoritative when set; the probe only catches rows
	// whose recorded media_type drifted from write time. A
	// non-missing failure (auth, throttling) surfaces immediately —
	// we never treat 403 as "try the next extension".
	prefix := "agent_session/" + sha[:2] + "/" + sha
	keys := []string{prefix + agentSessionExtensionFor(mediaType), prefix + ".png", prefix + ".jpg", prefix + ".webp", prefix + ".gif", prefix}
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key] {
			continue
		}
		seen[key] = true
		data, ct, err := f.s3.Get(ctx, key)
		if err == nil {
			return data, ct, nil
		}
		if !isMissingKey(err) {
			return nil, "", fmt.Errorf("agent_session fetcher: get %s: %w", key, err)
		}
	}
	return nil, "", fmt.Errorf("agent_session fetcher: no mirrored object at %s.*", prefix)
}

// agentSessionExtensionFor must stay byte-for-byte compatible with
// the retired transcript writer: that code chose the object key when it
// mirrored the image. Do not normalize here.
// A noncanonical media type was written extensionless, and trying a
// normalized extension first would reproduce the 403-before-real-key bug.
func agentSessionExtensionFor(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

// isMissingKey reports whether err is the "object not there"
// signal from S3Mirror.Get (see s3.go's NoSuchKey path). The
// mirror wraps NoSuchKey with a "(missing)" trailer, so string
// match is stable — this classifier is here rather than the
// mirror because it's specific to the multi-extension probe.
func isMissingKey(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "(missing)")
}
