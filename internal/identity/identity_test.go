package identity

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/source"
)

// The sources' facts as their packages declare them, which can't be imported
// here; cmd/diderot's source surface golden pins the real ones.
var (
	slackInfo = source.Info{EventSource: "slack", ActorRole: RoleMessageAuthor, IdentityNamespace: func(canonical string) string {
		if strings.HasPrefix(canonical, "slack-bot:") {
			return "slack_bot"
		}
		return "slack_user"
	}}
	sessionInfo = source.Info{EventSource: "agent_session", ActorRole: RoleSessionOwner,
		IdentityNamespace: func(string) string { return "agent_session" }}
	linearInfo = source.Info{EventSource: "linear", ActorRole: RoleIssueCreator,
		IdentityNamespace: func(string) string { return "linear_user" }}
	zendeskInfo = source.Info{EventSource: "zendesk", ActorRole: RoleEventActor,
		IdentityNamespace: func(string) string { return "zendesk_user" }}
)

func TestZendeskIdentityDoesNotPretendToBeATicketAuthor(t *testing.T) {
	actor := FromEvent(zendeskInfo, "Support Agent", "Agent@example.test", "12")
	if actor.Role != RoleEventActor || actor.SourceIdentity != (Key{Namespace: "zendesk_user", ID: "12"}) ||
		actor.Canonical != (Key{Namespace: "email", ID: "Agent@example.test"}) {
		t.Fatalf("unexpected Zendesk identity: %+v", actor)
	}
}

func TestFromEventPreservesCanonicalEmailAndRole(t *testing.T) {
	got := FromEvent(sessionInfo, "alex@example.com", "alex@example.com", "alex@example.com")
	if got.Display != "alex@example.com" {
		t.Fatalf("display = %q", got.Display)
	}
	if got.Canonical != (Key{Namespace: "email", ID: "alex@example.com"}) {
		t.Fatalf("canonical = %+v", got.Canonical)
	}
	if got.SourceIdentity != (Key{Namespace: "agent_session", ID: "alex@example.com"}) {
		t.Fatalf("source identity = %+v", got.SourceIdentity)
	}
	if got.Role != RoleSessionOwner {
		t.Fatalf("role = %q", got.Role)
	}
}

func TestFromEventKeepsNamespacedServiceIdentityOpaque(t *testing.T) {
	got := FromEvent(sessionInfo, "diderot-worker", "svc:diderot@worker.example", "svc:diderot@worker.example")
	if got.Canonical != (Key{Namespace: "agent_session", ID: "svc:diderot@worker.example"}) {
		t.Fatalf("canonical = %+v, want opaque service identity", got.Canonical)
	}
}

func TestFromEventDistinguishesSlackUsersAndBots(t *testing.T) {
	cases := []struct {
		name, canonical, wantNamespace string
	}{
		{"user", "slack:U123", "slack_user"},
		{"bot", "slack-bot:B123", "slack_bot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FromEvent(slackInfo, "sender", tc.canonical, "U123")
			if got.Canonical.Namespace != tc.wantNamespace || got.SourceIdentity.Namespace != tc.wantNamespace {
				t.Fatalf("identity = %+v, want namespace %q", got, tc.wantNamespace)
			}
			if got.Role != RoleMessageAuthor {
				t.Fatalf("role = %q", got.Role)
			}
		})
	}
}

func TestFromEventLinearUnknownDoesNotFabricateIdentity(t *testing.T) {
	got := FromEvent(linearInfo, "", "linear:unknown", "")
	want := Actor{
		Role: RoleIssueCreator,
	}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

func TestFromEventMissingSourceIDUsesEmptyKey(t *testing.T) {
	got := FromEvent(slackInfo, "legacy", "", "")
	if got.SourceIdentity != (Key{}) {
		t.Fatalf("source identity = %+v, want empty key", got.SourceIdentity)
	}
}

// An unregistered source's actor keeps its stored values without a source
// identity, as an unclassified event actor.
func TestFromEventUnregisteredSource(t *testing.T) {
	got := FromEvent(source.Infos{slackInfo}.Of("retired"), "someone", "opaque-value", "X1")
	want := Actor{Display: "someone", Canonical: Key{Namespace: "retired", ID: "opaque-value"}, Role: RoleEventActor}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

func TestActorJSONShapeIsStructured(t *testing.T) {
	got, err := json.Marshal(FromEvent(slackInfo, "Alex", "alex@example.com", "U123"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"display":"Alex","canonical":{"namespace":"email","id":"alex@example.com"},"source_identity":{"namespace":"slack_user","id":"U123"},"role":"message_author"}`
	if string(got) != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
}
