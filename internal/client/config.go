// Package client is the diderot client CLI's connection to a Diderot server:
// its saved sign-in, how it signs in, and the MCP calls it makes. The client
// talks only to the server's MCP endpoint.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/oauth2"
)

// Auth kinds a server can ask for, as setup discovered them.
const (
	AuthNone  = "none"  // the server takes no credential (auth mode none)
	AuthToken = "token" // a bearer token the server issued (auth mode static)
	AuthOAuth = "oauth" // OAuth through MCP authorization discovery
)

// Config is what setup saved: the server, how to authenticate to it, and the
// harnesses it configured. It holds credentials, so it is written 0600.
type Config struct {
	URL       string   `json:"url"`
	Auth      string   `json:"auth"`
	Token     string   `json:"token,omitempty"`
	OAuth     *OAuth   `json:"oauth,omitempty"`
	Account   string   `json:"account,omitempty"`
	Harnesses []string `json:"harnesses,omitempty"`
}

// OAuth is a signed-in OAuth client: enough to refresh its token without
// discovery.
type OAuth struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	// HarnessClientID is the client ID harnesses are configured with, empty
	// when the authorization server registers each harness itself.
	HarnessClientID string           `json:"harness_client_id,omitempty"`
	Issuer          string           `json:"issuer"`
	TokenURL        string           `json:"token_url"`
	AuthStyle       oauth2.AuthStyle `json:"auth_style"`
	Token           *oauth2.Token    `json:"token"`
}

// ConfigPath is $XDG_CONFIG_HOME/diderot/client.json, or
// ~/.config/diderot/client.json on every platform when it is unset.
func ConfigPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "diderot", "client.json"), nil
}

// ErrNoConfig means setup has not run.
var ErrNoConfig = errors.New("not set up")

// LoadConfig reads the saved configuration. A missing file is ErrNoConfig.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path is ConfigPath's
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoConfig
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if cfg.URL == "" {
		return nil, fmt.Errorf("read %s: no server URL", path)
	}
	return &cfg, nil
}

// SaveConfig writes cfg atomically, 0600 in a 0700 directory.
func SaveConfig(path string, cfg *Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ") // #nosec G117 -- the saved sign-in is this file's purpose
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(b, '\n'), 0o600)
}

// WriteFileAtomic writes data to path through a temporary file in the same
// directory and a rename, so a reader never sees half a file. The directory
// is created 0700 when missing.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// lockConfig takes an exclusive lock on path's sibling .lock file, waiting
// while another diderot process holds it. Refreshing an OAuth token must hold
// it: identity providers rotate refresh tokens, so two processes refreshing
// at once, or one that drops the rotated token, sign the user out.
func lockConfig(ctx context.Context, path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // beside ConfigPath
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // a file descriptor fits an int
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // as above
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
