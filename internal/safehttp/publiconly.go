package safehttp

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// blockedCIDRs enumerates IP ranges that a PublicOnly client
// refuses to dial. Author-supplied URLs, such as Notion's
// type=external image blocks, could otherwise point at internal
// infrastructure (AWS IMDS, cluster DNS, RFC1918, Tailscale
// CGNAT), exfiltrating sensitive responses through the captioner
// → embedder → retrieval surface.
//
// IMPORTANT: this list must include every IPv4 range, and every
// genuinely-IPv6 range, that could route to the local node or
// local network. It does NOT need an entry per IPv6 transition
// encoding (IPv4-mapped, IPv4-compatible, NAT64, 6to4, Teredo,
// SIIT): embeddedIPv4 decodes those down to the IPv4 address they
// carry, and isBlocked applies these same IPv4 ranges to that
// address instead. The one exception is the NAT64 local-use
// prefix, 64:ff9b:1::/48 — see its entry below and embeddedIPv4's
// doc comment. Notable inclusions:
//
//   - 0.0.0.0/8: Linux routes 0.0.0.0 to loopback. Without this
//     entry, an author can paste http://0.0.0.0:8080/ to reach a
//     localhost service, bypassing the 127.0.0.0/8 entry.
//   - 100.64.0.0/10: CGNAT, where overlay networks such as
//     Tailscale put hosts that can include internal services.
//   - 169.254.0.0/16: link-local, includes AWS IMDS at
//     169.254.169.254.
//   - fc00::/7: RFC4193 unique-local IPv6 (covers fd00::/8 too).
//   - fe80::/10: IPv6 link-local.
//   - ::1: IPv6 loopback.
//   - 64:ff9b:1::/48: RFC8215 NAT64 local-use prefix. An operator
//     carves a network-specific prefix of /48, /56, /64 or /96
//     inside this /48 for their own NAT64 translation, and the
//     embedded IPv4 address lands at a different byte offset
//     depending on which length they chose — an offset this code
//     has no way to know. So, unlike every other transition
//     encoding, this one is blocked outright rather than decoded:
//     it is never a legitimate dial target for us (we are not that
//     operator's NAT64 client), and IANA's special-purpose registry
//     marks it not globally reachable in any case.
//
// Any IP in any of these ranges → DialContext refuses.
var blockedCIDRs = mustParseCIDRs([]string{
	// IPv4
	"0.0.0.0/8",      // "this network"; routes to loopback on Linux
	"10.0.0.0/8",     // RFC1918
	"100.64.0.0/10",  // RFC6598 CGNAT (Tailscale)
	"127.0.0.0/8",    // loopback
	"169.254.0.0/16", // link-local (incl. AWS IMDS)
	"172.16.0.0/12",  // RFC1918
	"192.168.0.0/16", // RFC1918
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved
	// IPv6
	"::1/128",        // loopback
	"fc00::/7",       // RFC4193 unique-local (covers fd00::/8)
	"fe80::/10",      // link-local
	"ff00::/8",       // multicast
	"64:ff9b:1::/48", // RFC8215 NAT64 local-use prefix; blocked
	// outright rather than decoded — see the
	// comment above.
})

// publicOnlyTransport returns a Transport whose DialContext
// resolves the target hostname, rejects any returned address in
// the blocklist, and dials the validated IP. Each client gets its
// own connection pool.
//
// TLS SNI handling: dialing a literal IP would normally make
// crypto/tls use the IP as the SNI hostname (and bind cert
// verification to it), which fails for any real cert. We avoid
// that by leaving the http.Transport's TLS config defaults
// alone — net/http extracts the SNI from the request URL's
// hostname, not from the dial address — and only swapping the
// dial address inside DialContext. The connection's TLS
// handshake then sends the original hostname as SNI and verifies
// the cert against it. This is the standard pattern for
// SSRF-safe HTTP clients in Go.
//
// DNS rebinding mitigation: the resolve happens ONCE inside
// DialContext, and the dialed IP is the same IP we validated.
// There's no second resolve in the stdlib's connection path
// that could return a different address. (A custom Resolver
// passed via net.Dialer would be re-invoked by Transport for
// the underlying dial; instead we resolve, then call
// DialContext with an explicit IP literal.)
func publicOnlyTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		// Proxy MUST be nil, not ProxyFromEnvironment. With an
		// ambient HTTP(S)_PROXY set, net/http dials the PROXY and
		// hands it the attacker-controlled target — so our
		// DialContext validates the proxy's address while the proxy
		// itself resolves and fetches e.g. 169.254.169.254,
		// bypassing the blocklist entirely. An SSRF-safe client can't
		// delegate egress to an ambient proxy; we always dial the
		// validated target directly. (No proxy is configured in any
		// diderot deployment today, so this is defense against a
		// future env change voiding the guarantee, not a live hole.)
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("ssrf: split host:port %q: %w", addr, err)
			}
			// If addr is already a literal IP, skip the
			// resolve hop but still validate.
			if ip := net.ParseIP(host); ip != nil {
				if isBlocked(ip) {
					return nil, &BlockedAddressError{Host: host, IP: ip}
				}
				return dialer.DialContext(ctx, network, addr)
			}
			ips, err := dialer.Resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("ssrf: resolve %s: %w", host, err)
			}
			chosen, err := chooseDialIP(host, ips)
			if err != nil {
				return nil, err
			}
			// Dial the literal IP we validated, not the
			// hostname. crypto/tls reads SNI from the
			// request's hostname (set by net/http on the
			// outgoing TLS connection), independent of the
			// dial address — so this preserves correct SNI
			// and cert verification.
			return dialer.DialContext(ctx, network, net.JoinHostPort(chosen.String(), port))
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
}

