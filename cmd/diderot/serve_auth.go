package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"

	"github.com/bluesky-social/diderot/internal/auth"
	"github.com/bluesky-social/diderot/internal/config"
	"github.com/bluesky-social/diderot/internal/settings"
)

// serverAuth holds one authenticator per HTTP surface. Browser identity
// headers are never an API credential, and the API bearer is never a browser
// credential.
type serverAuth struct {
	mode string
	// browserMode names the browser's external identity source: "none" (the
	// fixed none-mode principal), "trusted-proxy", or "oidc" (built-in
	// sign-in).
	browserMode string
	// localSignIn reports whether the browser has its own sign-in (session
	// keys are set): the bootstrap admin, and static tokens in static mode.
	localSignIn bool
	// api authenticates /v1 bearer callers.
	api auth.Authenticator
	// static is the token file's tokens in static mode, nil otherwise.
	static *auth.Static
	// browser identifies /admin and /sessions callers. With session keys it
	// is an *auth.BrowserLogin, whose sign-in routes adminui mounts.
	browser auth.Authenticator
	// mcp identifies the human behind a gateway's /mcp call, after the
	// gateway's backend key (DIDEROT_GATEWAY_API_KEY) has been checked. A
	// /mcp call without that key authenticates through api instead, and
	// this is never consulted for it.
	mcp auth.Authenticator
	// browserIdentity is false when no source can identify a browser, so the
	// UI answers every request with 401.
	browserIdentity bool
}

// buildServerAuth assembles the authenticators for cfg.AuthMode, which
// config.Load has checked is complete. listenAddrs are every address this
// process will listen on; none mode refuses any that is not a literal
// loopback IP. bootstrap answers whether the bootstrap admin may sign in;
// nil offers no bootstrap admin.
func buildServerAuth(cfg *config.Config, listenAddrs []string, bootstrap auth.BootstrapAdmin, logger *slog.Logger) (*serverAuth, error) {
	if cfg.AuthMode == config.AuthModeNone {
		if err := requireLoopbackListeners(listenAddrs); err != nil {
			return nil, fmt.Errorf("DIDEROT_AUTH_MODE=none: %w", err)
		}
		admin := auth.NewFixed(nonePrincipal(cfg))
		return &serverAuth{
			mode: cfg.AuthMode, browserMode: config.AuthModeNone,
			api: admin, browser: admin, mcp: admin, browserIdentity: true,
		}, nil
	}

	api, static, err := apiAuthenticator(cfg, logger)
	if err != nil {
		return nil, err
	}
	proxyLogger := logger.With("component", "trusted-proxy")
	mcp, err := auth.NewTrustedProxy(auth.TrustedProxyConfig{
		CIDRs: cfg.TrustedProxyCIDRs, EmailHeader: cfg.TrustedProxyMCPEmailHeader,
		GroupsHeader: cfg.TrustedProxyGroupsHeader, GroupsDelimiter: cfg.TrustedProxyGroupsDelimiter,
		Logger: proxyLogger,
	})
	if err != nil {
		return nil, err
	}
	sa := &serverAuth{mode: cfg.AuthMode, api: api, static: static, mcp: mcp, localSignIn: len(cfg.BrowserSessionKeys) > 0}
	// With the built-in OIDC sign-in, trusted-proxy headers are never read
	// on browser routes, even from a trusted peer; the proxy settings then
	// serve /mcp alone. Otherwise they identify a browser without a session.
	var proxy *auth.TrustedProxy
	if cfg.BrowserAuth == config.BrowserAuthOIDC {
		sa.browserMode = config.BrowserAuthOIDC
	} else {
		sa.browserMode = config.BrowserAuthTrustedProxy
		proxy, err = auth.NewTrustedProxy(auth.TrustedProxyConfig{
			CIDRs: cfg.TrustedProxyCIDRs, EmailHeader: cfg.TrustedProxyEmailHeader,
			GroupsHeader: cfg.TrustedProxyGroupsHeader, GroupsDelimiter: cfg.TrustedProxyGroupsDelimiter,
			Logger: proxyLogger,
		})
		if err != nil {
			return nil, err
		}
	}
	if !sa.localSignIn {
		if proxy == nil {
			return nil, fmt.Errorf("DIDEROT_BROWSER_AUTH=oidc requires DIDEROT_BROWSER_SESSION_KEYS")
		}
		sa.browser, sa.browserIdentity = proxy, proxy.Enabled()
		return sa, nil
	}
	login, err := newBrowserLogin(cfg, bootstrap, static, proxy, logger)
	if err != nil {
		return nil, err
	}
	sa.browser, sa.browserIdentity = login, true
	return sa, nil
}

