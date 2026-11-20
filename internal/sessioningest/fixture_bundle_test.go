package sessioningest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/diderot/internal/sessionfixture"
)

func TestSyntheticFixtureDocumentsMatchDiderotContract(t *testing.T) {
	bundle, err := sessionfixture.Load(filepath.Join("..", "sessionfixture", "testdata", "synthetic-v1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range bundle.Manifest.Sessions {
		identity := Identity{Owner: session.Owner, Client: session.Client, SessionID: session.SessionID}
		_, err := parseSessionDocument(bundle.Documents[session.SessionID], identity, session.Revision, 0)
		if session.Complete && err != nil {
			t.Errorf("complete %s: %v", session.SessionID, err)
		}
		if !session.Complete && (err == nil || !strings.Contains(err.Error(), "marked incomplete")) {
			t.Errorf("incomplete %s: error = %v, want marked incomplete", session.SessionID, err)
		}
	}
}
