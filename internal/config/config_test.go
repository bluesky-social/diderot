package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requiredEnv sets every variable Load requires, so each test starts from a
// configuration that loads and changes only what it is about.
func requiredEnv(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DIDEROT_DB_URL":              "postgres://test",
		"DIDEROT_PUBLIC_URL":          "https://diderot.example.com",
		"DIDEROT_AUTH_MODE":           "",
		"DIDEROT_OIDC_ISSUER":         "https://idp.example.com",
		"DIDEROT_OIDC_AUDIENCE":       "diderot",
		"DIDEROT_QDRANT_ADDR":         "qdrant.invalid:6334",
		"DIDEROT_ADMIN_GROUPS":        "diderot-admins",
		"DIDEROT_SELF_SERVICE_GROUPS": "diderot-users",
		"POD_NAMESPACE":               "",
	} {
		t.Setenv(key, value)
	}
}

// TestLoad_NeedsNoModel: the server starts with no model configured; models
// are chosen on the Models page.
func TestLoad_NeedsNoModel(t *testing.T) {
	requiredEnv(t)
	_, err := Load()
	require.NoError(t, err)
}

func TestLoad_RequiresDBURL(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_DB_URL", "")
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DIDEROT_DB_URL")
}

func TestLoad_ValidConfig(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_DB_URL", "postgres://diderot:diderot@localhost:15432/diderot?sslmode=disable")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://diderot:diderot@localhost:15432/diderot?sslmode=disable", cfg.DBURL)
	assert.Equal(t, []string{"diderot-admins"}, cfg.AdminGroups)
	assert.Equal(t, []string{"diderot-users"}, cfg.SelfServiceGroups)
	assert.Equal(t, "https://diderot.example.com", cfg.PublicURL)
	assert.Equal(t, uint32(1), cfg.QdrantShardNumber)
	assert.Equal(t, uint32(1), cfg.QdrantReplicationFactor)
}

func TestLoad_AuthorizationPolicyPath(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_AUTHORIZATION_POLICY_PATH", "/etc/diderot/authorization/policy.yaml")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "/etc/diderot/authorization/policy.yaml", cfg.AuthorizationPolicyPath)
}

// Without Qdrant nothing is searchable, so the server refuses to start rather
// than run with retrieval, Ask, and the embedder off.
func TestLoad_RequiresQdrant(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_QDRANT_ADDR", " ")
	_, err := Load()
	require.ErrorContains(t, err, "DIDEROT_QDRANT_ADDR is required")

	t.Setenv("DIDEROT_QDRANT_ADDR", " qdrant:6334 ")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "qdrant:6334", cfg.QdrantAddr)
}

func TestLoad_QdrantTopology(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_QDRANT_SHARD_NUMBER", "5")
	t.Setenv("DIDEROT_QDRANT_REPLICATION_FACTOR", "2")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, uint32(5), cfg.QdrantShardNumber)
	assert.Equal(t, uint32(2), cfg.QdrantReplicationFactor)
}

func TestLoad_QdrantTopologyRejectsInvalidValues(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_QDRANT_SHARD_NUMBER", "zero")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DIDEROT_QDRANT_SHARD_NUMBER")
}

