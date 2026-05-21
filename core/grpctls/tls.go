package grpctls

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
	// EnvGRPCTLSCertFile is the path to the PEM-encoded server
	// certificate the gRPC server presents to clients. When empty,
	// the server stays insecure (legacy default; suitable for
	// deployments behind a TLS-terminating proxy).
	EnvGRPCTLSCertFile = "MEMQL_GRPC_TLS_CERT_FILE"

	// EnvGRPCTLSKeyFile is the path to the PEM-encoded server key.
	// Required when EnvGRPCTLSCertFile is set. The file MUST be
	// mode 0600 (or stricter); more-permissive modes are rejected at
	// load time so a misconfigured deployment can't silently leak
	// the private key to other local users.
	EnvGRPCTLSKeyFile = "MEMQL_GRPC_TLS_KEY_FILE"

	// EnvGRPCTLSClientCAFile is the path to a PEM-encoded CA bundle
	// used to validate client certificates (mTLS). Empty means the
	// server doesn't request client certs.
	//
	// Doubles as the CA pool for inter-node outbound dials: when a
	// peer connection is established with TLS (controlled by
	// EnvGRPCTLSCertFile being set on this node), the dialer trusts
	// the system CA bundle by default; setting this env adds the
	// named CA pool on top. The same env var is reused so an
	// operator can stamp one CA file per cluster without juggling
	// server-side vs. client-side variants.
	EnvGRPCTLSClientCAFile = "MEMQL_GRPC_TLS_CLIENT_CA_FILE"

	// EnvGRPCRequireClientCert, when set to "1", makes the gRPC
	// server require + verify a client certificate. Only meaningful
	// when EnvGRPCTLSClientCAFile is also set; without a CA bundle
	// the server has no way to verify the client cert.
	EnvGRPCRequireClientCert = "MEMQL_GRPC_REQUIRE_CLIENT_CERT"
)

