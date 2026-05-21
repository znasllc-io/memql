package worker

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

const (
	// EnvWorkerCACertFile is an optional PEM-encoded CA bundle used
	// to validate the cluster's server certificate. When unset, the
	// system trust store applies. Useful for clusters fronted by a
	// private or self-signed CA.
	EnvWorkerCACertFile = "MEMQL_WORKER_CA_CERT_FILE"

	// EnvWorkerClientCertFile + EnvWorkerClientKeyFile enable mTLS
	// from the worker side. Both must be set together. The key file
	// MUST be 0600 (or stricter); a more-permissive mode is rejected
	// at load time so a misconfigured deployment can't silently leak
	// the private key to other local users.
	EnvWorkerClientCertFile = "MEMQL_WORKER_CLIENT_CERT_FILE"
	EnvWorkerClientKeyFile  = "MEMQL_WORKER_CLIENT_KEY_FILE"

	// EnvWorkerTLSInsecureSkipVerify is a dev-only escape hatch. When
	// set to "1", the TLS handshake skips hostname + chain validation
	// entirely. Production deployments must NEVER set this. Every
	// use emits a WARN log so accidental production toggles surface
	// in the operator's log shipper.
	EnvWorkerTLSInsecureSkipVerify = "MEMQL_WORKER_TLS_INSECURE_SKIP_VERIFY"
)

// BuildTLSConfig assembles the *tls.Config the worker SDK uses for
// gRPC dials.
//
// The base config:
//   - Always pins ServerName to the configured host so the certificate
//     hostname / SAN is verified against the expected cluster FQDN.
//     Without this, a successful TLS handshake against an attacker
//     endpoint that holds a valid-but-different certificate would pass
//     the system verifier.
//   - MinVersion: TLS 1.2 (matches Go's modern default and rules out
//     known-broken handshakes).
//
// Optional knobs (env-driven):
//   - MEMQL_WORKER_CA_CERT_FILE -- private/self-signed CA bundle.
//   - MEMQL_WORKER_CLIENT_CERT_FILE + MEMQL_WORKER_CLIENT_KEY_FILE -- mTLS.
//   - MEMQL_WORKER_TLS_INSECURE_SKIP_VERIFY=1 -- dev escape hatch.
//
// host is the dial address (host:port or bare hostname). The
// function strips the trailing :port when pinning ServerName.
func BuildTLSConfig(host string, logger *slog.Logger) (*tls.Config, error) {
	serverName := stripPort(strings.TrimSpace(host))
	if serverName == "" {
		return nil, errors.New("sdk/worker.tls: server name (hostname) is required")
	}

	cfg := &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}

	if v := strings.TrimSpace(os.Getenv(EnvWorkerCACertFile)); v != "" {
		pool, err := loadCAPool(v)
		if err != nil {
			return nil, fmt.Errorf("sdk/worker.tls: load CA bundle %s: %w", v, err)
		}
		cfg.RootCAs = pool
	}

	certPath := strings.TrimSpace(os.Getenv(EnvWorkerClientCertFile))
	keyPath := strings.TrimSpace(os.Getenv(EnvWorkerClientKeyFile))
	switch {
	case certPath != "" && keyPath != "":
		if err := verifyPrivateKeyFileMode(keyPath); err != nil {
			return nil, err
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("sdk/worker.tls: load client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	case certPath == "" && keyPath == "":
		// Neither set: no mTLS, fall through.
	default:
		return nil, fmt.Errorf("sdk/worker.tls: %s and %s must be set together",
			EnvWorkerClientCertFile, EnvWorkerClientKeyFile)
	}

	if strings.TrimSpace(os.Getenv(EnvWorkerTLSInsecureSkipVerify)) == "1" {
		cfg.InsecureSkipVerify = true
		if logger != nil {
			logger.Warn("worker TLS verification disabled via "+EnvWorkerTLSInsecureSkipVerify+"=1; production must leave this unset",
				"serverName", serverName,
			)
		}
	}

	return cfg, nil
}

// stripPort removes a trailing :port suffix if present. IPv6
// hostnames in dial-address form (`[::1]:443`) are not expected
// from the cluster-URL parser, so this is a simple split.
func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("no PEM-encoded certificates found")
	}
	return pool, nil
}

// verifyPrivateKeyFileMode rejects key files whose mode allows
// access beyond the owner. Group- or world-readable private keys
// are a well-known operator-error footgun (e.g. a `chmod -R 755`
// sweep over a secrets directory). Catch it at load time so the
// failure is loud instead of silent.
//
// Symlinks: os.Stat follows symlinks; the target's mode is what
// matters.
func verifyPrivateKeyFileMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("sdk/worker.tls: stat client key %s: %w", path, err)
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Errorf("sdk/worker.tls: client key %s has mode %04o; must be 0600 (group/other access forbidden)", path, mode)
	}
	return nil
}
