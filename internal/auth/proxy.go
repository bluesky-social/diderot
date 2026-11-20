package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// TrustedProxyConfig configures identity headers injected by an
// authenticating reverse proxy, which strips caller-supplied copies and
// injects, for example, X-Auth-Request-Email plus delimited
// X-Auth-Request-Groups.
type TrustedProxyConfig struct {
	// CIDRs lists the proxy's addresses. A request's immediate peer
	// (http.Request.RemoteAddr) must fall inside one of them for any identity
	// header to be read. X-Forwarded-For is never consulted: only the hop that
	// connected to us can vouch for the headers it sent. Empty disables header
	// identity entirely.
	CIDRs []netip.Prefix
	// EmailHeader carries the caller's email. Required with CIDRs.
	EmailHeader string
	// GroupsHeader optionally carries the caller's groups, split on
	// GroupsDelimiter (required with GroupsHeader).
	GroupsHeader    string
	GroupsDelimiter string
	Logger          *slog.Logger
}

// TrustedProxy authenticates the identity headers of a trusted reverse proxy.
// Header identities count as verified: the proxy authenticated the caller.
type TrustedProxy struct {
	cidrs           []netip.Prefix
	emailHeader     string
	groupsHeader    string
	groupsDelimiter string
	logger          *slog.Logger
}

func NewTrustedProxy(cfg TrustedProxyConfig) (*TrustedProxy, error) {
	p := &TrustedProxy{logger: cfg.Logger}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	if len(cfg.CIDRs) == 0 {
		return p, nil
	}
	for _, prefix := range cfg.CIDRs {
		if !prefix.IsValid() {
			return nil, fmt.Errorf("auth: invalid trusted proxy prefix %v", prefix)
		}
		p.cidrs = append(p.cidrs, prefix.Masked())
	}
	if !validHeaderName(cfg.EmailHeader) {
		return nil, errors.New("auth: trusted proxy requires a valid email header name")
	}
	p.emailHeader = http.CanonicalHeaderKey(strings.TrimSpace(cfg.EmailHeader))
	if strings.TrimSpace(cfg.GroupsHeader) != "" {
		if !validHeaderName(cfg.GroupsHeader) {
			return nil, errors.New("auth: trusted proxy groups header name is invalid")
		}
		if cfg.GroupsDelimiter == "" {
			return nil, errors.New("auth: trusted proxy groups header requires a groups delimiter")
		}
		p.groupsHeader = http.CanonicalHeaderKey(strings.TrimSpace(cfg.GroupsHeader))
		p.groupsDelimiter = cfg.GroupsDelimiter
	}
	return p, nil
}

func validHeaderName(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && httpguts.ValidHeaderFieldName(name)
}

// Enabled reports whether any peer can supply header identity.
func (p *TrustedProxy) Enabled() bool {
	return len(p.cidrs) > 0
}

func (p *TrustedProxy) Authenticate(r *http.Request) (*Principal, error) {
	if !p.Enabled() {
		return nil, ErrNoCredentials
	}
	peer, ok := remotePeer(r.RemoteAddr)
	if !ok || !p.trusts(peer) {
		if r.Header.Get(p.emailHeader) != "" || (p.groupsHeader != "" && r.Header.Get(p.groupsHeader) != "") {
			p.logger.Warn("ignoring identity headers from untrusted peer",
				"peer", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
		}
		return nil, ErrNoCredentials
	}
	// A conforming proxy sends exactly one value. More than one means a
	// caller-supplied copy survived the proxy, so neither is trustworthy.
	values := r.Header.Values(p.emailHeader)
	if len(values) != 1 {
		if len(values) > 1 {
			p.logger.Warn("ignoring repeated proxy email header", "peer", r.RemoteAddr, "path", r.URL.Path)
		}
		return nil, ErrNoCredentials
	}
	email := strings.TrimSpace(values[0])
	if email == "" {
		return nil, ErrNoCredentials
	}
	return &Principal{
		Subject:       email,
		Email:         email,
		EmailVerified: true,
		Display:       email,
		Groups:        p.groups(r),
	}, nil
}

func (p *TrustedProxy) groups(r *http.Request) []string {
	if p.groupsHeader == "" {
		return nil
	}
	values := r.Header.Values(p.groupsHeader)
	if len(values) != 1 {
		return nil
	}
	var groups []string
	for _, group := range strings.Split(values[0], p.groupsDelimiter) {
		if group = strings.TrimSpace(group); group != "" {
			groups = append(groups, group)
		}
	}
	return groups
}

func (p *TrustedProxy) trusts(peer netip.Addr) bool {
	for _, prefix := range p.cidrs {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}

func remotePeer(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}
