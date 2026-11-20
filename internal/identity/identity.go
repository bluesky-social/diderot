// Package identity contains the single model-facing actor projection used by
// every corpus tool. Event storage deliberately remains source-native; this
// package is the boundary where those fields become an attributed identity.
package identity

import (
	"strings"

	"github.com/bluesky-social/diderot/internal/source"
)

// Key is one namespaced identity key. Namespace is deliberately explicit:
// opaque ids from different systems must never be compared as if they shared
// a namespace.
type Key struct {
	Namespace string `json:"namespace"`
	ID        string `json:"id"`
}

// Actor is the model-facing actor contract. Display is presentation only;
// canonical is the cross-source key when one exists, and source_identity is
// the source's own stable identity. Empty values mean the source did not
// provide that fact.
type Actor struct {
	Display        string `json:"display"`
	Canonical      Key    `json:"canonical"`
	SourceIdentity Key    `json:"source_identity"`
	Role           string `json:"role"`
}

const (
	RoleMessageAuthor = "message_author"
	RoleSessionOwner  = "session_owner"
	RoleLastEditor    = "last_editor"
	RoleIssueCreator  = "issue_creator"
	RoleEventActor    = "event_actor"
)

// FromEvent projects the source-native actor columns persisted on an event,
// with the role and source-identity namespace its source declares (see
// source.Infos.Of). It never derives a display name from an opaque id and
// never normalizes an email: the canonical email is emitted byte-for-byte as
// stored.
func FromEvent(info source.Info, display, canonical, sourceID string) Actor {
	a := Actor{
		Display: display,
		Role:    info.ActorRole,
	}
	if a.Role == "" {
		// An event of a source this build doesn't register is still
		// represented without inventing a namespace or identity. The role
		// remains explicit so callers can tell this is an unclassified
		// event actor.
		a.Role = RoleEventActor
	}
	if info.IdentityNamespace != nil {
		a.SourceIdentity = sourceKey(info.IdentityNamespace(canonical), sourceID)
	}

	a.Canonical = canonicalKey(info.EventSource, canonical)
	if info.EventSource == "linear" && sourceID == "" && canonical == "linear:unknown" {
		// Older Linear rows used a synthetic sentinel when the API omitted
		// creator data. Absence is not an identity; keep only the display
		// placeholder while the cleanup migration removes the stored sentinel.
		a.Canonical = Key{}
	}
	return a
}

func sourceKey(namespace, id string) Key {
	if id == "" {
		return Key{}
	}
	return Key{Namespace: namespace, ID: id}
}

func canonicalKey(source, value string) Key {
	if value == "" {
		return Key{}
	}
	for _, prefix := range []struct {
		prefix    string
		namespace string
	}{
		{"slack-bot:", "slack_bot"},
		{"slack:", "slack_user"},
		{"notion-bot:", "notion_bot"},
		{"notion:", "notion_user"},
		{"linear:", "linear_user"},
		{"zendesk:", "zendesk_user"},
		{"discord:", "discord_user"},
	} {
		if strings.HasPrefix(value, prefix.prefix) {
			return Key{Namespace: prefix.namespace, ID: strings.TrimPrefix(value, prefix.prefix)}
		}
	}
	// Only unprefixed values with an email-shaped local/domain pair
	// are canonical emails. Values such as svc:worker@example.com are
	// service identities, not people, and remain source-native opaque
	// values.
	if at := strings.IndexByte(value, '@'); at > 0 &&
		!strings.Contains(value[:at], ":") && strings.ContainsRune(value[at+1:], '.') {
		return Key{Namespace: "email", ID: value}
	}
	// A source-native value without a recognized sentinel is retained as
	// opaque data. This is preferable to guessing that it is an email or
	// fabricating a cross-source identity.
	if source != "" {
		return Key{Namespace: source, ID: value}
	}
	return Key{ID: value}
}
