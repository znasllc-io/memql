package grpctls

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeSelfSignedCertKey writes a fresh self-signed cert + key to
// the given dir at the given mode (key file). Returns the cert and
// key paths. Used by the TLS-config tests so we don't need to
// bundle a fixture cert in the repo.
func makeSelfSignedCertKey(t *testing.T, dir string, keyMode os.FileMode) (certPath, keyPath string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "memql.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:         true,
		DNSNames:     []string{"memql.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	certPath = filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	keyDER := x509.MarshalPKCS1PrivateKey(priv)
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER}), keyMode); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// resetEnv clears every TLS env var so each test starts from a
// known baseline. t.Setenv("", "") doesn't unset; we use t.Setenv
// with an empty string which has the same effect on os.Getenv
// reads (matchesSecretName-style "trim and check empty").
func resetEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvGRPCTLSCertFile, "")
	t.Setenv(EnvGRPCTLSKeyFile, "")
	t.Setenv(EnvGRPCTLSClientCAFile, "")
	t.Setenv(EnvGRPCRequireClientCert, "")
}

// TestLoadServerTLSConfig_NoEnvReturnsNil pins the legacy default:
// when no TLS env vars are set, the server gets (nil, nil) and
// stays insecure.
func TestLoadServerTLSConfig_NoEnvReturnsNil(t *testing.T) {
	resetEnv(t)
	cfg, err := LoadServerTLSConfig(discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil config when env unset, got %+v", cfg)
	}
}

// TestLoadServerTLSConfig_CertAndKeyLoads confirms a happy-path
// cert+key load returns a usable *tls.Config pinned to TLS 1.2.
func TestLoadServerTLSConfig_CertAndKeyLoads(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	certPath, keyPath := makeSelfSignedCertKey(t, dir, 0o600)
	t.Setenv(EnvGRPCTLSCertFile, certPath)
	t.Setenv(EnvGRPCTLSKeyFile, keyPath)

	cfg, err := LoadServerTLSConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadServerTLSConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil cfg")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates len = %d, want 1", len(cfg.Certificates))
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert (no CA configured)", cfg.ClientAuth)
	}
}

// TestLoadServerTLSConfig_RejectsHalfConfigured: cert without key
// (and vice versa) is a load-time error.
func TestLoadServerTLSConfig_RejectsHalfConfigured(t *testing.T) {
	resetEnv(t)
	t.Setenv(EnvGRPCTLSCertFile, "/tmp/cert.pem")
	t.Setenv(EnvGRPCTLSKeyFile, "")

	_, err := LoadServerTLSConfig(discardLogger())
	if err == nil {
		t.Fatal("expected error when only cert is set")
	}
	if !strings.Contains(err.Error(), "must be set together") {
		t.Errorf("error %q does not mention must-be-set-together", err.Error())
	}
}

// TestLoadServerTLSConfig_RejectsPermissiveKeyMode: 0644 key file
// fails the 0600 check at load.
func TestLoadServerTLSConfig_RejectsPermissiveKeyMode(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	certPath, keyPath := makeSelfSignedCertKey(t, dir, 0o644)
	t.Setenv(EnvGRPCTLSCertFile, certPath)
	t.Setenv(EnvGRPCTLSKeyFile, keyPath)

	_, err := LoadServerTLSConfig(discardLogger())
	if err == nil {
		t.Fatal("expected rejection for 0644 key")
	}
	if !strings.Contains(err.Error(), "must be 0600") {
		t.Errorf("error %q does not name the required mode", err.Error())
	}
}

// TestLoadServerTLSConfig_RequireClientCertNeedsCA: setting
// REQUIRE_CLIENT_CERT=1 without a CA bundle is an error -- without
// a CA the server has nothing to verify against.
func TestLoadServerTLSConfig_RequireClientCertNeedsCA(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	certPath, keyPath := makeSelfSignedCertKey(t, dir, 0o600)
	t.Setenv(EnvGRPCTLSCertFile, certPath)
	t.Setenv(EnvGRPCTLSKeyFile, keyPath)
	t.Setenv(EnvGRPCRequireClientCert, "1")

	_, err := LoadServerTLSConfig(discardLogger())
	if err == nil {
		t.Fatal("expected error when require-client-cert is set without CA")
	}
	if !strings.Contains(err.Error(), "requires "+EnvGRPCTLSClientCAFile) {
		t.Errorf("error %q does not point at the required env var", err.Error())
	}
}

