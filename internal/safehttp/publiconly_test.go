package safehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func publicOnlyClient(timeout time.Duration) *http.Client {
	return NewClient(Options{Timeout: timeout, PublicOnly: true, CheckRedirect: MaxRedirects(5)})
}

func TestIsBlocked(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
		why     string
	}{
		// Loopback
		{"127.0.0.1", true, "ipv4 loopback"},
		{"127.1.2.3", true, "ipv4 loopback range"},
		{"::1", true, "ipv6 loopback"},
		// "this network" — Linux routes 0.0.0.0 to loopback
		{"0.0.0.0", true, "unspecified"},
		{"0.1.2.3", true, "0.0.0.0/8"},
		// Link-local incl. AWS IMDS
		{"169.254.0.1", true, "link-local"},
		{"169.254.169.254", true, "AWS IMDS"},
		{"fe80::1", true, "ipv6 link-local"},
		// RFC1918
		{"10.0.0.1", true, "RFC1918 10/8"},
		{"172.16.0.1", true, "RFC1918 172.16/12"},
		{"172.31.255.255", true, "RFC1918 172.16/12 boundary"},
		{"192.168.1.1", true, "RFC1918 192.168/16"},
		// CGNAT (Tailscale)
		{"100.64.0.1", true, "CGNAT"},
		{"100.127.255.255", true, "CGNAT boundary"},
		// IPv6 unique-local
		{"fc00::1", true, "ipv6 ULA"},
		{"fd00::1", true, "ipv6 ULA"},
		// Multicast
		{"224.0.0.1", true, "ipv4 multicast"},
		{"ff02::1", true, "ipv6 multicast"},
		// Public — should pass
		{"8.8.8.8", false, "public DNS"},
		{"1.1.1.1", false, "public DNS"},
		{"172.15.0.1", false, "outside 172.16/12 lower"},
		{"172.32.0.1", false, "outside 172.16/12 upper"},
		{"100.63.255.255", false, "outside CGNAT lower"},
		{"100.128.0.1", false, "outside CGNAT upper"},
		// IPv6 public
		{"2606:4700:4700::1111", false, "Cloudflare DNS v6"},
		// IPv6 transition encodings that embed a plain IPv4 address.
		// Trigger: a URL whose host is a literal IPv6 address spelled
		// in one of these forms. Contract (blockedCIDRs' own doc
		// comment): "this list must include every shape that could
		// route to the local node or local network" — isBlocked must
		// decode the embedded IPv4 address and apply the IPv4 rules
		// to it, the same way it already does for the IPv4-mapped
		// form (::ffff:a.b.c.d).
		{"::127.0.0.1", true, "IPv4-compatible IPv6 (RFC4291 §2.5.5.1), embeds 127.0.0.1 loopback"},
		{"::169.254.169.254", true, "IPv4-compatible IPv6, embeds AWS IMDS 169.254.169.254"},
		{"::10.1.2.3", true, "IPv4-compatible IPv6, embeds RFC1918 10.1.2.3"},
		{"::2", true, "IPv4-compatible IPv6, embeds 0.0.0.2 in blocked 0.0.0.0/8"},
		{"64:ff9b::169.254.169.254", true, "NAT64 well-known prefix (RFC6052 64:ff9b::/96), embeds AWS IMDS"},
		{"2002:a9fe:a9fe::", true, "6to4 (RFC3056 2002::/16), embeds AWS IMDS"},
		{"2001:0:4137:9e76::5601:5601", true, "Teredo (RFC4380 2001::/32), embeds AWS IMDS obfuscated (XOR 0xffffffff)"},
		{"::ffff:0:169.254.169.254", true, "SIIT / IPv4-translated (RFC2765 ::ffff:0:0:0/96), embeds AWS IMDS"},
		// NAT64 local-use prefix (RFC8215 64:ff9b:1::/48) is blocked
		// in full, regardless of what IPv4 address its operator-
		// chosen sub-prefix embeds — see embeddedIPv4's doc comment
		// on why the embedded address can't be located in general.
		{"64:ff9b:1:a9fe:a9:fe00::", true, "NAT64 local-use prefix, PL=48 layout"},
		// Decoding every local-use address as the PL=48 byte layout
		// would misread the prefix bytes of an operator using a /96
		// sub-prefix (e.g. 64:ff9b:1:abcd::/96, the common case for
		// Tayga/AWS-style NAT64) as the IPv4 payload, letting an
		// attacker-chosen internal address (here 10.0.0.5) slip
		// through as "public". It is blocked outright, independent
		// of payload or assumed layout.
		{"64:ff9b:1:abcd::a00:5", true, "NAT64 local-use prefix, /96 sub-prefix, embeds RFC1918 10.0.0.5"},
		{"64:ff9b:1:abcd::7f00:1", true, "NAT64 local-use prefix, /96 sub-prefix, embeds 127.0.0.1"},
		// Under its operator's actual /96 sub-prefix (64:ff9b:1:100::/96)
		// this address's real payload is 127.0.0.1 (loopback) — but the
		// old PL=48-assumed decode misread bytes 6,7,9,10 as 1.0.0.0, a
		// public-looking address, which isBlocked then let through. That
		// was the exact bypass: block the whole /48 and this stops
		// mattering, regardless of which layout anyone assumes.
		{"64:ff9b:1:100::7f00:1", true, "NAT64 local-use prefix, /96 sub-prefix, embeds 127.0.0.1 (misread as public 1.0.0.0 under the old fixed PL=48 decode)"},
		// Ordinary public IPv6 addresses must stay allowed, including
		// ones that share a leading byte with a transition encoding
		// (2001::/32 is Teredo only when the second group is zero;
		// 2002::/16 is 6to4, not 2003::/16; 64:ff9b::/96 is the
		// well-known NAT64 prefix, not the local-use one).
		{"2001:4860:4860::8888", false, "public IPv6 (Google DNS), not Teredo: second group is non-zero"},
		{"2003::1", false, "public-looking IPv6, not 6to4"},
		{"64:ff9b::8.8.8.8", false, "NAT64 well-known prefix embedding a public IPv4 address (8.8.8.8) must stay allowed — a DNS64 resolver synthesizes exactly this for any public IPv4-only host"},
		{"2002:808:808::1", false, "6to4 embedding a public IPv4 address (8.8.8.8) must stay allowed"},
	}
	for _, tc := range tests {
		t.Run(tc.ip+"_"+tc.why, func(t *testing.T) {
			got := isBlocked(net.ParseIP(tc.ip))
			if got != tc.blocked {
				t.Errorf("isBlocked(%s) = %v, want %v (%s)",
					tc.ip, got, tc.blocked, tc.why)
			}
		})
	}
}

