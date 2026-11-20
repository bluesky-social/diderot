package config

import (
	"encoding/base64"
	"fmt"
	"net/mail"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	AuthModeOIDC   = "oidc"
	AuthModeStatic = "static"
	AuthModeNone   = "none"

	BrowserAuthTrustedProxy = "trusted-proxy"
	BrowserAuthOIDC         = "oidc"

	defaultBrowserSessionTTL  = 12 * time.Hour
	defaultBrowserSessionIdle = time.Hour
	maxBrowserSessionTTL      = 7 * 24 * time.Hour
)

// browserOIDCEnv configures only the built-in OIDC browser sign-in. Setting
// any of them without DIDEROT_BROWSER_AUTH=oidc is ambiguous and fails Load.
var browserOIDCEnv = []string{
	"DIDEROT_BROWSER_OIDC_CLIENT_ID", "DIDEROT_BROWSER_OIDC_CLIENT_SECRET",
}

// browserSessionEnv configures the browser session cookie. The lifetimes and
// the cookie flag mean nothing without the keys, so setting them alone fails
// Load.
var browserSessionEnv = []string{
	"DIDEROT_BROWSER_SESSION_TTL", "DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT", "DIDEROT_BROWSER_SESSION_INSECURE_COOKIE",
}

func loadAuth(cfg *Config) error {
	cfg.AuthMode = strings.TrimSpace(getenvDefault("DIDEROT_AUTH_MODE", AuthModeOIDC))
	switch cfg.AuthMode {
	case AuthModeOIDC:
	case AuthModeStatic:
		cfg.StaticTokensFile = strings.TrimSpace(os.Getenv("DIDEROT_AUTH_STATIC_TOKENS_FILE"))
		if cfg.StaticTokensFile == "" {
			return fmt.Errorf("DIDEROT_AUTH_MODE=static requires DIDEROT_AUTH_STATIC_TOKENS_FILE")
		}
	case AuthModeNone:
		cfg.NoneEmail = strings.ToLower(strings.TrimSpace(os.Getenv("DIDEROT_AUTH_NONE_EMAIL")))
		address, err := mail.ParseAddress(cfg.NoneEmail)
		if err != nil || address.Address != cfg.NoneEmail {
			return fmt.Errorf("DIDEROT_AUTH_MODE=none requires DIDEROT_AUTH_NONE_EMAIL to be a bare email address")
		}
	default:
		return fmt.Errorf("DIDEROT_AUTH_MODE: invalid value %q (must be %q, %q, or %q)",
			cfg.AuthMode, AuthModeOIDC, AuthModeStatic, AuthModeNone)
	}

	cfg.OIDCIssuer = strings.TrimSpace(os.Getenv("DIDEROT_OIDC_ISSUER"))
	cfg.OIDCJWKSURL = strings.TrimSpace(os.Getenv("DIDEROT_OIDC_JWKS_URL"))
	cfg.OIDCAudience = strings.TrimSpace(os.Getenv("DIDEROT_OIDC_AUDIENCE"))
	cfg.OIDCGroupsClaim = strings.TrimSpace(getenvDefault("DIDEROT_OIDC_GROUPS_CLAIM", "groups"))
	cfg.OIDCEmailClaim = strings.TrimSpace(getenvDefault("DIDEROT_OIDC_EMAIL_CLAIM", "email"))
	if cfg.OIDCGroupsClaim == "" || cfg.OIDCEmailClaim == "" {
		return fmt.Errorf("DIDEROT_OIDC_GROUPS_CLAIM and DIDEROT_OIDC_EMAIL_CLAIM must not be blank")
	}
	cfg.OIDCRequireEmailVerified = true
	if raw := strings.TrimSpace(os.Getenv("DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED")); raw != "" {
		require, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("DIDEROT_OIDC_REQUIRE_EMAIL_VERIFIED: %w", err)
		}
		cfg.OIDCRequireEmailVerified = require
	}
	// Without an issuer and an audience nothing could sign in to the API or
	// the browser UI, so a server missing them has no way to be used.
	if cfg.AuthMode == AuthModeOIDC {
		if cfg.OIDCIssuer == "" {
			return fmt.Errorf("DIDEROT_AUTH_MODE is oidc (the default), which needs DIDEROT_OIDC_ISSUER and DIDEROT_OIDC_AUDIENCE. " +
				"To use API tokens from a file instead, set DIDEROT_AUTH_MODE=static and DIDEROT_AUTH_STATIC_TOKENS_FILE (docs/auth.md)")
		}
		if cfg.OIDCAudience == "" {
			return fmt.Errorf("DIDEROT_OIDC_AUDIENCE is required with DIDEROT_OIDC_ISSUER (docs/auth.md)")
		}
	}

	domains, err := parseEmailDomains(os.Getenv("DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS"))
	if err != nil {
		return err
	}
	cfg.OIDCAllowedEmailDomains = domains
	cfg.OIDCHostedDomainClaim = strings.TrimSpace(os.Getenv("DIDEROT_OIDC_HOSTED_DOMAIN_CLAIM"))
	if cfg.OIDCHostedDomainClaim != "" && len(domains) == 0 {
		return fmt.Errorf("DIDEROT_OIDC_HOSTED_DOMAIN_CLAIM requires DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS")
	}

	if err := loadTrustedProxy(cfg); err != nil {
		return err
	}
	if err := loadBrowserAuth(cfg); err != nil {
		return err
	}
	// Only OIDC identities are checked; an allowlist nothing enforces must
	// not look like protection.
	usesOIDC := cfg.AuthMode == AuthModeOIDC || cfg.BrowserAuth == BrowserAuthOIDC
	if len(domains) > 0 && !usesOIDC {
		return fmt.Errorf("DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS applies only to OIDC identities: DIDEROT_AUTH_MODE=oidc or DIDEROT_BROWSER_AUTH=oidc")
	}
	return loadOIDCScopes(cfg, usesOIDC)
}

