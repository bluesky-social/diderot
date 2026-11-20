package ask

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/bluesky-social/diderot/internal/urlx"
)

// ReferenceRole controls how an explicit corpus reference participates in a
// turn. Primary references are requirements: if one cannot be resolved and
// read in the current audience, the turn is answered with the unavailable
// response. Supporting references are best-effort context. Format examples
// affect presentation only and never block a factual answer.
type ReferenceRole string

const (
	ReferencePrimary       ReferenceRole = "primary"
	ReferenceSupporting    ReferenceRole = "supporting"
	ReferenceFormatExample ReferenceRole = "format_example"
)

type ReferenceKind string

const (
	ReferenceSlackPermalink ReferenceKind = "slack_permalink"
	ReferenceLinearIssue    ReferenceKind = "linear_issue"
	ReferenceSessionUUID    ReferenceKind = "session_uuid"
	ReferenceEventID        ReferenceKind = "event_id"
	ReferenceChunkID        ReferenceKind = "chunk_id"
	ReferenceZendeskURL     ReferenceKind = "zendesk_url"
)

// CorpusReference is the parsed, source-neutral handle from user input.
// Value is canonical for lookup: Slack uses channel_id + NUL + message_ts;
// other kinds use their source-native identifier. Raw preserves the exact
// spelling for diagnostics and future presentation work.
type CorpusReference struct {
	Kind  ReferenceKind
	Value string
	Raw   string
	Role  ReferenceRole

	// Slack-only parsed fields. Keeping these explicit avoids making every
	// resolver re-parse a URL and makes the audience check unambiguous.
	ChannelID string
	MessageTS string
}

// WithRole returns a copy with the requested role. Parsing defaults to
// primary because a user who explicitly names a corpus handle is asking for
// that exact item; callers with structured intent can downgrade supporting or
// formatting references before preparation.
func (r CorpusReference) WithRole(role ReferenceRole) CorpusReference {
	if role != ReferencePrimary && role != ReferenceSupporting && role != ReferenceFormatExample {
		return r
	}
	r.Role = role
	return r
}

