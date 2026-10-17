package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/soroworks/soroforge/internal/api"
	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubService satisfies api.Service without doing any work: these tests are
// about the transport, not the lifecycle.
type stubService struct{}

func (stubService) Deploy(context.Context, deploy.DeployRequest) (*deploy.DeployResult, error) {
	return &deploy.DeployResult{}, nil
}

func (stubService) Upgrade(context.Context, deploy.UpgradeRequest) (*deploy.UpgradeResult, error) {
	return &deploy.UpgradeResult{}, nil
}

func (stubService) Status(context.Context, deploy.StatusRequest) (*deploy.StatusResult, error) {
	return &deploy.StatusResult{}, nil
}

func (stubService) StatusAll(context.Context, string) (*deploy.NetworkStatus, error) {
	return &deploy.NetworkStatus{}, nil
}

func (stubService) List(context.Context, string) ([]store.Contract, error) {
	return []store.Contract{}, nil
}

func (stubService) History(context.Context, string, string) ([]store.Deployment, error) {
	return []store.Deployment{}, nil
}

func TestResolveTLSFiles(t *testing.T) {
	envPair := config.Env{TLSCert: "env-cert", TLSKey: "env-key"}

	tests := map[string]struct {
		flagCert  string
		flagKey   string
		env       config.Env
		wantCert  string
		wantKey   string
		wantError bool
	}{
		"flags are used when given": {
			flagCert: "flag-cert", flagKey: "flag-key", env: envPair,
			wantCert: "flag-cert", wantKey: "flag-key",
		},
		"environment pair is used when no flags": {
			env: envPair, wantCert: "env-cert", wantKey: "env-key",
		},
		"nothing configured is plaintext": {},
		"flag certificate alone": {
			flagCert: "flag-cert", env: envPair, wantError: true,
		},
		"flag key alone": {
			flagKey: "flag-key", env: envPair, wantError: true,
		},
		"environment certificate alone": {
			env: config.Env{TLSCert: "env-cert"}, wantError: true,
		},
		"environment key alone": {
			env: config.Env{TLSKey: "env-key"}, wantError: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cert, key, err := resolveTLSFiles(tc.flagCert, tc.flagKey, tc.env)
			if tc.wantError {
				require.Error(t, err)
				assert.ErrorContains(t, err, "both")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantCert, cert)
			assert.Equal(t, tc.wantKey, key)
		})
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	loopback := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "127.5.5.5:80"}
	for _, addr := range loopback {
		assert.True(t, isLoopbackAddr(addr), "%s is loopback", addr)
	}

	// Anything not provably loopback counts as exposed, so the plaintext
	// warning cannot be silenced by a name the check fails to parse.
	exposed := []string{
		":8080", "0.0.0.0:8080", "[::]:8080", "192.168.1.5:8080",
		"api.example.com:443", "not-an-address", "",
	}
	for _, addr := range exposed {
		assert.False(t, isLoopbackAddr(addr), "%s is not loopback", addr)
	}
}

// generateSelfSignedCert writes a fresh certificate/key pair to temporary
// files and returns their paths together with the certificate PEM.
func generateSelfSignedCert(t *testing.T) (certFile, keyFile string, certPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "soroforge-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))
	return certFile, keyFile, certPEM
}

func TestTLSConfigPinsMinimumVersion(t *testing.T) {
	certFile, keyFile, _ := generateSelfSignedCert(t)

	cfg, err := tlsConfig(certFile, keyFile)
	require.NoError(t, err)
	require.Len(t, cfg.Certificates, 1)
	assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion,
		"older protocol versions must be refused explicitly, not by default")
}

func TestTLSConfigRejectsMismatchedPair(t *testing.T) {
	certFile, _, _ := generateSelfSignedCert(t)
	_, keyFile, _ := generateSelfSignedCert(t)

	_, err := tlsConfig(certFile, keyFile)
	assert.ErrorContains(t, err, "load TLS certificate")
}

func TestServeWithSelfSignedCertificate(t *testing.T) {
	certFile, keyFile, certPEM := generateSelfSignedCert(t)

	tlsCfg, err := tlsConfig(certFile, keyFile)
	require.NoError(t, err)

	router, err := api.NewRouter(api.Options{
		Service: stubService{},
		Tokens:  []string{"test-token-long-enough-to-pass"},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := api.NewServer(ln.Addr().String(), router)
	srv.TLSConfig = tlsCfg

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() {
		_ = srv.Close()
		<-errCh
	})

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(certPEM))

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}

	resp, err := client.Get("https://" + ln.Addr().String() + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, resp.TLS.Version, uint16(tls.VersionTLS12),
		"the listener must not negotiate below TLS 1.2")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "ok")
}

func TestPlainHTTPIsUnchangedWhenTLSIsNotConfigured(t *testing.T) {
	srv := api.NewServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	require.Nil(t, srv.TLSConfig, "the HTTP code path must not pick up TLS by itself")
}