// loadOIDCScopes reads the scopes that make the IdP release the identity
// claims. Google rejects offline_access and groups, so neither is a
// default; an IdP that releases groups only for a "groups" scope needs it
// listed.
func loadOIDCScopes(cfg *Config, usesOIDC bool) error {
	raw := strings.TrimSpace(os.Getenv("DIDEROT_OIDC_SCOPES"))
	if !usesOIDC {
		if raw != "" {
			return fmt.Errorf("DIDEROT_OIDC_SCOPES applies only to OIDC sign-in: DIDEROT_AUTH_MODE=oidc or DIDEROT_BROWSER_AUTH=oidc")
		}
		return nil
	}
	if raw == "" {
		raw = "openid email profile"
	}
	cfg.OIDCScopes = strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if !slices.Contains(cfg.OIDCScopes, "openid") {
		return fmt.Errorf("DIDEROT_OIDC_SCOPES must include openid")
	}
	return nil
}

func loadTrustedProxy(cfg *Config) error {
	for _, raw := range strings.Split(os.Getenv("DIDEROT_TRUSTED_PROXY_CIDRS"), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		prefix, err := parsePeerPrefix(raw)
		if err != nil {
			return fmt.Errorf("DIDEROT_TRUSTED_PROXY_CIDRS: %w", err)
		}
		cfg.TrustedProxyCIDRs = append(cfg.TrustedProxyCIDRs, prefix)
	}
	cfg.TrustedProxyEmailHeader = strings.TrimSpace(os.Getenv("DIDEROT_TRUSTED_PROXY_EMAIL_HEADER"))
	cfg.TrustedProxyGroupsHeader = strings.TrimSpace(os.Getenv("DIDEROT_TRUSTED_PROXY_GROUPS_HEADER"))
	cfg.TrustedProxyGroupsDelimiter = os.Getenv("DIDEROT_TRUSTED_PROXY_GROUPS_DELIMITER")
	cfg.TrustedProxyMCPEmailHeader = strings.TrimSpace(os.Getenv("DIDEROT_TRUSTED_PROXY_MCP_EMAIL_HEADER"))
	if cfg.TrustedProxyMCPEmailHeader == "" {
		cfg.TrustedProxyMCPEmailHeader = cfg.TrustedProxyEmailHeader
	}
	if len(cfg.TrustedProxyCIDRs) == 0 {
		return nil
	}
	if cfg.TrustedProxyEmailHeader == "" {
		return fmt.Errorf("DIDEROT_TRUSTED_PROXY_CIDRS requires DIDEROT_TRUSTED_PROXY_EMAIL_HEADER")
	}
	if cfg.TrustedProxyGroupsHeader != "" && cfg.TrustedProxyGroupsDelimiter == "" {
		return fmt.Errorf("DIDEROT_TRUSTED_PROXY_GROUPS_HEADER requires DIDEROT_TRUSTED_PROXY_GROUPS_DELIMITER")
	}
	return nil
}