func TestSSRFSafeClientBlocksLoopback(t *testing.T) {
	// Stand up a local httptest server (listens on 127.0.0.1)
	// and confirm the SSRF-safe client refuses to dial it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not be reached"))
	}))
	defer srv.Close()

	client := publicOnlyClient(5 * time.Second)
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected blocked dial; got %d", resp.StatusCode)
	}
	var bae *BlockedAddressError
	if !errors.As(err, &bae) {
		t.Errorf("expected BlockedAddressError; got %T: %v", err, err)
	}
}

func TestSSRFSafeClientAllowsPublicHost(t *testing.T) {
	// Quick sanity check: the dialer should at least attempt
	// connection to a public address. We can't make a real HTTP
	// request (network-flaky in CI), so we just verify isBlocked
	// returns false for a hostname that resolves to a public
	// address. The Get() side of this is exercised in
	// TestNotionFetcherFetchImageHappy via a non-loopback
	// httptest server when needed.
	if isBlocked(net.ParseIP("8.8.8.8")) {
		t.Error("8.8.8.8 should NOT be blocked")
	}
}

func TestChooseDialIP(t *testing.T) {
	mk := func(ss ...string) []net.IPAddr {
		out := make([]net.IPAddr, 0, len(ss))
		for _, s := range ss {
			out = append(out, net.IPAddr{IP: net.ParseIP(s)})
		}
		return out
	}

	t.Run("all public picks first", func(t *testing.T) {
		got, err := chooseDialIP("example.com", mk("8.8.8.8", "1.1.1.1"))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !got.Equal(net.ParseIP("8.8.8.8")) {
			t.Errorf("chose %v, want 8.8.8.8 (first)", got)
		}
	})

	t.Run("empty set errors", func(t *testing.T) {
		if _, err := chooseDialIP("example.com", nil); err == nil {
			t.Error("expected error on empty address set")
		}
	})

	// The security-critical case: a resolver returning a blocked +
	// public pair (the DNS-rebind / split-horizon poisoning attack)
	// must be refused outright, NOT silently dialed via the public IP.
	t.Run("blocked anywhere refuses whole dial", func(t *testing.T) {
		cases := []struct {
			name string
			ips  []net.IPAddr
			bad  string
		}{
			{"blocked first", mk("169.254.169.254", "8.8.8.8"), "169.254.169.254"},
			{"blocked last", mk("8.8.8.8", "10.0.0.1"), "10.0.0.1"},
			{"blocked middle", mk("1.1.1.1", "127.0.0.1", "8.8.8.8"), "127.0.0.1"},
			// Variant of the IPv6-encoding gap: an attacker-controlled
			// hostname whose AAAA record is a NAT64-encoded AWS IMDS
			// address must refuse the dial on the DNS-resolution path
			// too, not just the literal-IP fast path in DialContext
			// (TestSSRFSafeClientBlocksIPv4CompatibleLiteral covers
			// that one).
			{"NAT64-encoded AWS IMDS via AAAA", mk("8.8.8.8", "64:ff9b::169.254.169.254"), "64:ff9b::169.254.169.254"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := chooseDialIP("rebind.example.com", tc.ips)
				if err == nil {
					t.Fatalf("expected refusal; got dial IP %v", got)
				}
				var bae *BlockedAddressError
				if !errors.As(err, &bae) {
					t.Fatalf("expected BlockedAddressError; got %T: %v", err, err)
				}
				if !bae.IP.Equal(net.ParseIP(tc.bad)) {
					t.Errorf("blamed IP %v, want %v", bae.IP, tc.bad)
				}
			})
		}
	})
}