// LoadServerTLSConfig reads the MEMQL_GRPC_TLS_* env vars and
// returns the *tls.Config the gRPC server should use. Returns
// (nil, nil) when the cert + key pair is not configured -- the
// server should fall back to insecure transport in that case.
//
// Behavior:
//   - cert + key set    -> TLS enabled, MinVersion TLS 1.2.
//   - client CA set     -> server requests client certs; if
//     MEMQL_GRPC_REQUIRE_CLIENT_CERT=1, requires + verifies them.
//   - 0600 mode on key  -> enforced; rejected with a load-time error.
//
// The returned *tls.Config has GetCertificate set so the server
// can hot-reload the cert in a future iteration without restart.
// Today the cert is loaded once at startup.
func LoadServerTLSConfig(logger *slog.Logger) (*tls.Config, error) {
	certPath := strings.TrimSpace(os.Getenv(EnvGRPCTLSCertFile))
	keyPath := strings.TrimSpace(os.Getenv(EnvGRPCTLSKeyFile))
	switch {
	case certPath == "" && keyPath == "":
		return nil, nil
	case certPath == "" || keyPath == "":
		return nil, fmt.Errorf("grpc.tls: %s and %s must be set together (cert=%q key=%q)",
			EnvGRPCTLSCertFile, EnvGRPCTLSKeyFile, certPath, keyPath)
	}

	if err := verifyPrivateKeyFileMode(keyPath); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("grpc.tls: load server keypair: %w", err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	caPath := strings.TrimSpace(os.Getenv(EnvGRPCTLSClientCAFile))
	if caPath != "" {
		pool, err := loadCAPool(caPath)
		if err != nil {
			return nil, fmt.Errorf("grpc.tls: load client CA bundle %s: %w", caPath, err)
		}
		cfg.ClientCAs = pool
		if strings.TrimSpace(os.Getenv(EnvGRPCRequireClientCert)) == "1" {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
			if logger != nil {
				logger.Info("grpc TLS: mTLS enabled -- client certs required",
					"ca_file", caPath,
				)
			}
		} else {
			cfg.ClientAuth = tls.VerifyClientCertIfGiven
			if logger != nil {
				logger.Info("grpc TLS: mTLS optional -- client certs verified when presented",
					"ca_file", caPath,
				)
			}
		}
	} else if strings.TrimSpace(os.Getenv(EnvGRPCRequireClientCert)) == "1" {
		return nil, fmt.Errorf("grpc.tls: %s=1 requires %s to be set",
			EnvGRPCRequireClientCert, EnvGRPCTLSClientCAFile)
	}

	if logger != nil {
		logger.Info("grpc TLS: server cert configured",
			"cert_file", certPath,
			"client_auth", clientAuthLabel(cfg.ClientAuth),
		)
	}
	return cfg, nil
}

// LoadClientTLSConfig returns the *tls.Config an outbound inter-node
// dialer should use, pinned to the given peer hostname (serverName).
// Returns (nil, nil) when this node has no TLS configured -- the
// dialer falls back to insecure transport (matching the pre-#126
// behavior).
//
// The trust anchor is the system CA bundle plus, optionally, the
// CA pool loaded from EnvGRPCTLSClientCAFile. Same env var as the
// server side (the CA pool is symmetric: usually the same CA
// signs both ends of the mesh).
//
// When this node also presents a server cert (cert+key set), the
// dialer presents the same cert as a client cert for mTLS. Both
// sides of an inter-node connection authenticate.
func LoadClientTLSConfig(serverName string, logger *slog.Logger) (*tls.Config, error) {
	certPath := strings.TrimSpace(os.Getenv(EnvGRPCTLSCertFile))
	if certPath == "" {
		return nil, nil
	}
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return nil, errors.New("grpc.tls: client config requires a non-empty serverName")
	}

	cfg := &tls.Config{
		ServerName: stripPort(serverName),
		MinVersion: tls.VersionTLS12,
	}

	// Optional CA pool overlay.
	caPath := strings.TrimSpace(os.Getenv(EnvGRPCTLSClientCAFile))
	if caPath != "" {
		pool, err := loadCAPool(caPath)
		if err != nil {
			return nil, fmt.Errorf("grpc.tls: load CA bundle %s: %w", caPath, err)
		}
		cfg.RootCAs = pool
	}

	// Present a client cert if a server cert is also configured on
	// this node -- mTLS rides the same key material both ways.
	keyPath := strings.TrimSpace(os.Getenv(EnvGRPCTLSKeyFile))
	if keyPath != "" {
		if err := verifyPrivateKeyFileMode(keyPath); err != nil {
			return nil, err
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("grpc.tls: load client keypair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	if logger != nil {
		logger.Info("grpc TLS: client dial configured",
			"server_name", cfg.ServerName,
			"mtls", len(cfg.Certificates) > 0,
		)
	}
	return cfg, nil
}

// verifyPrivateKeyFileMode rejects key files whose mode allows
// access beyond the owner. Mirrors the sdk/go/worker discipline so
// the same 0600 floor applies to every memql-side TLS material.
func verifyPrivateKeyFileMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("grpc.tls: stat key %s: %w", path, err)
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Errorf("grpc.tls: key %s has mode %04o; must be 0600 (group/other access forbidden)", path, mode)
	}
	return nil
}

// loadCAPool reads a PEM-encoded CA bundle into an x509.CertPool.
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

// stripPort removes a trailing :port suffix if present. Used to
// derive the ServerName for client TLS pinning from a dial-address
// string (host:port form).
func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}

// clientAuthLabel returns a human-readable label for a tls.ClientAuthType
// value. Used by structured-log emissions.
func clientAuthLabel(t tls.ClientAuthType) string {
	switch t {
	case tls.NoClientCert:
		return "none"
	case tls.RequestClientCert:
		return "request"
	case tls.RequireAnyClientCert:
		return "require-any"
	case tls.VerifyClientCertIfGiven:
		return "verify-if-given"
	case tls.RequireAndVerifyClientCert:
		return "require-and-verify"
	default:
		return "unknown"
	}
}