// loadBrowserAuth selects the browser's external identity source and loads
// the session settings every sign-in method shares. It runs after the OIDC,
// trusted-proxy, and public-URL settings it depends on.
func loadBrowserAuth(cfg *Config) error {
	cfg.BrowserAuth = strings.TrimSpace(os.Getenv("DIDEROT_BROWSER_AUTH"))
	switch cfg.BrowserAuth {
	case "", BrowserAuthTrustedProxy, BrowserAuthOIDC:
	default:
		return fmt.Errorf("DIDEROT_BROWSER_AUTH: invalid value %q (must be %q or %q)",
			cfg.BrowserAuth, BrowserAuthTrustedProxy, BrowserAuthOIDC)
	}
	if cfg.AuthMode == AuthModeNone {
		if cfg.BrowserAuth != "" {
			return fmt.Errorf("DIDEROT_BROWSER_AUTH must be unset with DIDEROT_AUTH_MODE=none, which identifies every browser as DIDEROT_AUTH_NONE_EMAIL")
		}
		for _, key := range append([]string{"DIDEROT_BROWSER_SESSION_KEYS", "DIDEROT_BROWSER_SESSION_KEYS_FILE"}, browserSessionEnv...) {
			if strings.TrimSpace(os.Getenv(key)) != "" {
				return fmt.Errorf("%s must be unset with DIDEROT_AUTH_MODE=none, which needs no browser sign-in", key)
			}
		}
	}
	if cfg.BrowserAuth == BrowserAuthTrustedProxy && len(cfg.TrustedProxyCIDRs) == 0 {
		return fmt.Errorf("DIDEROT_BROWSER_AUTH=trusted-proxy requires DIDEROT_TRUSTED_PROXY_CIDRS")
	}
	if err := loadBrowserSessions(cfg); err != nil {
		return err
	}
	if cfg.BrowserAuth != BrowserAuthOIDC {
		for _, key := range browserOIDCEnv {
			if strings.TrimSpace(os.Getenv(key)) != "" {
				return fmt.Errorf("%s is set but DIDEROT_BROWSER_AUTH is not %q", key, BrowserAuthOIDC)
			}
		}
		return nil
	}

	if cfg.OIDCIssuer == "" {
		return fmt.Errorf("DIDEROT_BROWSER_AUTH=oidc requires DIDEROT_OIDC_ISSUER")
	}
	if len(cfg.BrowserSessionKeys) == 0 {
		return fmt.Errorf("DIDEROT_BROWSER_AUTH=oidc requires DIDEROT_BROWSER_SESSION_KEYS")
	}
	cfg.BrowserOIDCClientID = strings.TrimSpace(os.Getenv("DIDEROT_BROWSER_OIDC_CLIENT_ID"))
	if cfg.BrowserOIDCClientID == "" {
		return fmt.Errorf("DIDEROT_BROWSER_AUTH=oidc requires DIDEROT_BROWSER_OIDC_CLIENT_ID")
	}
	cfg.BrowserOIDCClientSecret = strings.TrimSpace(os.Getenv("DIDEROT_BROWSER_OIDC_CLIENT_SECRET"))
	return nil
}

