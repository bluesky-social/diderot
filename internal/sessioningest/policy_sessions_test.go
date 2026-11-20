package sessioningest

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPolicySessionExclusions(t *testing.T) {
	const heldID = "01900000-0000-7000-8000-000000000002"
	const otherID = "01900000-0000-7000-8000-000000000003"
	policy := DefaultPolicy()
	defaultHash := policy.Hash()
	policy.Exclude.SessionIDs = []string{heldID}
	require.NoError(t, policy.Validate())
	require.NotEqual(t, defaultHash, policy.Hash())
	require.False(t, policy.AdmitChange(heldID, "human", "primary", "codex", "", "owner@example.com", false))
	require.True(t, policy.AdmitChange(otherID, "human", "primary", "codex", "", "owner@example.com", false))
	policy.Exclude.SessionIDs = []string{heldID, otherID}
	reordered := policy
	reordered.Exclude.SessionIDs = []string{otherID, heldID}
	require.Equal(t, policy.Hash(), reordered.Hash())
	require.Equal(t, []string{otherID, heldID}, reordered.Exclude.SessionIDs)
	for _, invalid := range []string{"", "not-a-session-id", "01A07DB2-95E6-7BCD-8330-4AF4A5A78306"} {
		policy.Exclude.SessionIDs = []string{invalid}
		require.Error(t, policy.Validate())
	}
}

func TestSessionExclusionPolicyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("sessionIngest:\n  exclude:\n    sessionIDs: [01900000-0000-7000-8000-000000000002]\n"), 0o600))
	policy, _, err := LoadPolicyFile(path)
	require.NoError(t, err)
	require.Equal(t, []string{"01900000-0000-7000-8000-000000000002"}, policy.Exclude.SessionIDs)
}

func TestSessionExclusionPolicyReplayIsReversible(t *testing.T) {
	store, _ := newWorkerTestStore(t)
	ctx := t.Context()
	const sessionID = "01900000-0000-7000-8000-000000000002"
	change := SessionChange{
		ChangeSeq: 1, SessionID: sessionID, Owner: "owner@example.com", OwnerKind: "human",
		Client: "claude_code", Role: "primary", Complete: true, Revision: "sha256:large",
		RecordCount: 100, UpdatedAt: time.Now().Add(-time.Hour),
	}
	worker := &Worker{Cfg: WorkerConfig{Policy: DefaultPolicy()}, Store: store}
	identity := Identity{Owner: change.Owner, Client: change.Client, SessionID: sessionID}
	for _, excluded := range []bool{false, true, false} {
		worker.Cfg.Policy.Exclude.SessionIDs = nil
		if excluded {
			worker.Cfg.Policy.Exclude.SessionIDs = []string{sessionID}
		}
		worker.Feed = &fakeFeed{pages: []*ChangesPage{{Changes: []SessionChange{change}}}}
		require.NoError(t, worker.Tick(ctx))
		candidate, err := store.LoadCandidate(ctx, identity)
		require.NoError(t, err)
		require.Equal(t, excluded, candidate.ExcludedAt != nil)
		require.False(t, candidate.Deleted)
		require.Equal(t, change.Revision, candidate.Revision)
	}
}