// chooseDialIP applies the SSRF policy to a resolved address set
// and returns the IP to dial. The stance is all-or-nothing: if ANY
// resolved address is blocked, the whole dial is refused — this
// prevents an attacker from poisoning resolution with a
// blocked+public pair to slip past the check while keeping the page
// reachable (DNS-rebind / split-horizon defense). Otherwise it
// returns the first address. Extracted from the dialer closure so
// the policy is unit-testable without injecting DNS.
func chooseDialIP(host string, ips []net.IPAddr) (net.IP, error) {
	if len(ips) == 0 {
		return nil, fmt.Errorf("ssrf: no addresses for %s", host)
	}
	var chosen net.IP
	for _, ipa := range ips {
		if isBlocked(ipa.IP) {
			return nil, &BlockedAddressError{Host: host, IP: ipa.IP}
		}
		if chosen == nil {
			chosen = ipa.IP
		}
	}
	return chosen, nil
}

// BlockedAddressError is the error returned by the SSRF-safe
// dialer when a hostname resolves to a blocked address. Exposed
// as a typed error so callers/tests can assert on the cause.
type BlockedAddressError struct {
	Host string
	IP   net.IP
}

func (e *BlockedAddressError) Error() string {
	return fmt.Sprintf("ssrf: refusing to dial %s (resolves to blocked %s)", e.Host, e.IP)
}

// isBlocked reports whether ip falls in any of the blocked
// ranges. Also rejects unspecified addresses (0.0.0.0, ::) and
// non-global multicast.
//
// An IPv6 address can carry a plain IPv4 address under several
// transition encodings (IPv4-mapped, the deprecated IPv4-compatible
// form, NAT64, 6to4, Teredo, SIIT). Rather than list each encoding's
// IPv6 range in blockedCIDRs, isBlocked decodes the embedded IPv4
// address (embeddedIPv4) and applies the IPv4 rules to it directly —
// the same rules a native IPv4 address gets. A genuine IPv6 address
// with no such encoding is checked against the IPv6-specific rules
// unchanged. Both checks run even when embeddedIPv4 decodes an
// address, so an IPv6-specific blockedCIDRs entry that happens to
// overlap a transition prefix (like 64:ff9b:1::/48) still applies on
// its own terms, rather than being silently shadowed by the decode.
func isBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4, ok := embeddedIPv4(ip); ok {
		return rangeBlocked(v4) || rangeBlocked(ip)
	}
	return rangeBlocked(ip)
}