// TestLoad_QdrantTLS: TLS is off unless DIDEROT_QDRANT_TLS turns it on, and
// a CA file replaces the system's CAs only when TLS is on and the file holds
// a certificate.
func TestLoad_QdrantTLS(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_QDRANT_TLS", "")
	t.Setenv("DIDEROT_QDRANT_CA_FILE", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.QdrantTLS)
	assert.Nil(t, cfg.QdrantRootCAs)

	t.Setenv("DIDEROT_QDRANT_TLS", "maybe")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_QDRANT_TLS")

	t.Setenv("DIDEROT_QDRANT_TLS", "true")
	cfg, err = Load()
	require.NoError(t, err)
	assert.True(t, cfg.QdrantTLS)
	assert.Nil(t, cfg.QdrantRootCAs, "no CA file trusts the system's CAs")

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, testCACertificatePEM(t), 0o600))
	t.Setenv("DIDEROT_QDRANT_CA_FILE", caFile)
	cfg, err = Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.QdrantRootCAs)

	t.Setenv("DIDEROT_QDRANT_TLS", "false")
	_, err = Load()
	require.EqualError(t, err, "DIDEROT_QDRANT_CA_FILE requires DIDEROT_QDRANT_TLS=true")

	t.Setenv("DIDEROT_QDRANT_TLS", "true")
	t.Setenv("DIDEROT_QDRANT_CA_FILE", filepath.Join(t.TempDir(), "missing.pem"))
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_QDRANT_CA_FILE cannot be read")

	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a certificate\n"), 0o600))
	t.Setenv("DIDEROT_QDRANT_CA_FILE", notPEM)
	_, err = Load()
	require.EqualError(t, err, "DIDEROT_QDRANT_CA_FILE "+notPEM+" holds no PEM certificates")
}

// testCACertificatePEM makes a self-signed CA certificate at test time, so no
// key material is checked in.
func testCACertificatePEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "diderot test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestLoad_PublicURL(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_PUBLIC_URL", "https://diderot.example/")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "https://diderot.example", cfg.PublicURL)

	t.Setenv("DIDEROT_PUBLIC_URL", "https://diderot.example/not-an-origin")
	_, err = Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DIDEROT_PUBLIC_URL")
}