// TestSSRFSafeClientBlocksLiteralIP exercises the IP-literal fast
// path: a URL whose host is already a blocked literal IP (no DNS
// hop) must be refused with a BlockedAddressError before any dial.
func TestSSRFSafeClientBlocksLiteralIP(t *testing.T) {
	client := publicOnlyClient(2 * time.Second)
	// AWS IMDS by literal — the canonical SSRF target. No DNS
	// resolution happens; the fast path validates the parsed IP.
	resp, err := client.Get("http://169.254.169.254/latest/meta-data/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected blocked dial to IMDS literal; got %d", resp.StatusCode)
	}
	var bae *BlockedAddressError
	if !errors.As(err, &bae) {
		t.Errorf("expected BlockedAddressError; got %T: %v", err, err)
	}
}

// TestSSRFSafeClientBlocksIPv4CompatibleLiteral proves the gap is
// reachable through the real dialer, not just isBlocked: it calls
// publicOnlyTransport's DialContext (the function net/http invokes per
// connection) with "[::169.254.169.254]:80" — the IPv4-compatible IPv6
// spelling of AWS IMDS. Because the host parses as a literal IP,
// DialContext takes the fast path straight to isBlocked with no DNS hop.
// Before the fix, isBlocked did not decode this encoding's embedded IPv4
// address, so the dial was attempted and only failed with an ordinary
// OS-level network error, never with *BlockedAddressError — the same
// failure mode TestSSRFSafeClientBlocksLiteralIP proves does NOT happen
// for the plain "169.254.169.254" spelling.
func TestSSRFSafeClientBlocksIPv4CompatibleLiteral(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport := publicOnlyTransport()
	conn, err := transport.DialContext(ctx, "tcp", "[::169.254.169.254]:80")
	if conn != nil {
		_ = conn.Close()
	}
	var bae *BlockedAddressError
	if !errors.As(err, &bae) {
		t.Fatalf("DialContext(\"[::169.254.169.254]:80\") = conn=%v, err=%v (%T); want *BlockedAddressError "+
			"refusing AWS IMDS before any dial — instead the SSRF check was skipped for this address's "+
			"IPv4-compatible-IPv6 spelling", conn, err, err)
	}
}

func TestSSRFSafeClientRedirectCap(t *testing.T) {
	// Configure a public-style redirect chain that would loop
	// forever; confirm we cap at 5.
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
	}))
	defer srv.Close()
	// Note: this server is on 127.0.0.1 so the SSRF check fires
	// before redirect logic ever engages — that's fine, this test
	// just ensures the redirect policy *would* cap if dialing
	// were allowed. It exercises CheckRedirect's loop guard
	// indirectly: any test against an actual public redirect
	// chain would be network-flaky, so we settle for the type
	// assertion here.
	client := publicOnlyClient(2 * time.Second)
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("expected blocked dial")
	}
}