// TestLoadServerTLSConfig_RequireClientCertHappyPath: with both
// CA + REQUIRE_CLIENT_CERT, ClientAuth is RequireAndVerifyClientCert.
func TestLoadServerTLSConfig_RequireClientCertHappyPath(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	certPath, keyPath := makeSelfSignedCertKey(t, dir, 0o600)
	t.Setenv(EnvGRPCTLSCertFile, certPath)
	t.Setenv(EnvGRPCTLSKeyFile, keyPath)
	t.Setenv(EnvGRPCTLSClientCAFile, certPath) // self-signed cert doubles as CA for this test
	t.Setenv(EnvGRPCRequireClientCert, "1")

	cfg, err := LoadServerTLSConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadServerTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs is nil")
	}
}

// TestLoadServerTLSConfig_OptionalMTLSWhenCAWithoutRequire: with
// CA set but no REQUIRE_CLIENT_CERT, ClientAuth is
// VerifyClientCertIfGiven -- the server allows but doesn't demand
// client certs.
func TestLoadServerTLSConfig_OptionalMTLSWhenCAWithoutRequire(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	certPath, keyPath := makeSelfSignedCertKey(t, dir, 0o600)
	t.Setenv(EnvGRPCTLSCertFile, certPath)
	t.Setenv(EnvGRPCTLSKeyFile, keyPath)
	t.Setenv(EnvGRPCTLSClientCAFile, certPath)

	cfg, err := LoadServerTLSConfig(discardLogger())
	if err != nil {
		t.Fatalf("LoadServerTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuth = %v, want VerifyClientCertIfGiven", cfg.ClientAuth)
	}
}

// TestLoadClientTLSConfig_NoEnvReturnsNil: matches the server-side
// default -- when this node has no TLS, the dialer falls back to
// insecure.
func TestLoadClientTLSConfig_NoEnvReturnsNil(t *testing.T) {
	resetEnv(t)
	cfg, err := LoadClientTLSConfig("memql.test:50050", discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Errorf("expected nil config when env unset, got %+v", cfg)
	}
}

// TestLoadClientTLSConfig_PinsServerName: the dial config's
// ServerName is stripped of the port and set to the hostname.
func TestLoadClientTLSConfig_PinsServerName(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	certPath, _ := makeSelfSignedCertKey(t, dir, 0o600)
	t.Setenv(EnvGRPCTLSCertFile, certPath)

	cfg, err := LoadClientTLSConfig("memql.test:50050", discardLogger())
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil cfg")
	}
	if cfg.ServerName != "memql.test" {
		t.Errorf("ServerName = %q, want memql.test", cfg.ServerName)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
}

// TestLoadClientTLSConfig_PresentsClientCertWhenKeyConfigured:
// when this node has both cert + key, the dial config presents the
// pair as a client cert (mTLS rides the same key material both ways).
func TestLoadClientTLSConfig_PresentsClientCertWhenKeyConfigured(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	certPath, keyPath := makeSelfSignedCertKey(t, dir, 0o600)
	t.Setenv(EnvGRPCTLSCertFile, certPath)
	t.Setenv(EnvGRPCTLSKeyFile, keyPath)

	cfg, err := LoadClientTLSConfig("memql.test:50050", discardLogger())
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates len = %d, want 1 (mTLS expected)", len(cfg.Certificates))
	}
}

// TestLoadClientTLSConfig_LoadsCAPool: when MEMQL_GRPC_TLS_CLIENT_CA_FILE
// is set, the dialer trusts the named pool in addition to the
// system store.
func TestLoadClientTLSConfig_LoadsCAPool(t *testing.T) {
	resetEnv(t)
	dir := t.TempDir()
	certPath, _ := makeSelfSignedCertKey(t, dir, 0o600)
	t.Setenv(EnvGRPCTLSCertFile, certPath)
	t.Setenv(EnvGRPCTLSClientCAFile, certPath)

	cfg, err := LoadClientTLSConfig("memql.test:50050", discardLogger())
	if err != nil {
		t.Fatalf("LoadClientTLSConfig: %v", err)
	}
	if cfg.RootCAs == nil {
		t.Error("RootCAs is nil; expected the configured pool to be loaded")
	}
}

// TestStripPort matches the sdk/go/worker shape so the same idiom
// applies on both sides.
func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"host":           "host",
		"host:443":       "host",
		"memql.test:50050": "memql.test",
		"":               "",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestVerifyPrivateKeyFileMode_AcceptsAndRejects covers the 0600
// floor.
func TestVerifyPrivateKeyFileMode_AcceptsAndRejects(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.key")
	if err := os.WriteFile(good, []byte("x"), 0o600); err != nil {
		t.Fatalf("write good: %v", err)
	}
	if err := verifyPrivateKeyFileMode(good); err != nil {
		t.Errorf("0600 key rejected: %v", err)
	}

	bad := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(bad, []byte("x"), 0o644); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	if err := verifyPrivateKeyFileMode(bad); err == nil {
		t.Error("0644 key accepted; expected rejection")
	}
}