// rangeBlocked applies the blocked-ranges policy to ip, which may be
// a native IPv4 address, an IPv4 address decoded from an IPv6
// transition encoding by embeddedIPv4, or a genuine IPv6 address
// that carries no IPv4 payload.
func rangeBlocked(ip net.IP) bool {
	if ip.IsUnspecified() {
		return true
	}
	if ip.IsLoopback() {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsInterfaceLocalMulticast() {
		return true
	}
	if ip.IsPrivate() {
		// Go's IsPrivate covers RFC1918 (10/8, 172.16/12,
		// 192.168/16) AND RFC4193 (fc00::/7). Belt-and-suspenders
		// with the explicit CIDRs below.
		return true
	}
	for _, n := range blockedCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// embeddedIPv4 extracts the IPv4 address that ip carries under any
// IPv6 transition encoding, so isBlocked can apply the IPv4 policy
// to that address directly instead of maintaining a separate IPv6
// CIDR for every encoding. ok is false when ip carries no IPv4
// address under any known encoding (an ordinary IPv6 address, or one
// of the encodings below that isBlocked instead blocks outright via
// blockedCIDRs because the embedded address can't be located).
//
// Deliberately NOT decoded here:
//
//   - NAT64 local-use prefix, RFC8215 64:ff9b:1::/48: unlike the
//     well-known prefix below, an operator can carve a /48, /56,
//     /64 or /96 network-specific prefix out of this /48 for their
//     own NAT64 translator, and the embedded IPv4 address lands at
//     a different byte offset for each length. Decoding only one
//     length (as an earlier version of this function did, assuming
//     /48) reads the operator's own prefix bits as if they were the
//     payload for any deployment using a different length, and so
//     silently lets an attacker-chosen internal IPv4 address
//     through undetected. Since the offset isn't knowable from the
//     address alone, blockedCIDRs blocks the entire /48 instead.
//   - ISATAP interface IDs, ::5EFE:a.b.c.d or ::0200:5EFE:a.b.c.d:
//     this pattern lives in the low 64 bits of the address, with no
//     fixed global prefix — any /64, public or private, can carry
//     an ISATAP interface ID. Decoding it would mean treating any
//     address with this interface-ID pattern as its embedded IPv4
//     payload regardless of network prefix, which risks matching a
//     coincidental genuine address. Left undecoded: ISATAP is a
//     protocol-41 tunnel (like 6to4 and IPv4-compatible without a
//     routed path to the payload on an ordinary network), so it is
//     lower-risk than NAT64's stateful, same-protocol translation.
func embeddedIPv4(ip net.IP) (v4 net.IP, ok bool) {
	if v4 := ip.To4(); v4 != nil {
		// Native IPv4, or IPv4-mapped IPv6 (::ffff:a.b.c.d) — the
		// only two forms net.IP.To4 itself recognizes.
		return v4, true
	}
	ip16 := ip.To16()
	if ip16 == nil {
		return nil, false
	}
	switch {
	case bytes.Equal(ip16[0:4], []byte{0x00, 0x64, 0xff, 0x9b}) && isZeroBytes(ip16[4:12]):
		// NAT64 well-known prefix, RFC6052 §2.1/2.2 (PL=96):
		// 64:ff9b::/96, IPv4 address in the last 32 bits.
		return append(net.IP(nil), ip16[12:16]...), true
	case ip16[0] == 0x20 && ip16[1] == 0x02:
		// 6to4, RFC3056: 2002::/16, IPv4 address in bytes 2-5.
		return append(net.IP(nil), ip16[2:6]...), true
	case ip16[0] == 0x20 && ip16[1] == 0x01 && ip16[2] == 0x00 && ip16[3] == 0x00:
		// Teredo, RFC4380: 2001:0000::/32. The tunneled client's
		// IPv4 address is in the last 32 bits, obfuscated by XOR
		// with 0xffffffff (bitwise complement).
		v4 := make(net.IP, 4)
		for i := range v4 {
			v4[i] = ^ip16[12+i]
		}
		return v4, true
	case isZeroBytes(ip16[0:8]) && ip16[8] == 0xff && ip16[9] == 0xff && ip16[10] == 0x00 && ip16[11] == 0x00:
		// SIIT / "IPv4-translated", RFC2765 §2: ::ffff:0:a.b.c.d
		// (::ffff:0:0:0/96), IPv4 address in bytes 12-15. Produced
		// by a stateless IP/ICMP translator rather than a stateful
		// NAT64 gateway; same fixed-offset embedding as IPv4-mapped
		// (::ffff:a.b.c.d), shifted one group over, so it carries
		// none of the local-use prefix's ambiguity.
		return append(net.IP(nil), ip16[12:16]...), true
	case isZeroBytes(ip16[0:12]) && !ip16.Equal(net.IPv6unspecified) && !ip16.Equal(net.IPv6loopback):
		// Deprecated IPv4-compatible IPv6, RFC4291 §2.5.5.1:
		// ::a.b.c.d. "::" and "::1" are the IPv6 unspecified and
		// loopback addresses, not this mapping — matching the RFC's
		// own carve-out (and moot either way: both are already
		// blocked by rangeBlocked's IsUnspecified/IsLoopback checks).
		return append(net.IP(nil), ip16[12:16]...), true
	}
	return nil, false
}

// isZeroBytes reports whether every byte in b is zero.
func isZeroBytes(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// mustParseCIDRs parses every CIDR or panics. Called once at
// package init.
func mustParseCIDRs(in []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(in))
	for _, s := range in {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic(fmt.Sprintf("safehttp: bad CIDR %q: %v", s, err))
		}
		out = append(out, n)
	}
	return out
}