// loadBrowserSessions reads the session cookie settings. Without keys the
// browser has no sign-in of its own, so the other session settings must be
// unset.
func loadBrowserSessions(cfg *Config) error {
	keys, err := readKeyring("DIDEROT_BROWSER_SESSION_KEYS", "DIDEROT_BROWSER_SESSION_KEYS_FILE")
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		for _, key := range browserSessionEnv {
			if strings.TrimSpace(os.Getenv(key)) != "" {
				return fmt.Errorf("%s is set but DIDEROT_BROWSER_SESSION_KEYS is not", key)
			}
		}
		return nil
	}
	cfg.BrowserSessionKeys = keys
	if cfg.BrowserSessionTTL, err = browserDuration("DIDEROT_BROWSER_SESSION_TTL", defaultBrowserSessionTTL); err != nil {
		return err
	}
	if cfg.BrowserSessionIdleTimeout, err = browserDuration("DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT", defaultBrowserSessionIdle); err != nil {
		return err
	}
	if cfg.BrowserSessionTTL > maxBrowserSessionTTL {
		return fmt.Errorf("DIDEROT_BROWSER_SESSION_TTL must not exceed %s", maxBrowserSessionTTL)
	}
	if cfg.BrowserSessionIdleTimeout > cfg.BrowserSessionTTL {
		return fmt.Errorf("DIDEROT_BROWSER_SESSION_IDLE_TIMEOUT must not exceed DIDEROT_BROWSER_SESSION_TTL")
	}
	if raw := strings.TrimSpace(os.Getenv("DIDEROT_BROWSER_SESSION_INSECURE_COOKIE")); raw != "" {
		insecure, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("DIDEROT_BROWSER_SESSION_INSECURE_COOKIE: %w", err)
		}
		cfg.BrowserSessionInsecureCookie = insecure
	}
	return nil
}

// parseEmailDomains reads comma- or space-separated bare domains
// (example.com, not @example.com or *.example.com), lowercased.
func parseEmailDomains(raw string) ([]string, error) {
	var domains []string
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		domain := strings.ToLower(field)
		for _, label := range strings.Split(domain, ".") {
			if label == "" || strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
				return nil, fmt.Errorf("DIDEROT_OIDC_ALLOWED_EMAIL_DOMAINS: %q is not a bare domain name (for example example.com)", field)
			}
		}
		if !slices.Contains(domains, domain) {
			domains = append(domains, domain)
		}
	}
	return domains, nil
}

// readKeyring reads a keyring from its variable name, or from the file its
// variable fileName names, for deployments that mount keys as files. Setting
// both is an error.
func readKeyring(name, fileName string) ([][]byte, error) {
	raw := os.Getenv(name)
	if path := strings.TrimSpace(os.Getenv(fileName)); path != "" {
		if strings.TrimSpace(raw) != "" {
			return nil, fmt.Errorf("set %s or %s, not both", name, fileName)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fileName, err)
		}
		raw = string(contents)
	}
	return parseKeyring(name, raw)
}

// parseKeyring reads the variable name's comma- or space-separated base64
// keys of exactly 32 bytes (openssl rand -base64 32). None is no keys.
// Errors never echo key material.
func parseKeyring(name, raw string) ([][]byte, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	var keys [][]byte
	for i, field := range fields {
		var key []byte
		for _, encoding := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
		} {
			if decoded, err := encoding.DecodeString(field); err == nil {
				key = decoded
				break
			}
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("%s: key %d must be 32 bytes, base64-encoded (openssl rand -base64 32)", name, i+1)
		}
		if slices.ContainsFunc(keys, func(k []byte) bool { return string(k) == string(key) }) {
			return nil, fmt.Errorf("%s: key %d repeats an earlier key", name, i+1)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func browserDuration(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return d, nil
}

// parsePeerPrefix accepts a CIDR or a bare IP (one host). A zero-length
// prefix is rejected: trusting every peer lets any caller forge identity.
func parsePeerPrefix(raw string) (netip.Prefix, error) {
	if !strings.Contains(raw, "/") {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("invalid address %q", raw)
		}
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid CIDR %q", raw)
	}
	if prefix.Bits() == 0 {
		return netip.Prefix{}, fmt.Errorf("refusing to trust every peer (%q)", raw)
	}
	return prefix.Masked(), nil
}
