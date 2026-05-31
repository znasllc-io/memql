// Package httptls provides the HTTP-side companion to core/grpctls:
// optional TLS for the memQL HTTP server and a CA-overlay HTTP client
// for nodes that talk to the identity service over an internal
// (self-signed) CA.
//
// Why this exists: the identity service's POST /node/bootstrap and the
// JWKS feed at /.well-known/jwks.json must be reachable over TLS (the
// bootstrap handler rejects plaintext with 403 insecure_transport). On
// a cluster without a TLS-terminating proxy in front of identity, the
// identity HTTP server has to serve TLS itself, and every other node's
// outbound HTTP client (bootstrap + JWKS verifier) has to trust the
// internal CA. gRPC mesh transport is a separate concern (core/grpctls)
// and stays insecure + JWT-authenticated.
//
// Env vars (all optional; unset == legacy plaintext behaviour):
//   - MEMQL_HTTP_TLS_CERT_FILE / MEMQL_HTTP_TLS_KEY_FILE: PEM server
//     cert + key. Set together on the node that serves TLS (identity).
//   - MEMQL_HTTP_TLS_CA_FILE: PEM CA bundle to trust on outbound HTTP
//     calls, overlaid on the system roots. Set on every node that
//     dials the identity service over https.
package httptls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// EnvHTTPTLSCertFile is the path to the PEM-encoded server cert the
	// HTTP server presents. When empty (and key empty too) the server
	// stays plaintext -- the legacy default, fine behind a
	// TLS-terminating proxy / public LB.
	EnvHTTPTLSCertFile = "MEMQL_HTTP_TLS_CERT_FILE"

	// EnvHTTPTLSKeyFile is the path to the PEM-encoded server key.
	// Required when EnvHTTPTLSCertFile is set.
	EnvHTTPTLSKeyFile = "MEMQL_HTTP_TLS_KEY_FILE"

	// EnvHTTPTLSCAFile is the path to a PEM CA bundle trusted (on top
	// of the system roots) by outbound HTTP clients built via Client.
	// Set on nodes that call the identity service over an internal
	// self-signed CA.
	EnvHTTPTLSCAFile = "MEMQL_HTTP_TLS_CA_FILE"
)

// ServerCertFiles returns the configured server cert + key paths and
// whether TLS is enabled. Returns enabled=false when neither var is set
// (plaintext). Returns an error when exactly one of the pair is set.
func ServerCertFiles() (certFile, keyFile string, enabled bool, err error) {
	certFile = strings.TrimSpace(os.Getenv(EnvHTTPTLSCertFile))
	keyFile = strings.TrimSpace(os.Getenv(EnvHTTPTLSKeyFile))
	switch {
	case certFile == "" && keyFile == "":
		return "", "", false, nil
	case certFile == "" || keyFile == "":
		return "", "", false, fmt.Errorf("http.tls: %s and %s must be set together (cert=%q key=%q)",
			EnvHTTPTLSCertFile, EnvHTTPTLSKeyFile, certFile, keyFile)
	}
	return certFile, keyFile, true, nil
}

// internalCARootPool returns an x509 pool = system roots + the CA at
// EnvHTTPTLSCAFile. Returns (nil, false, nil) when the CA file is unset
// (caller should use the default transport / system roots only).
func internalCARootPool() (*x509.CertPool, bool, error) {
	caPath := strings.TrimSpace(os.Getenv(EnvHTTPTLSCAFile))
	if caPath == "" {
		return nil, false, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// Fall back to an empty pool so the internal CA still works
		// even on an image without a system bundle.
		pool = x509.NewCertPool()
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, false, fmt.Errorf("http.tls: read CA bundle %s: %w", caPath, err)
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, false, fmt.Errorf("http.tls: no PEM certificates found in %s", caPath)
	}
	return pool, true, nil
}

// Client returns an *http.Client that trusts the system roots plus the
// internal CA from EnvHTTPTLSCAFile (when set). When the CA file is
// unset it returns a plain client with the given timeout (system roots
// only) -- so callers can use this unconditionally and get the right
// behaviour whether or not internal TLS is configured. A zero timeout
// leaves the client unbounded (rely on the request context).
func Client(timeout time.Duration, logger *slog.Logger) (*http.Client, error) {
	pool, ok, err := internalCARootPool()
	if err != nil {
		return nil, err
	}
	if !ok {
		return &http.Client{Timeout: timeout}, nil
	}
	if logger != nil {
		logger.Info("http.tls: outbound client trusting internal CA overlay",
			"ca_file", strings.TrimSpace(os.Getenv(EnvHTTPTLSCAFile)),
		)
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
			},
		},
	}, nil
}