// TestLoad_RefusesRemovedEnv: a removed variable stops startup, naming every
// one that is set and where each setting went, and never a value. No release
// has removed a variable yet, so the list is filled here with stand-ins: one
// moved to a page, two to another, one renamed, one dropped.
func TestLoad_RefusesRemovedEnv(t *testing.T) {
	requiredEnv(t)
	require.Empty(t, removedEnv, "a removed variable needs its own case here and a row in docs/configuration.md")
	removedEnv = []struct{ page, name string }{
		{"/admin/connectors/example", "DIDEROT_TEST_MOVED_ONE"},
		{"/admin/settings/models", "DIDEROT_TEST_MOVED_TWO"},
		{"/admin/settings/models", "DIDEROT_TEST_MOVED_THREE"},
		{"DIDEROT_TEST_NEW_NAME", "DIDEROT_TEST_RENAMED"},
		{"", "DIDEROT_TEST_DROPPED"},
	}
	t.Cleanup(func() { removedEnv = nil })

	for _, removed := range removedEnv {
		t.Setenv(removed.name, "")
	}
	_, err := Load()
	require.NoError(t, err, "empty is unset")

	t.Setenv("DIDEROT_TEST_MOVED_ONE", "deployment-value")
	_, err = Load()
	require.EqualError(t, err, "DIDEROT_TEST_MOVED_ONE: removed; set them at /admin/connectors/example instead (docs/configuration.md#removed). Unset them to start")
	assert.NotContains(t, err.Error(), "deployment-value")

	// Variables that moved to the same page name it once.
	t.Setenv("DIDEROT_TEST_MOVED_TWO", "x")
	t.Setenv("DIDEROT_TEST_MOVED_THREE", "x")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_TEST_MOVED_ONE, DIDEROT_TEST_MOVED_TWO, DIDEROT_TEST_MOVED_THREE: removed; set them at /admin/connectors/example and /admin/settings/models instead (docs/configuration.md#removed)")
	for _, name := range []string{"DIDEROT_TEST_MOVED_ONE", "DIDEROT_TEST_MOVED_TWO", "DIDEROT_TEST_MOVED_THREE"} {
		t.Setenv(name, "")
	}

	t.Setenv("DIDEROT_TEST_RENAMED", "x")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_TEST_RENAMED: removed; set them at DIDEROT_TEST_NEW_NAME instead")
	t.Setenv("DIDEROT_TEST_RENAMED", "")

	// A dropped variable is refused whatever it is set to: "false" is as
	// stale as "true".
	for _, value := range []string{"true", "false"} {
		t.Setenv("DIDEROT_TEST_DROPPED", value)
		_, err = Load()
		require.EqualError(t, err, "DIDEROT_TEST_DROPPED: removed; nothing replaces them (docs/configuration.md#removed). Unset them to start")
	}

	for _, removed := range removedEnv {
		t.Setenv(removed.name, "x")
	}
	_, err = Load()
	for _, removed := range removedEnv {
		assert.Contains(t, err.Error(), removed.name)
		assert.Contains(t, err.Error(), removed.page)
	}
}

func TestLoad_ConfigKeys(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_CONFIG_KEYS", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.ConfigKeys)

	t.Setenv("DIDEROT_CONFIG_KEYS", "/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v7+/v4=, AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Len(t, cfg.ConfigKeys, 2)

	t.Setenv("DIDEROT_CONFIG_KEYS", "c2hvcnQta2V5")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_CONFIG_KEYS: key 1 must be 32 bytes")

	// The keys can come from a file instead, but not from both.
	path := filepath.Join(t.TempDir(), "config-keys")
	require.NoError(t, os.WriteFile(path, []byte("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=\n"), 0o600))
	t.Setenv("DIDEROT_CONFIG_KEYS_FILE", path)
	_, err = Load()
	require.ErrorContains(t, err, "set DIDEROT_CONFIG_KEYS or DIDEROT_CONFIG_KEYS_FILE, not both")
	t.Setenv("DIDEROT_CONFIG_KEYS", "")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Len(t, cfg.ConfigKeys, 1)
	t.Setenv("DIDEROT_CONFIG_KEYS_FILE", filepath.Join(t.TempDir(), "missing"))
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_CONFIG_KEYS_FILE")
}

// The browser UI checks request origins against the public URL, and every
// auth mode mounts it.
func TestLoad_PublicURLRequired(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"oidc":   {},
		"static": {"DIDEROT_AUTH_MODE": "static", "DIDEROT_AUTH_STATIC_TOKENS_FILE": "/etc/diderot/tokens.yaml"},
		"none":   {"DIDEROT_AUTH_MODE": "none", "DIDEROT_AUTH_NONE_EMAIL": "dev@example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			requiredEnv(t)
			for key, value := range env {
				t.Setenv(key, value)
			}
			t.Setenv("DIDEROT_PUBLIC_URL", "")
			_, err := Load()
			require.ErrorContains(t, err, "DIDEROT_PUBLIC_URL is required")

			t.Setenv("DIDEROT_PUBLIC_URL", "https://diderot.example")
			cfg, err := Load()
			require.NoError(t, err)
			assert.Equal(t, "https://diderot.example", cfg.PublicURL)
		})
	}
}

func TestLoad_AskReadinessDefaults(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_ASK_READINESS_TIMEOUT", "")
	t.Setenv("DIDEROT_ASK_READINESS_INTERVAL", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, cfg.AskReadinessTimeout, "default readiness probe timeout")
	assert.Equal(t, 30*time.Second, cfg.AskReadinessInterval, "default readiness probe interval")
}

func TestLoad_AskReadinessOverrides(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_ASK_READINESS_TIMEOUT", "2s")
	t.Setenv("DIDEROT_ASK_READINESS_INTERVAL", "45s")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 2*time.Second, cfg.AskReadinessTimeout)
	assert.Equal(t, 45*time.Second, cfg.AskReadinessInterval)
}