// ParseReferences finds only identifiers with an exact corpus interpretation.
// It deliberately does not turn arbitrary words, URLs, dates, or numbers into
// references: unresolved semantic guesses belong to retrieval, while explicit
// handles must be exact or unavailable.
//
// A bare issue key counts only when its team prefix is in linearTeams, the
// teams whose issues the corpus holds; SHA-256, UTF-8, or CVE-2024 are
// ordinary words, not unavailable references. A nil set admits no bare key.
// Linear URLs are explicit whatever their team.
func ParseReferences(input string, linearTeams map[string]struct{}) []CorpusReference {
	type match struct {
		start int
		end   int
		ref   CorpusReference
	}
	var matches []match
	type byteRange struct{ start, end int }
	var urlRanges []byteRange
	inURL := func(start, end int) bool {
		for _, r := range urlRanges {
			if start < r.end && end > r.start {
				return true
			}
		}
		return false
	}
	occupied := func(start, end int) bool {
		for _, m := range matches {
			if start < m.end && end > m.start {
				return true
			}
		}
		return false
	}
	add := func(start, end int, ref CorpusReference) {
		if start >= end || occupied(start, end) {
			return
		}
		ref.Role = ReferencePrimary
		matches = append(matches, match{start: start, end: end, ref: ref})
	}

	// Parse complete URLs first. urlx handles markdown/Slack wrappers and
	// punctuation, while this pass retains positions for deterministic order.
	for _, rawURL := range urlx.ExtractURLs(input) {
		start := -1
		for searchAt := 0; searchAt < len(input); {
			rel := strings.Index(input[searchAt:], rawURL)
			if rel < 0 {
				break
			}
			occurrenceStart := searchAt + rel
			occurrenceEnd := occurrenceStart + len(rawURL)
			urlRanges = append(urlRanges, byteRange{start: occurrenceStart, end: occurrenceEnd})
			if start < 0 {
				start = occurrenceStart
			}
			searchAt = occurrenceEnd
		}
		if start < 0 {
			continue
		}
		end := start + len(rawURL)
		if channelID, ts, ok := parseSlackPermalink(rawURL); ok {
			add(start, end, CorpusReference{
				Kind: ReferenceSlackPermalink, Value: channelID + "\x00" + ts,
				Raw: rawURL, ChannelID: channelID, MessageTS: ts,
			})
			continue
		}
		if key, ok := parseLinearURL(rawURL); ok {
			add(start, end, CorpusReference{
				Kind: ReferenceLinearIssue, Value: key, Raw: rawURL,
			})
			continue
		}
		if canonical, ok := parseZendeskURL(rawURL); ok {
			add(start, end, CorpusReference{Kind: ReferenceZendeskURL, Value: canonical, Raw: rawURL})
		}
	}

	for _, loc := range labeledIDRE.FindAllStringSubmatchIndex(input, -1) {
		start, end := loc[0], loc[1]
		if inURL(start, end) {
			continue
		}
		labelStart, labelEnd := loc[2], loc[3]
		valueStart, valueEnd := loc[4], loc[5]
		kind := ReferenceEventID
		if strings.HasPrefix(strings.ToLower(input[labelStart:labelEnd]), "chunk") {
			kind = ReferenceChunkID
		}
		value := input[valueStart:valueEnd]
		add(start, end, CorpusReference{Kind: kind, Value: value, Raw: input[start:end]})
	}

	for _, loc := range uuidRE.FindAllStringIndex(input, -1) {
		if inURL(loc[0], loc[1]) || partOfLongerName(input, loc[0], loc[1]) {
			continue
		}
		raw := input[loc[0]:loc[1]]
		add(loc[0], loc[1], CorpusReference{
			Kind: ReferenceSessionUUID, Value: strings.ToLower(raw), Raw: raw,
		})
	}

	for _, loc := range linearKeyRE.FindAllStringIndex(input, -1) {
		if inURL(loc[0], loc[1]) {
			continue
		}
		raw := input[loc[0]:loc[1]]
		team, _, _ := strings.Cut(raw, "-")
		if _, ok := linearTeams[team]; !ok {
			continue
		}
		add(loc[0], loc[1], CorpusReference{
			Kind: ReferenceLinearIssue, Value: strings.ToUpper(raw), Raw: raw,
		})
	}

	sort.SliceStable(matches, func(i, j int) bool { return matches[i].start < matches[j].start })
	seen := make(map[string]struct{}, len(matches))
	out := make([]CorpusReference, 0, len(matches))
	for _, m := range matches {
		key := string(m.ref.Kind) + "\x00" + m.ref.Value
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, m.ref)
	}
	return out
}

var (
	// Slack's archive path is the stable permalink identity. Require an
	// HTTPS/HTTP URL host ending in slack.com; a pasted arbitrary path is not
	// an explicit source permalink.
	slackPathRE          = regexp.MustCompile(`^/archives/([CDGW][A-Z0-9]+)/p([0-9]{16})$`)
	linearPathRE         = regexp.MustCompile(`(?i)(?:^|/)([A-Z][A-Z0-9]{1,15}-[0-9]+)(?:$|[/?#])`)
	linearKeyRE          = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,15}-[0-9]+\b`)
	uuidRE               = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b`)
	labeledIDRE          = regexp.MustCompile(`(?i)\b(event(?:_id)?|chunk(?:_id)?)\s*[:=#]\s*([0-9]+)\b`)
	zendeskTicketPathRE  = regexp.MustCompile(`^/agent/tickets/([1-9][0-9]*)/?$`)
	zendeskArticlePathRE = regexp.MustCompile(`^/hc/([a-zA-Z0-9-]+)/articles/([1-9][0-9]*)(?:-[^/]*)?/?$`)
)

