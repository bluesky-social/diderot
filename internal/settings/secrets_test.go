package settings

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestKeyringRoundTripAndRotation(t *testing.T) {
	ring, err := NewKeyring(nil)
	require.NoError(t, err)
	assert.Nil(t, ring, "no keys is no keyring")

	old, err := NewKeyring([][]byte{testKey(1)})
	require.NoError(t, err)
	sealed, err := old.Seal("xoxb-secret", "connector_settings:slack:bot_token")
	require.NoError(t, err)
	assert.NotContains(t, sealed, "xoxb-secret")
	again, err := old.Seal("xoxb-secret", "connector_settings:slack:bot_token")
	require.NoError(t, err)
	assert.NotEqual(t, sealed, again, "every seal uses a fresh nonce")

	value, err := old.Open(sealed, "connector_settings:slack:bot_token")
	require.NoError(t, err)
	assert.Equal(t, "xoxb-secret", value)
	_, err = old.Open(sealed, "connector_settings:slack:app_token")
	assert.Error(t, err, "a ciphertext is bound to its field")

	// Rotation: a new key first encrypts; the old one still decrypts.
	rotated, err := NewKeyring([][]byte{testKey(2), testKey(1)})
	require.NoError(t, err)
	value, err = rotated.Open(sealed, "connector_settings:slack:bot_token")
	require.NoError(t, err)
	assert.Equal(t, "xoxb-secret", value)
	resealed, err := rotated.Seal("xoxb-secret", "aad")
	require.NoError(t, err)
	_, err = old.Open(resealed, "aad")
	assert.Error(t, err, "the new key encrypts")

	dropped, err := NewKeyring([][]byte{testKey(2)})
	require.NoError(t, err)
	_, err = dropped.Open(sealed, "connector_settings:slack:bot_token")
	require.ErrorContains(t, err, "no DIDEROT_CONFIG_KEYS key decrypts")
	assert.NotContains(t, err.Error(), "xoxb")

	_, err = NewKeyring([][]byte{[]byte("short")})
	assert.Error(t, err)
	_, err = old.Open("not base64!", "aad")
	assert.Error(t, err)
}

func TestFingerprint(t *testing.T) {
	assert.Equal(t, "2bb80d53", Fingerprint("secret"))
	assert.Len(t, Fingerprint(""), 8)
}

func TestEnvName(t *testing.T) {
	assert.Equal(t, "DIDEROT_EXAMPLE_TOKEN", EnvName("example", "token"))
	assert.Equal(t, "DIDEROT_MY_SOURCE_API_KEY", EnvName("my_source", "api_key"))
}

// TestReadEnvReadsOnlySecretFields: only a registered secret field's own
// variable is read, trimmed, and an empty one is unset.
func TestReadEnvReadsOnlySecretFields(t *testing.T) {
	registry := NewRegistry(&fakeConnector{name: "example", fields: exampleFields()})
	env := map[string]string{
		"DIDEROT_EXAMPLE_TOKEN": "  tok-env\n",
		"DIDEROT_EXAMPLE_EXTRA": "   ",
		"DIDEROT_EXAMPLE_MODE":  "all",
		"DIDEROT_OTHER_TOKEN":   "tok-other",
	}
	var asked []string
	got := ReadEnv(registry, func(name string) string {
		asked = append(asked, name)
		return env[name]
	})
	assert.Equal(t, map[string]string{"DIDEROT_EXAMPLE_TOKEN": "tok-env"}, got)
	assert.Equal(t, []string{"DIDEROT_EXAMPLE_TOKEN", "DIDEROT_EXAMPLE_EXTRA"}, asked)
}
