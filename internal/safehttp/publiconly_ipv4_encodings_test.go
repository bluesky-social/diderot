package safehttp

// Property tests for the invariant blockedCIDRs' own doc comment states:
// every IPv6 shape that could route to a blocked IPv4 address must be
// refused. isBlocked enforces this by decoding the embedded IPv4 address
// (embeddedIPv4) rather than listing each encoding's IPv6 range, so these
// tests check the decode itself: every encoding of every blocked IPv4
// address must be blocked, and ordinary public IPv6 addresses (which
// happen to share a leading byte or two with a transition encoding) must
// stay allowed.

import (
	"net"
	"testing"
)

// ipv4Encodings returns every IPv6 literal that embeds v4 under the
// encodings isBlocked is responsible for decoding (embeddedIPv4),
// keyed by encoding name. v4 must be a 4-byte net.IP.
//
// The NAT64 local-use prefix (RFC8215 64:ff9b:1::/48) is deliberately
// NOT in this map: embeddedIPv4 no longer decodes it (see its doc
// comment — the embedded address's byte offset depends on an
// operator-chosen prefix length this code can't know), so it isn't an
// "encoding of v4" in the sense this map represents; isBlocked instead
// blocks the whole /48 regardless of payload. That invariant has its
// own tests: TestIsBlocked's "NAT64 local-use prefix" cases and
// TestIsBlockedBlocksNAT64LocalUsePrefixRegardlessOfPayload below.
func ipv4Encodings(v4 net.IP) map[string]net.IP {
	v4 = v4.To4()
	mapped := net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, v4[0], v4[1], v4[2], v4[3]}
	compatible := net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, v4[0], v4[1], v4[2], v4[3]}
	nat64WKP := net.IP{0x00, 0x64, 0xff, 0x9b, 0, 0, 0, 0, 0, 0, 0, 0, v4[0], v4[1], v4[2], v4[3]}
	sixToFour := net.IP{0x20, 0x02, v4[0], v4[1], v4[2], v4[3], 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	teredo := net.IP{0x20, 0x01, 0x00, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, ^v4[0], ^v4[1], ^v4[2], ^v4[3]}
	siit := net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0, 0, v4[0], v4[1], v4[2], v4[3]}
	return map[string]net.IP{
		"native":                  v4,
		"IPv4-mapped":             mapped,
		"IPv4-compatible":         compatible,
		"NAT64 well-known prefix": nat64WKP,
		"6to4":                    sixToFour,
		"Teredo":                  teredo,
		"SIIT/IPv4-translated":    siit,
	}
}

// TestIsBlockedBlocksEveryEncodingOfABlockedIPv4Address checks the
// property blockedCIDRs' own doc comment requires: for every IPv4
// address the policy blocks (one representative from each range in
// blockedCIDRs, plus the private/loopback/unspecified ranges net.IP's
// own predicates cover), every IPv6 encoding that carries that
// address as its payload must also be blocked.
func TestIsBlockedBlocksEveryEncodingOfABlockedIPv4Address(t *testing.T) {
	blockedV4 := []string{
		"0.0.0.2",         // 0.0.0.0/8, "this network"
		"10.1.2.3",        // RFC1918 10/8
		"100.64.0.1",      // RFC6598 CGNAT
		"127.0.0.1",       // loopback
		"169.254.169.254", // link-local, AWS IMDS
		"172.16.5.5",      // RFC1918 172.16/12
		"192.168.1.1",     // RFC1918 192.168/16
		"224.0.0.1",       // multicast
		"240.0.0.1",       // reserved
	}
	for _, s := range blockedV4 {
		v4 := net.ParseIP(s)
		if v4 == nil {
			t.Fatalf("test bug: %q does not parse as an IP", s)
		}
		if !isBlocked(v4) {
			t.Fatalf("test bug: %s is not blocked natively; fix the fixture before testing its encodings", s)
		}
		for name, enc := range ipv4Encodings(v4) {
			if !isBlocked(enc) {
				t.Errorf("isBlocked(%s) = false, want true (%s encoding of blocked IPv4 %s)", enc, name, s)
			}
		}
	}
}

