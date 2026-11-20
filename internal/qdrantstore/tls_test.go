package qdrantstore

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/qdrant/go-client/qdrant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// testCA is a certificate authority made for one test, so no key material
// is checked in.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "diderot test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return testCA{cert: cert, key: key, pool: pool}
}

// serverCert issues a server certificate for the given DNS names and IPs.
func (ca testCA) serverCert(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeQdrant answers the health check Ping makes and keeps the API key it
// was sent.
type fakeQdrant struct {
	qdrant.UnimplementedQdrantServer
	mu     sync.Mutex
	apiKey []string
}

func (f *fakeQdrant) HealthCheck(ctx context.Context, _ *qdrant.HealthCheckRequest) (*qdrant.HealthCheckReply, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	f.apiKey = md.Get("api-key")
	f.mu.Unlock()
	return &qdrant.HealthCheckReply{Title: "fake qdrant", Version: "test"}, nil
}

func (f *fakeQdrant) receivedAPIKey() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apiKey
}

// serveFakeQdrant serves fake on a loopback port, over TLS with cert when it
// is not nil, and returns the address.
func serveFakeQdrant(t *testing.T, fake *fakeQdrant, cert *tls.Certificate) string {
	t.Helper()
	var options []grpc.ServerOption
	if cert != nil {
		options = append(options, grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{*cert},
			MinVersion:   tls.VersionTLS13,
		})))
	}
	server := grpc.NewServer(options...)
	qdrant.RegisterQdrantServer(server, fake)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func pingFake(t *testing.T, cfg Config) error {
	t.Helper()
	cfg.Collection = "tls_test"
	store, err := New(cfg, prometheus.NewRegistry())
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return store.Ping(ctx)
}

// TestPingTLS connects to an in-process Qdrant stand-in over TLS and checks
// that every mismatch between Diderot's TLS settings and the server's fails
// Ping with a message that names the setting to change.
func TestPingTLS(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.serverCert(t, "localhost", "127.0.0.1")
	fake := &fakeQdrant{}
	tlsAddr := serveFakeQdrant(t, fake, &cert)
	plainAddr := serveFakeQdrant(t, &fakeQdrant{}, nil)

	t.Run("trusted CA connects and sends the API key", func(t *testing.T) {
		require.NoError(t, pingFake(t, Config{Addr: tlsAddr, APIKey: "test-key", UseTLS: true, RootCAs: ca.pool}))
		assert.Equal(t, []string{"test-key"}, fake.receivedAPIKey())
	})

	t.Run("private CA without a CA setting is not trusted", func(t *testing.T) {
		err := pingFake(t, Config{Addr: tlsAddr, UseTLS: true})
		require.ErrorContains(t, err, "not signed by a CA this system trusts; set DIDEROT_QDRANT_CA_FILE")
	})

	t.Run("another CA is not trusted", func(t *testing.T) {
		err := pingFake(t, Config{Addr: tlsAddr, UseTLS: true, RootCAs: newTestCA(t).pool})
		require.ErrorContains(t, err, "not signed by a CA in DIDEROT_QDRANT_CA_FILE")
	})

	t.Run("TLS against a plaintext server", func(t *testing.T) {
		err := pingFake(t, Config{Addr: plainAddr, UseTLS: true, RootCAs: ca.pool})
		require.ErrorContains(t, err, "DIDEROT_QDRANT_TLS is on, but Qdrant at "+plainAddr+" does not speak TLS")
	})

	t.Run("plaintext against a TLS server", func(t *testing.T) {
		err := pingFake(t, Config{Addr: tlsAddr})
		require.ErrorContains(t, err, "DIDEROT_QDRANT_TLS is off, but Qdrant at "+tlsAddr+" requires TLS")
	})

	t.Run("certificate for another name", func(t *testing.T) {
		other := ca.serverCert(t, "qdrant.example")
		addr := serveFakeQdrant(t, &fakeQdrant{}, &other)
		_, port, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		err = pingFake(t, Config{Addr: "localhost:" + port, UseTLS: true, RootCAs: ca.pool})
		require.ErrorContains(t, err, "does not name that host; set DIDEROT_QDRANT_ADDR")
		require.ErrorContains(t, err, "qdrant.example")
	})

	t.Run("an unreachable server is not a TLS problem", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := listener.Addr().String()
		require.NoError(t, listener.Close())
		for _, useTLS := range []bool{false, true} {
			err := pingFake(t, Config{Addr: addr, UseTLS: useTLS})
			require.ErrorContains(t, err, "qdrantstore: health check")
			assert.NotContains(t, err.Error(), "DIDEROT_QDRANT")
		}
	})
}

func TestNewRejectsRootCAsWithoutTLS(t *testing.T) {
	_, err := New(Config{Addr: "localhost:6334", Collection: "c", RootCAs: x509.NewCertPool()}, prometheus.NewRegistry())
	require.ErrorContains(t, err, "RootCAs needs UseTLS")
}