// apiAuthenticator is /v1's bearer authenticator for cfg.AuthMode, oidc or
// static; in static mode it is also returned as the tokens the browser's
// sign-in accepts.
func apiAuthenticator(cfg *config.Config, logger *slog.Logger) (auth.Authenticator, *auth.Static, error) {
	switch cfg.AuthMode {
	case config.AuthModeOIDC:
		oidcAuth, err := auth.NewOIDC(auth.OIDCConfig{
			Issuer: cfg.OIDCIssuer, Audience: cfg.OIDCAudience, JWKSURL: cfg.OIDCJWKSURL,
			GroupsClaim: cfg.OIDCGroupsClaim, EmailClaim: cfg.OIDCEmailClaim,
			RequireEmailVerified: cfg.OIDCRequireEmailVerified,
			AllowedEmailDomains:  cfg.OIDCAllowedEmailDomains, HostedDomainClaim: cfg.OIDCHostedDomainClaim,
			Logger: logger.With("component", "oidc"),
		})
		if err != nil {
			return nil, nil, err
		}
		return oidcAuth, nil, nil
	case config.AuthModeStatic:
		static, err := auth.LoadStaticTokens(cfg.StaticTokensFile)
		if err != nil {
			return nil, nil, err
		}
		return static, static, nil
	default:
		return nil, nil, fmt.Errorf("unsupported auth mode %q", cfg.AuthMode)
	}
}

// newBrowserLogin is the browser's own sign-in, with session keys set: the
// bootstrap admin, static's tokens in static mode, and with
// DIDEROT_BROWSER_AUTH=oidc the OIDC sign-in, or else proxy's headers for a
// browser without a session.
func newBrowserLogin(cfg *config.Config, bootstrap auth.BootstrapAdmin, static *auth.Static, proxy *auth.TrustedProxy, logger *slog.Logger) (*auth.BrowserLogin, error) {
	loginCfg := auth.BrowserLoginConfig{
		Bootstrap: bootstrap, StaticTokens: static, Proxy: proxy,
		PublicURL:   cfg.PublicURL,
		SessionKeys: cfg.BrowserSessionKeys,
		SessionTTL:  cfg.BrowserSessionTTL, IdleTimeout: cfg.BrowserSessionIdleTimeout,
		InsecureCookies: cfg.BrowserSessionInsecureCookie,
		Logger:          logger.With("component", "browser-login"),
	}
	if cfg.BrowserAuth == config.BrowserAuthOIDC {
		loginCfg.Issuer, loginCfg.JWKSURL = cfg.OIDCIssuer, cfg.OIDCJWKSURL
		loginCfg.ClientID, loginCfg.ClientSecret = cfg.BrowserOIDCClientID, cfg.BrowserOIDCClientSecret
		loginCfg.Scopes = cfg.OIDCScopes
		loginCfg.GroupsClaim, loginCfg.EmailClaim = cfg.OIDCGroupsClaim, cfg.OIDCEmailClaim
		loginCfg.RequireEmailVerified = cfg.OIDCRequireEmailVerified
		loginCfg.AllowedEmailDomains, loginCfg.HostedDomainClaim = cfg.OIDCAllowedEmailDomains, cfg.OIDCHostedDomainClaim
		loginCfg.SessionGroups = allowlistGroups(cfg)
	}
	return auth.NewBrowserLogin(loginCfg)
}

