package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// StaticToken maps the SHA-256 of one bearer token to the identity it
// authenticates. Only the hash is configured, so the file reveals no usable
// credential.
type StaticToken struct {
	// SHA256 is the lowercase or uppercase hex digest of the raw token
	// (`printf %s "$TOKEN" | sha256sum`).
	SHA256 string   `yaml:"sha256"`
	Email  string   `yaml:"email"`
	Groups []string `yaml:"groups"`
}

// Static authenticates bearer tokens listed by hash in operator config
// (DIDEROT_AUTH_MODE=static). Its emails count as verified: the operator
// assigned them.
type Static struct {
	tokens map[[sha256.Size]byte]Principal
}

// LoadStaticTokens reads a YAML (or JSON) document of the form
//
//	tokens:
//	  - sha256: <64 hex chars>
//	    email: alice@example.com
//	    groups: [admins]
func LoadStaticTokens(path string) (*Static, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read static tokens: %w", err)
	}
	return ParseStaticTokens(data)
}

func ParseStaticTokens(data []byte) (*Static, error) {
	var document struct {
		Tokens []StaticToken `yaml:"tokens"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("auth: decode static tokens: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("auth: static tokens must be exactly one YAML document")
	}
	return NewStatic(document.Tokens)
}

func NewStatic(tokens []StaticToken) (*Static, error) {
	if len(tokens) == 0 {
		return nil, errors.New("auth: static mode requires at least one token")
	}
	s := &Static{tokens: make(map[[sha256.Size]byte]Principal, len(tokens))}
	for i, token := range tokens {
		raw, err := hex.DecodeString(strings.TrimSpace(token.SHA256))
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("auth: static token %d: sha256 must be 64 hex characters", i)
		}
		var digest [sha256.Size]byte
		copy(digest[:], raw)
		if _, exists := s.tokens[digest]; exists {
			return nil, fmt.Errorf("auth: static token %d: duplicate sha256", i)
		}
		email, err := ConfiguredEmail(token.Email)
		if err != nil {
			return nil, fmt.Errorf("auth: static token %d: %w", i, err)
		}
		var groups []string
		for _, group := range token.Groups {
			if group = strings.TrimSpace(group); group != "" {
				groups = append(groups, group)
			}
		}
		s.tokens[digest] = Principal{
			Subject: "static:" + email, Email: email, EmailVerified: true,
			Display: email, Groups: groups,
		}
	}
	return s, nil
}

// AnyInGroups reports whether some token's identity is in one of groups.
func (s *Static) AnyInGroups(groups []string) bool {
	for _, principal := range s.tokens {
		for _, group := range principal.Groups {
			if slices.Contains(groups, group) {
				return true
			}
		}
	}
	return false
}

// ConfiguredEmail normalizes an operator-supplied identity email and rejects
// anything but a bare address.
func ConfiguredEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		return "", fmt.Errorf("email %q must be a bare email address", raw)
	}
	return email, nil
}

// Authenticate looks the presented token up by digest. The digest is
// preimage-resistant, so the map lookup leaks nothing useful through timing.
func (s *Static) Authenticate(r *http.Request) (*Principal, error) {
	raw := BearerToken(r)
	if raw == "" {
		return nil, ErrNoCredentials
	}
	p, ok := s.lookup(sha256.Sum256([]byte(raw)))
	if !ok {
		return nil, fmt.Errorf("%w: unknown static token", ErrInvalidCredentials)
	}
	return p, nil
}

func (s *Static) lookup(digest [sha256.Size]byte) (*Principal, bool) {
	if s == nil {
		return nil, false
	}
	p, ok := s.tokens[digest]
	if !ok {
		return nil, false
	}
	return p.clone(), true
}

// lookupHex looks a token up by its hex SHA-256, as a static-token browser
// session records it.
func (s *Static) lookupHex(digestHex string) (*Principal, bool) {
	raw, err := hex.DecodeString(digestHex)
	if err != nil || len(raw) != sha256.Size {
		return nil, false
	}
	return s.lookup([sha256.Size]byte(raw))
}
