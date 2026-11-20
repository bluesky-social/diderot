package leader

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Lease lives in POD_NAMESPACE when it is set, else in the pod's own
// namespace: a default namespace name would hold the Lease where the pod's
// Role grants nothing, and the replica would never lead.
func TestLeaseNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namespace")
	require.NoError(t, os.WriteFile(path, []byte("knowledge\n"), 0o600))

	namespace, err := leaseNamespace("", path)
	require.NoError(t, err)
	assert.Equal(t, "knowledge", namespace)

	namespace, err = leaseNamespace("override", path)
	require.NoError(t, err)
	assert.Equal(t, "override", namespace, "POD_NAMESPACE wins")

	_, err = leaseNamespace("", filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, err, "set POD_NAMESPACE")

	require.NoError(t, os.WriteFile(path, []byte(" \n"), 0o600))
	_, err = leaseNamespace("", path)
	require.ErrorContains(t, err, "set POD_NAMESPACE")
}