func TestLoad_AskReadinessInvalidDurationsFallBackToDefaults(t *testing.T) {
	requiredEnv(t)
	// getenvDuration is lenient by convention across the config surface:
	// an unparsable value falls back to the default rather than failing
	// load. The readiness probe bounds are safety rails, not hard gates.
	t.Setenv("DIDEROT_ASK_READINESS_TIMEOUT", "bogus")
	t.Setenv("DIDEROT_ASK_READINESS_INTERVAL", "-5s")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, cfg.AskReadinessTimeout)
	assert.Equal(t, 30*time.Second, cfg.AskReadinessInterval)
}

// Which groups administer Diderot, and which manage their own sessions,
// differ at every organization, so neither list has a default. A value with
// no group in it admits no one.
func TestLoad_GroupsAreRequired(t *testing.T) {
	for _, name := range []string{"DIDEROT_ADMIN_GROUPS", "DIDEROT_SELF_SERVICE_GROUPS"} {
		t.Run(name, func(t *testing.T) {
			requiredEnv(t)
			t.Setenv(name, " ")
			_, err := Load()
			require.ErrorContains(t, err, name+" is required")

			t.Setenv(name, ",")
			cfg, err := Load()
			require.NoError(t, err)
			groups := map[string][]string{"DIDEROT_ADMIN_GROUPS": cfg.AdminGroups, "DIDEROT_SELF_SERVICE_GROUPS": cfg.SelfServiceGroups}
			assert.Empty(t, groups[name], "a lone comma admits no one")
		})
	}
}

func TestLoad_AdminGroupsOverride(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_ADMIN_GROUPS", "ops, diderot-admins")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"ops", "diderot-admins"}, cfg.AdminGroups)
}

func TestLoad_SelfServiceGroupsOverride(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_SELF_SERVICE_GROUPS", "everyone, engineering")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"everyone", "engineering"}, cfg.SelfServiceGroups)
}

func TestLoad_NamespaceDefaultsAndOverrides(t *testing.T) {
	requiredEnv(t)

	// Unset is the pod's own namespace, which the Lease election reads from
	// the pod's service account, not a namespace this install may not use.
	t.Setenv("POD_NAMESPACE", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.Namespace)
	assert.Equal(t, "diderot-leader", cfg.LeaseName)

	// Override.
	t.Setenv("POD_NAMESPACE", "diderot-staging")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "diderot-staging", cfg.Namespace)
}

// A deployment that sets neither DIDEROT_DEPLOYMENT nor
// DIDEROT_LEADER_BACKEND and pins DIDEROT_LEADER_ELECTION=true gets the
// Kubernetes Lease.
func TestLoad_DeploymentAndLeaderBackend(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_DEPLOYMENT", "")
	t.Setenv("DIDEROT_LEADER_BACKEND", "")
	t.Setenv("DIDEROT_LEADER_ELECTION", "true")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, DeploymentKubernetes, cfg.Deployment)
	assert.True(t, cfg.LeaderElection)
	assert.Equal(t, LeaderBackendKubernetes, cfg.LeaderBackend)

	t.Setenv("DIDEROT_LEADER_BACKEND", "postgres")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, LeaderBackendPostgres, cfg.LeaderBackend, "a Kubernetes deployment may still elect through Postgres")

	t.Setenv("DIDEROT_DEPLOYMENT", "standalone")
	t.Setenv("DIDEROT_LEADER_BACKEND", "")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, DeploymentStandalone, cfg.Deployment)
	assert.Equal(t, LeaderBackendPostgres, cfg.LeaderBackend, "standalone defaults to the Postgres backend")

	t.Setenv("DIDEROT_LEADER_BACKEND", "kubernetes")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_LEADER_BACKEND=kubernetes needs DIDEROT_DEPLOYMENT=kubernetes")

	t.Setenv("DIDEROT_LEADER_BACKEND", "etcd")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_LEADER_BACKEND must be")

	t.Setenv("DIDEROT_LEADER_BACKEND", "")
	t.Setenv("DIDEROT_DEPLOYMENT", "laptop")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_DEPLOYMENT must be")
}

