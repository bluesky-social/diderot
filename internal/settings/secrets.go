package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EnvName is the deployment variable that sets a connector's secret field:
// DIDEROT_<CONNECTOR>_<FIELD> in upper case, such as DIDEROT_SLACK_BOT_TOKEN.
// When it is set, it is the field's value, and a value pasted on the page is
// ignored.
func EnvName(connector, field string) string {
	return "DIDEROT_" + strings.ToUpper(connector+"_"+field)
}

// SecretEnvNamer is a Definition whose secret fields are set by variables
// other than EnvName's, such as a tool connection's, whose row name is not a
// variable name.
type SecretEnvNamer interface {
	SecretEnv(field string) string
}

// SecretEnv is the deployment variable that sets def's secret field.
func SecretEnv(def Definition, field string) string {
	if namer, ok := def.(SecretEnvNamer); ok {
		return namer.SecretEnv(field)
	}
	return EnvName(def.Name(), field)
}

// SecretState is what connector_settings.secrets stores for one pasted
// field. It never holds a plaintext value.
type SecretState struct {
	// Ciphertext is the AES-256-GCM sealed value.
	Ciphertext string `json:"ciphertext"`
	// Fingerprint is the first 8 hex characters of SHA-256(value).
	Fingerprint string    `json:"fingerprint"`
	SetBy       string    `json:"set_by"`
	SetAt       time.Time `json:"set_at"`
}

// SecretInput changes one secret field: Value is pasted and stored
// encrypted, and Clear removes the saved value. A field left out of a change
// keeps its saved value.
type SecretInput struct {
	Value string `json:"value,omitempty"`
	Clear bool   `json:"clear,omitempty"`
}

// Fingerprint is the short identifier of a secret: the first 8 hex
// characters of its SHA-256.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}

// Secrets is where secret values come from: the deployment's variables, and
// the config keys that seal pasted values. Either may be empty.
type Secrets struct {
	Keys *Keyring
	// Env maps the SecretEnv of each secret field whose variable is set to
	// its value.
	Env map[string]string
}

// ReadEnv reads the variable of every secret field in registry with getenv
// (os.Getenv), keeping those that are set.
func ReadEnv(registry *Registry, getenv func(string) string) map[string]string {
	env := map[string]string{}
	for _, def := range registry.Definitions() {
		for _, field := range def.Fields() {
			if field.Kind != KindSecret {
				continue
			}
			name := SecretEnv(def, field.Key)
			if value := strings.TrimSpace(getenv(name)); value != "" {
				env[name] = value
			}
		}
	}
	return env
}

// env returns the value the deployment's variable sets for def's field, or
// "".
func (s Secrets) env(def Definition, field string) string {
	return s.Env[SecretEnv(def, field)]
}

// secretAAD binds a ciphertext to its connector and field, so a value can't
// be moved to another field.
func secretAAD(connector, field string) string {
	return "connector_settings:" + connector + ":" + field
}

// Keyring encrypts pasted secrets with AES-256-GCM. The first key encrypts;
// every key decrypts, so a new key can be prepended and the old one dropped
// once every secret has been saved again.
type Keyring struct {
	aeads []cipher.AEAD
}

const sealVersion = 1

// NewKeyring builds a keyring from 32-byte keys; none returns nil.
func NewKeyring(keys [][]byte) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ring := &Keyring{}
	for i, key := range keys {
		block, err := aes.NewCipher(key)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("settings: config key %d must be 32 bytes", i+1)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("settings: config key %d: %w", i+1, err)
		}
		ring.aeads = append(ring.aeads, aead)
	}
	return ring, nil
}

// Seal encrypts value with the first key, bound to aad.
func (k *Keyring) Seal(value, aad string) (string, error) {
	aead := k.aeads[0]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(append([]byte{sealVersion}, nonce...), nonce, []byte(value), []byte(aad))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value Seal produced with any key of the ring.
func (k *Keyring) Open(ciphertext, aad string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil || len(raw) == 0 || raw[0] != sealVersion {
		return "", errors.New("settings: malformed stored secret")
	}
	for _, aead := range k.aeads {
		header := 1 + aead.NonceSize()
		if len(raw) < header+aead.Overhead() {
			return "", errors.New("settings: malformed stored secret")
		}
		plaintext, err := aead.Open(nil, raw[1:header], raw[header:], []byte(aad))
		if err == nil {
			return string(plaintext), nil
		}
	}
	return "", errors.New("settings: no DIDEROT_CONFIG_KEYS key decrypts the stored secret; was its key removed?")
}