// configureServerAuth builds each surface's authenticator, warns while the
// bootstrap admin is on, and logs how each surface authenticates. store
// answers, on every request, whether the bootstrap admin may still sign in.
func configureServerAuth(cfg *config.Config, listenAddrs []string, store *settings.Store, logger *slog.Logger) (*serverAuth, error) {
	sa, err := buildServerAuth(cfg, listenAddrs, store, logger)
	if err != nil {
		logger.Error("invalid authentication configuration", "err", err)
		return nil, fmt.Errorf("configure authentication: %w", err)
	}
	warnBootstrapAdmin(cfg, store.Org().Org, logger)
	logServerAuth(cfg, sa, logger)
	return sa, nil
}

// logServerAuth logs how each surface authenticates, and warns when no
// source can identify a browser.
func logServerAuth(cfg *config.Config, sa *serverAuth, logger *slog.Logger) {
	logger.Info("operator auth", "mode", sa.mode, "browser", sa.browserMode,
		"local_sign_in", sa.localSignIn, "issuer", cfg.OIDCIssuer,
		"require_email_verified", cfg.OIDCRequireEmailVerified,
		"allowed_email_domains", cfg.OIDCAllowedEmailDomains, "hosted_domain_claim", cfg.OIDCHostedDomainClaim,
		"trusted_proxy_cidrs", len(cfg.TrustedProxyCIDRs))
	if !sa.browserIdentity {
		logger.Warn("browser UI has no identity source and will answer 401; set DIDEROT_BROWSER_SESSION_KEYS for local sign-in, DIDEROT_TRUSTED_PROXY_CIDRS and its header names, or DIDEROT_BROWSER_AUTH=oidc")
	}
}

// warnBootstrapAdmin logs, on every boot, that the bootstrap admin is still
// enabled, or that it is enabled but unusable because the browser has no
// session keys. None mode has no sign-in, so the account never applies.
func warnBootstrapAdmin(cfg *config.Config, org settings.Org, logger *slog.Logger) {
	if !org.BootstrapAdminEnabled || cfg.AuthMode == config.AuthModeNone {
		return
	}
	if len(cfg.BrowserSessionKeys) == 0 {
		logger.Warn("the bootstrap admin is enabled but cannot sign in: set DIDEROT_BROWSER_SESSION_KEYS (openssl rand -base64 32) and restart, then sign in at /admin as admin / admin")
		return
	}
	logger.Warn("the bootstrap admin (admin / admin) is enabled: anyone who reaches /admin can sign in as an administrator; follow /admin/settings and turn it off")
}

// nonePrincipal is the single administrator of a none-mode install. It holds
// every configured administrator and self-service group so each surface
// treats it as both.
func nonePrincipal(cfg *config.Config) auth.Principal {
	return auth.Principal{
		Subject: cfg.NoneEmail, Email: cfg.NoneEmail, EmailVerified: true,
		Display: cfg.NoneEmail, Groups: allowlistGroups(cfg),
	}
}

// allowlistGroups is every group the browser surfaces gate on: the
// administrator allowlist, then the self-service one.
func allowlistGroups(cfg *config.Config) []string {
	groups := append([]string(nil), cfg.AdminGroups...)
	for _, group := range cfg.SelfServiceGroups {
		if !slices.Contains(groups, group) {
			groups = append(groups, group)
		}
	}
	return groups
}

// requireLoopbackListeners accepts only literal loopback IPs. An empty host
// binds every interface, and a hostname could resolve anywhere.
func requireLoopbackListeners(addrs []string) error {
	for _, addr := range addrs {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("listener %q: %w", addr, err)
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.IsLoopback() {
			return fmt.Errorf("listener %q must bind a literal loopback IP (for example 127.0.0.1:9091)", addr)
		}
	}
	return nil
}
