package install

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real, independently signed CAs and leaf certificates. Only mkcert's machine
// trust-store write/issuance is stubbed; OpenSSL validates actual key material.
func fixtureCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func fixtureLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, expires time.Time) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: expires,
		DNSNames: []string{"*.memql.localhost", "memql.localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeTLSFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestMkcertReusesOnlyAValidPairFromTheCurrentCA(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl required")
	}
	current, currentKey, currentPEM := fixtureCA(t, "current MemQL CA")
	former, formerKey, formerPEM := fixtureCA(t, "previous MemQL CA")
	validCert, validKey := fixtureLeaf(t, current, currentKey, time.Now().Add(time.Hour))
	oldCert, oldKey := fixtureLeaf(t, former, formerKey, time.Now().Add(time.Hour))
	expiredCert, expiredKey := fixtureLeaf(t, current, currentKey, time.Now().Add(-time.Minute))
	for _, tc := range []struct {
		name      string
		cert, key []byte
		reissue   bool
	}{
		{"healthy", validCert, validKey, false},
		{"same names but previous CA", oldCert, oldKey, true},
		{"same CA but expired", expiredCert, expiredKey, true},
		{"same CA but wrong key", validCert, oldKey, true},
		{"corrupt certificate", []byte("interrupted certificate write"), validKey, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newMkcertEnv(t)
			writeTLSFixture(t, e.caPEM(), currentPEM)
			writeTLSFixture(t, e.certFile(), tc.cert)
			writeTLSFixture(t, e.keyFile(), tc.key)
			issued := t.TempDir()
			writeTLSFixture(t, filepath.Join(issued, "cert.pem"), validCert)
			writeTLSFixture(t, filepath.Join(issued, "key.pem"), validKey)
			// Even a formerly trusted CA must not authorize reuse. -trusted must
			// exclude default/system trust stores when checking this install's CA.
			writeTLSFixture(t, filepath.Join(issued, "former-ca.pem"), formerPEM)
			e.extra = append(e.extra,
				"STUB_ISSUED_CERT="+filepath.Join(issued, "cert.pem"),
				"STUB_ISSUED_KEY="+filepath.Join(issued, "key.pem"),
				"SSL_CERT_FILE="+filepath.Join(issued, "former-ca.pem"))
			env, code, out := e.run(t)
			if code != 0 || !env.OK {
				t.Fatalf("exit %d: %s", code, out)
			}
			if mkcertBool(t, env, "certIssued") != tc.reissue || env.Changed != tc.reissue {
				t.Fatalf("reissue=%v: %s", tc.reissue, out)
			}
			caAfter, err := os.ReadFile(e.caPEM())
			if err != nil || !bytes.Equal(caAfter, currentPEM) {
				t.Fatal("repair changed the CA")
			}
			pair, err := tls.LoadX509KeyPair(e.certFile(), e.keyFile())
			if err != nil {
				t.Fatalf("result is not a usable TLS pair: %v", err)
			}
			leaf, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			roots.AddCert(current)
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "api.memql.localhost"}); err != nil {
				t.Fatalf("repaired certificate is still untrusted: %v", err)
			}
			calls := strings.Count(e.stubLog(t), "-cert-file")
			again, code, out := e.run(t)
			if code != 0 || again.Changed || strings.Count(e.stubLog(t), "-cert-file") != calls {
				t.Fatalf("repair did not converge on a reusable pair: %s", out)
			}
		})
	}
}

func TestMkcertRejectsAnInvalidIssuedPair(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl required")
	}
	e := newMkcertEnv(t)
	_, _, ca := fixtureCA(t, "current CA")
	writeTLSFixture(t, e.caPEM(), ca)
	// The default stub issues non-PEM bytes. A successful process alone is
	// insufficient evidence that the new certificate is usable.
	env, code, out := e.run(t)
	if code != 5 || env.OK {
		t.Fatalf("invalid issuance accepted: exit %d: %s", code, out)
	}
}