// partOfLongerName reports whether input[start:end] is joined to a longer
// identifier by a hyphen, an underscore or a letter or digit on either side.
// A UUID inside a volume name (pvc-<uuid>) or a pod name names that volume
// or pod, not an agent session: read as a session it could never be found,
// and the turn ended before the model ran. An alert payload is full of them.
func partOfLongerName(input string, start, end int) bool {
	joins := func(b byte) bool {
		return b == '-' || b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	return start > 0 && joins(input[start-1]) || end < len(input) && joins(input[end])
}

func parseZendeskURL(raw string) (string, bool) {
	location, err := url.Parse(raw)
	if err != nil || (location.Scheme != "https" && location.Scheme != "http") || location.User != nil || location.Port() != "" {
		return "", false
	}
	host := strings.ToLower(location.Hostname())
	tenant, ok := strings.CutSuffix(host, ".zendesk.com")
	if !ok || tenant == "" || strings.Contains(tenant, ".") {
		return "", false
	}
	if match := zendeskTicketPathRE.FindStringSubmatch(location.Path); match != nil {
		return "https://" + host + "/agent/tickets/" + match[1], true
	}
	if match := zendeskArticlePathRE.FindStringSubmatch(location.Path); match != nil {
		return "https://" + host + "/hc/" + strings.ToLower(match[1]) + "/articles/" + match[2], true
	}
	return "", false
}

func parseSlackPermalink(raw string) (channelID, messageTS string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "slack.com" && !strings.HasSuffix(host, ".slack.com") {
		return "", "", false
	}
	m := slackPathRE.FindStringSubmatch(u.Path)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2][:10] + "." + m[2][10:], true
}

func parseLinearURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "linear.app" && !strings.HasSuffix(host, ".linear.app") {
		return "", false
	}
	m := linearPathRE.FindStringSubmatch(u.Path)
	if m == nil {
		return "", false
	}
	return strings.ToUpper(m[1]), true
}

// ReferencePrerequisiteState is the boundary between parsing/exact source
// reads and answer generation. It is intentionally transport-neutral so every
// delivery surface can consume the same state without reimplementing reference
// handling.
type ReferencePrerequisiteState struct {
	References     []CorpusReference
	Resolved       []ResolvedReference
	Unavailable    []CorpusReference
	PrimaryBlocked bool
}

// ResolvedReference contains content fetched through an exact reader or the
// source expander. It is model context, never a retrieval hit.
type ResolvedReference struct {
	Reference CorpusReference
	EventID   int64
	Source    string
	URL       string
	Content   string
	// Truncated reports that Content holds only a leading slice: the source
	// cut it, or the resolver capped it at expand's content budget.
	Truncated bool
}

// HasUnavailablePrimary reports whether exact reference preparation must
// block substantive generation.
func (s ReferencePrerequisiteState) HasUnavailablePrimary() bool {
	if s.PrimaryBlocked {
		return true
	}
	for _, ref := range s.Unavailable {
		if ref.Role == ReferencePrimary {
			return true
		}
	}
	return false
}

// ReferencePrerequisiteResolver is the clean seam for surfaces that need
// exact corpus context before generation. The allowedScopes argument is the
// response audience, never the asker's identity; implementations must not
// widen it because a reference contains a link.
type ReferencePrerequisiteResolver interface {
	PrepareReferences(ctx context.Context, refs []CorpusReference, allowedScopes []string) (ReferencePrerequisiteState, error)
}

// ReferenceUnavailableText is deliberately identical for missing, denied,
// deleted, ambiguous, and unsupported exact primary references. It contains
// no identifier or permission detail and is safe for shared Slack history.
const ReferenceUnavailableText = "I couldn't access the referenced corpus item."

const referenceUnavailableReason IncompleteReason = "reference_unavailable"

// ReferenceUnavailableReason is the persisted terminal state for a blocked
// exact reference. It is exported for diagnostics without exposing which
// reference failed.
const ReferenceUnavailableReason = referenceUnavailableReason