// TestIsBlockedLeavesOrdinaryPublicIPv6Allowed checks the converse:
// genuine public IPv6 addresses, including some that share a leading
// byte or two with a transition encoding's prefix, must stay allowed.
func TestIsBlockedLeavesOrdinaryPublicIPv6Allowed(t *testing.T) {
	public := []string{
		"2606:4700:4700::1111", // Cloudflare DNS
		"2001:4860:4860::8888", // Google DNS — starts with 2001 like
		// Teredo (2001:0000::/32), but its second group isn't zero
		"2003::1",     // not 6to4 (2002::/16)
		"2620:fe::fe", // Quad9 DNS
		"64:ff00::1",  // not the NAT64 well-known prefix (64:ff9b::/96)
	}
	for _, s := range public {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("test bug: %q does not parse as an IP", s)
		}
		if isBlocked(ip) {
			t.Errorf("isBlocked(%s) = true, want false (ordinary public IPv6 address)", ip)
		}
	}
}

// TestIsBlockedBlocksNAT64LocalUsePrefixRegardlessOfPayload pins that
// the RFC8215 NAT64 local-use prefix (64:ff9b:1::/48) is blocked whole.
// An earlier decoder assumed every deployment
// used an RFC6052 PL=48 byte layout, reading bytes 6,7,9,10 as the
// embedded IPv4 address. Real NAT64 translators (Tayga, AWS) use a
// /96 network-specific prefix inside that /48 far more commonly than
// /48 itself, which puts the actual embedded IPv4 address at bytes
// 12-15 instead — bytes the PL=48 decode never looked at. So for any
// deployment using a /56, /64 or /96 sub-prefix, the PL=48 decode
// read a constant, operator-prefix-derived value instead of the real
// payload, and an attacker-chosen embedded IPv4 address (e.g. AWS
// IMDS or an RFC1918 host) slipped through as "public".
//
// Since the sub-prefix length isn't recoverable from the address
// alone, the fix blocks the entire 64:ff9b:1::/48 outright (embedded
// IPv4 and embeddedIPv4 no longer decodes it at all — the SIIT and
// NAT64-well-known-prefix fixtures above continue to be decoded, to
// confirm this didn't regress). Every case below is a different
// assumed sub-prefix length carrying either an RFC1918/loopback/IMDS
// payload or a payload that looks public; all must be blocked.
func TestIsBlockedBlocksNAT64LocalUsePrefixRegardlessOfPayload(t *testing.T) {
	cases := []struct {
		ip   string
		desc string
	}{
		{"64:ff9b:1::", "the local-use prefix's network address itself"},
		{"64:ff9b:1::8.8.8.8", "PL=96 sub-prefix 64:ff9b:1::/96, suffix looks like public 8.8.8.8"},
		{"64:ff9b:1:abcd::a9fe:a9fe", "PL=96 sub-prefix 64:ff9b:1:abcd::/96, suffix is AWS IMDS 169.254.169.254"},
		{"64:ff9b:1:abcd::a00:5", "PL=96 sub-prefix 64:ff9b:1:abcd::/96, suffix is RFC1918 10.0.0.5"},
		{"64:ff9b:1:100::7f00:1", "would decode to public-looking 1.0.0.0 under the old PL=48-assumed layout; real /96-sub-prefix payload is loopback 127.0.0.1"},
		{"64:ff9b:1:ffff:ffff:ffff:ffff:ffff", "top of the /48 range"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("test bug: %q does not parse as an IP", tc.ip)
			}
			if !isBlocked(ip) {
				t.Errorf("isBlocked(%s) = false, want true (%s)", ip, tc.desc)
			}
			if _, ok := embeddedIPv4(ip); ok {
				t.Errorf("embeddedIPv4(%s) returned ok=true; the local-use prefix must not be decoded "+
					"(its sub-prefix length, and so its embedded address's byte offset, isn't knowable "+
					"from the address alone) — it must be blocked via blockedCIDRs instead", ip)
			}
		})
	}
}

// TestIPv4EncodingsRoundTrip is a sanity check on the test helper
// itself: each encoding's embedded IPv4 address, decoded by
// embeddedIPv4, must equal the address it was built from. This pins
// down that the property test above is actually exercising the
// intended encodings, not some other address by construction error.
func TestIPv4EncodingsRoundTrip(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "169.254.169.254", "10.1.2.3", "1.2.3.4"} {
		v4 := net.ParseIP(s).To4()
		for name, enc := range ipv4Encodings(v4) {
			got, ok := embeddedIPv4(enc)
			if !ok {
				t.Errorf("embeddedIPv4(%s) [%s encoding of %s] = _, false; want ok", enc, name, s)
				continue
			}
			if !got.Equal(v4) {
				t.Errorf("embeddedIPv4(%s) [%s encoding of %s] = %s, want %s", enc, name, s, got, v4)
			}
		}
	}
}