// Object storage carries no AWS-specific defaults: the region is the
// operator's (or the AWS SDK's) and the endpoint may name any S3-compatible
// store.
func TestLoad_ImagesObjectStorage(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_IMAGES_BUCKET", "")
	t.Setenv("DIDEROT_IMAGES_REGION", "")
	t.Setenv("DIDEROT_IMAGES_ENDPOINT", "")
	t.Setenv("DIDEROT_IMAGES_PATH_STYLE", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.ImagesBucket)
	assert.Empty(t, cfg.ImagesRegion, "no hard-coded region fallback")
	assert.Empty(t, cfg.ImagesEndpoint)
	assert.False(t, cfg.ImagesPathStyle)

	t.Setenv("DIDEROT_IMAGES_BUCKET", "diderot-files")
	t.Setenv("DIDEROT_IMAGES_REGION", "us-east-1")
	t.Setenv("DIDEROT_IMAGES_ENDPOINT", "http://minio:9000")
	t.Setenv("DIDEROT_IMAGES_PATH_STYLE", "true")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "diderot-files", cfg.ImagesBucket)
	assert.Equal(t, "us-east-1", cfg.ImagesRegion)
	assert.Equal(t, "http://minio:9000", cfg.ImagesEndpoint)
	assert.True(t, cfg.ImagesPathStyle)

	for _, endpoint := range []string{
		"minio:9000",
		"ftp://minio:9000",
		"http://key:secret@minio:9000",
		"https://example.r2.cloudflarestorage.com?x=1",
		"https://example.r2.cloudflarestorage.com#frag",
	} {
		t.Setenv("DIDEROT_IMAGES_ENDPOINT", endpoint)
		_, err = Load()
		require.ErrorContains(t, err, "DIDEROT_IMAGES_ENDPOINT", endpoint)
	}
	t.Setenv("DIDEROT_IMAGES_ENDPOINT", "")

	t.Setenv("DIDEROT_IMAGES_PATH_STYLE", "sometimes")
	_, err = Load()
	require.ErrorContains(t, err, "DIDEROT_IMAGES_PATH_STYLE")
}

func TestLoad_SessionIngestDefaults(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_SESSION_INGEST_TICK", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 30_000_000_000, int(cfg.SessionIngestTick), "default tick 30s")
}

func TestLoad_SessionIngestSettings(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_SESSION_INGEST_TICK", "1m")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 60_000_000_000, int(cfg.SessionIngestTick))
}

func TestLoad_SessionIngestConcurrency(t *testing.T) {
	requiredEnv(t)

	// Unset → default 1 (serial), same as the in-package worker default.
	t.Setenv("DIDEROT_SESSION_INGEST_CONCURRENCY", "")
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.SessionIngestConcurrency, "default concurrency must be 1")

	// Explicit override.
	t.Setenv("DIDEROT_SESSION_INGEST_CONCURRENCY", "4")
	cfg, err = Load()
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.SessionIngestConcurrency)
}

func TestLoad_SessionIngestChunkConcurrency(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value string
		want  int
	}{
		{name: "default", want: 20},
		{name: "override", value: "7", want: 7},
		{name: "zero", value: "0"},
		{name: "negative", value: "-1"},
		{name: "invalid", value: "many"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			requiredEnv(t)
			t.Setenv("DIDEROT_SESSION_INGEST_CHUNK_CONCURRENCY", testCase.value)
			cfg, err := Load()
			if testCase.want == 0 {
				require.ErrorContains(t, err, "DIDEROT_SESSION_INGEST_CHUNK_CONCURRENCY must be a positive integer")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, testCase.want, cfg.SessionIngestChunkConcurrency)
		})
	}
}

// The /mcp bearer is trimmed like every other secret.
func TestLoad_GatewayAPIKey(t *testing.T) {
	requiredEnv(t)
	t.Setenv("DIDEROT_GATEWAY_API_KEY", " test-key ")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "test-key", cfg.GatewayAPIKey)
}
