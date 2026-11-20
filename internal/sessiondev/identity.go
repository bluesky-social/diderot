// Package sessiondev contains local-only process assembly shared by the
// session development command and its browserless acceptance tests.
package sessiondev

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/auth"
)

const tokenLifetime = 15 * time.Minute

type Identity struct {
	email  string
	groups []string
	key    []byte
	now    func() time.Time
}

type tokenClaims struct {
	Subject string   `json:"sub"`
	Email   string   `json:"email"`
	Groups  []string `json:"groups"`
	Issued  int64    `json:"iat"`
	Expires int64    `json:"exp"`
}

// NewIdentity creates one process-local identity and a short-lived signed
// bearer. The signing key remains only in process memory.
func NewIdentity(email string, groups []string) (*Identity, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil, "", errors.New("session-dev identity email is required")
	}
	canonicalGroups := make([]string, 0, len(groups))
	for _, group := range groups {
		if group = strings.TrimSpace(group); group != "" {
			canonicalGroups = append(canonicalGroups, group)
		}
	}
	if len(canonicalGroups) == 0 {
		return nil, "", errors.New("session-dev identity requires at least one group")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, "", fmt.Errorf("generate session-dev signing key: %w", err)
	}
	identity := &Identity{email: email, groups: canonicalGroups, key: key, now: time.Now}
	token, err := identity.sign()
	if err != nil {
		return nil, "", err
	}
	return identity, token, nil
}

func (i *Identity) sign() (string, error) {
	now := i.now().UTC()
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}{Algorithm: "HS256", Type: "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(tokenClaims{
		Subject: "session-dev:" + i.email,
		Email:   i.email, Groups: append([]string(nil), i.groups...),
		Issued: now.Unix(), Expires: now.Add(tokenLifetime).Unix(),
	})
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	signature := hmac.New(sha256.New, i.key)
	_, _ = signature.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature.Sum(nil)), nil
}

// Authenticate verifies the session-dev bearer without an issuer or network
// call. It backs the focused API routes; the browser side uses
// BrowserAuthenticator.
func (i *Identity) Authenticate(r *http.Request) (*auth.Principal, error) {
	raw := auth.BearerToken(r)
	if raw == "" {
		return nil, auth.ErrNoCredentials
	}
	principal, err := i.verify(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidCredentials, err)
	}
	return principal, nil
}

// BrowserAuthenticator identifies every browser request as the process
// identity. Caller-supplied identity headers have no effect.
func (i *Identity) BrowserAuthenticator() auth.Authenticator {
	return auth.NewFixed(i.principal())
}

func (i *Identity) principal() auth.Principal {
	return auth.Principal{
		Subject: "session-dev:" + i.email, Email: i.email, EmailVerified: true,
		Display: i.email, Groups: append([]string(nil), i.groups...),
	}
}

func (i *Identity) verify(raw string) (*auth.Principal, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid session-dev token")
	}
	signatureBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("invalid session-dev token signature")
	}
	mac := hmac.New(sha256.New, i.key)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if subtle.ConstantTimeCompare(signatureBytes, mac.Sum(nil)) != 1 {
		return nil, errors.New("invalid session-dev token signature")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("invalid session-dev token header")
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if err := decodeExact(headerBytes, &header); err != nil || header.Algorithm != "HS256" || header.Type != "JWT" {
		return nil, errors.New("invalid session-dev token header")
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("invalid session-dev token claims")
	}
	var claims tokenClaims
	if err := decodeExact(claimsBytes, &claims); err != nil {
		return nil, errors.New("invalid session-dev token claims")
	}
	now := i.now().Unix()
	if claims.Email != i.email || claims.Subject != "session-dev:"+i.email || claims.Issued > now || claims.Expires <= now || claims.Expires-claims.Issued > int64(tokenLifetime/time.Second) {
		return nil, errors.New("invalid or expired session-dev token")
	}
	return &auth.Principal{
		Subject:       claims.Subject,
		Email:         claims.Email,
		EmailVerified: true,
		Display:       claims.Email,
		Groups:        append([]string(nil), claims.Groups...),
	}, nil
}

// ListenLoopback refuses hostnames and non-loopback addresses, then returns
// the listener and the exact HTTP origin selected by the kernel.
func ListenLoopback(address string) (net.Listener, string, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, "", fmt.Errorf("session-dev listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, "", fmt.Errorf("session-dev listen address must use a literal loopback IP")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, "", err
	}
	return listener, "http://" + listener.Addr().String(), nil
}

// RequireLoopbackAddress validates a host:port dependency address without
// resolving hostnames, so session-dev cannot write to a remote service.
func RequireLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("session-dev service address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("session-dev service address must use a literal loopback IP")
	}
	return nil
}

// RequireDevDatabase prevents the focused server from resetting an ordinary
// local or production database by mistake.
func RequireDevDatabase(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return errors.New("session-dev database URL must be an absolute postgres URL")
	}
	hostIP := net.ParseIP(parsed.Hostname())
	if hostIP == nil || !hostIP.IsLoopback() {
		return fmt.Errorf("session-dev database host %q must be a literal loopback IP", parsed.Hostname())
	}
	name := path.Base(strings.TrimSuffix(parsed.Path, "/"))
	if !strings.HasSuffix(name, "_session_dev") {
		return fmt.Errorf("session-dev database name %q must end in _session_dev", name)
	}
	return nil
}

// WriteToken creates a mode-0600 token file without following a stale symlink.
func WriteToken(filePath, token string) error {
	directory := pathDir(filePath)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("session-dev runtime directory must be a non-symlink mode-0700 directory")
	}
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(file, token+"\n"); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// RotateToken atomically replaces an existing token file with a freshly
// signed token. The temporary file is created inside the mode-0700 runtime
// directory, and rename replaces a stale destination symlink rather than
// following it.
func (i *Identity) RotateToken(filePath string) error {
	token, err := i.sign()
	if err != nil {
		return err
	}
	directory := pathDir(filePath)
	file, err := os.CreateTemp(directory, ".session-dev-token-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := io.WriteString(file, token+"\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filePath)
}

func pathDir(filePath string) string {
	index := strings.LastIndexAny(filePath, `/\`)
	if index < 0 {
		return "."
	}
	if index == 0 {
		return filePath[:1]
	}
	return filePath[:index]
}

func decodeExact(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing token JSON")
	}
	return nil
}
