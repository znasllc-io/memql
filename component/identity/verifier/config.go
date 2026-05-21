// Package verifier holds the per-node JWT verifier that bff, voice,
// cognition, agent, and planner binaries use to validate access
// tokens minted by the identity service.
//
// The verifier fetches the identity service's JWKS document
// (`${IDENTITY_VERIFIER_BASE_URL}/.well-known/jwks.json`), caches the
// public keys by `kid`, and validates incoming bearer tokens against
// the cached set. PAT tokens (mql_pat_*) are routed through the
// optional pat.Verifier; JWTs are validated with the EdDSA public
// key whose kid matches the token header.
//
// Identity-tagged binaries do not use this package — they own the
// signing-key material directly.
package verifier

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Default values for env-driven configuration.
const (
	// DefaultJWKSRefreshSeconds is the polling cadence the cache uses
	// to refresh the public keyset in the background. The cache also
	// refreshes on demand when an unknown kid arrives — this controls
	// the steady-state cadence so a rotation that happens between
	// requests is picked up promptly.
	DefaultJWKSRefreshSeconds = 300 // 5 min

	// DefaultJWKSFetchTimeoutSeconds caps a single JWKS HTTP fetch.
	DefaultJWKSFetchTimeoutSeconds = 10

	// DefaultRevocationCheckSeconds is the in-stream re-check cadence
	// for the bulk-revoke epoch (#106). Streams that ran past this
	// many seconds since the last check resolve the user's current
	// revocationEpoch and cancel the stream context if the token's
	// claim is now stale. Per-stream goroutine; cheap.
	DefaultRevocationCheckSeconds = 300 // 5 min
)

// DefaultJWTAudience must match identity.DefaultJWTAudience. We
// duplicate the literal here rather than import component/identity
// to keep the verifier package importable from non-identity-tagged
// builds (identity binaries pull in audit logging, key files, etc.
// — none of which a verifier-only consumer should be linking).
const DefaultJWTAudience = "memql"

// Config captures the per-node verifier configuration. All fields are
// loaded from `IDENTITY_VERIFIER_*` env vars by LoadConfigFromEnv.
//
// The verifier is enabled when BaseURL is set. Empty BaseURL means
// "no auth" — the binary runs in dev mode with the synthetic local
// admin identity and no inbound token verification.
type Config struct {
	// BaseURL is the public origin of the identity service. The
	// verifier fetches `${BaseURL}/.well-known/jwks.json` and (unless
	// ExpectedIssuer is overridden) treats BaseURL as the JWT `iss`.
	// Env: IDENTITY_VERIFIER_BASE_URL
	BaseURL string

	// ExpectedIssuer is the value the verifier compares against the
	// JWT `iss` claim. Defaults to BaseURL when empty. Override only
	// when the public-facing identity URL differs from the URL placed
	// in tokens (e.g. internal-mesh routing via a different hostname).
	// Env: IDENTITY_VERIFIER_EXPECTED_ISSUER
	ExpectedIssuer string

	// ExpectedAudience is the value the verifier compares against the
	// JWT `aud` claim. Defaults to "memql".
	// Env: IDENTITY_VERIFIER_AUDIENCE
	ExpectedAudience string

	// JWKSRefreshInterval drives the background refresh cadence.
	// Env: IDENTITY_VERIFIER_JWKS_REFRESH_SECONDS (default 300)
	JWKSRefreshInterval time.Duration

	// JWKSFetchTimeout caps a single JWKS HTTP fetch.
	// Env: IDENTITY_VERIFIER_JWKS_FETCH_TIMEOUT_SECONDS (default 10)
	JWKSFetchTimeout time.Duration

	// JWKSURL overrides the auto-derived JWKS endpoint. Optional;
	// useful when the verifier should hit an internal-mesh URL
	// rather than the public BaseURL.
	// Env: IDENTITY_VERIFIER_JWKS_URL
	JWKSURL string

	// RevocationCheckInterval drives the in-stream revocation-epoch
	// re-check cadence (#106). Per-stream goroutine; cheap. Zero falls
	// back to DefaultRevocationCheckSeconds. The interval is also the
	// granularity of bulk-revoke effectiveness: a token bumped at
	// time T stays usable on already-open streams up to T+interval.
	// Env: IDENTITY_VERIFIER_REVOCATION_CHECK_SECONDS (default 300)
	RevocationCheckInterval time.Duration
}

// Enabled reports whether the verifier should be wired in. Returns
// true when BaseURL is set; false signals dev/no-auth mode.
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.BaseURL) != ""
}

// EffectiveIssuer returns the issuer value the verifier compares
// against the JWT `iss` claim — ExpectedIssuer when set, otherwise
// BaseURL.
func (c Config) EffectiveIssuer() string {
	if v := strings.TrimSpace(c.ExpectedIssuer); v != "" {
		return v
	}
	return strings.TrimRight(c.BaseURL, "/")
}

// EffectiveAudience returns the audience value the verifier compares
// against the JWT `aud` claim — ExpectedAudience when set, otherwise
// the package default.
func (c Config) EffectiveAudience() string {
	if v := strings.TrimSpace(c.ExpectedAudience); v != "" {
		return v
	}
	return DefaultJWTAudience
}

// EffectiveJWKSURL returns the URL the cache fetches. JWKSURL when
// set, otherwise BaseURL + "/.well-known/jwks.json".
func (c Config) EffectiveJWKSURL() string {
	if v := strings.TrimSpace(c.JWKSURL); v != "" {
		return v
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return ""
	}
	return base + "/.well-known/jwks.json"
}

// Validate confirms the config is internally consistent when enabled.
// Returns nil for the disabled (dev/no-auth) case.
func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("IDENTITY_VERIFIER_BASE_URL must be an absolute URL (got %q)", c.BaseURL)
	}
	if c.JWKSRefreshInterval <= 0 {
		return errors.New("IDENTITY_VERIFIER_JWKS_REFRESH_SECONDS must be > 0")
	}
	if c.JWKSFetchTimeout <= 0 {
		return errors.New("IDENTITY_VERIFIER_JWKS_FETCH_TIMEOUT_SECONDS must be > 0")
	}
	return nil
}

// LoadConfigFromEnv reads IDENTITY_VERIFIER_* env vars into a Config.
// Missing values fall back to the defaults declared at the top of
// this file. Returns an error only for malformed values.
func LoadConfigFromEnv() (Config, error) {
	cfg := Config{
		BaseURL:          strings.TrimRight(os.Getenv("IDENTITY_VERIFIER_BASE_URL"), "/"),
		ExpectedIssuer:   strings.TrimRight(os.Getenv("IDENTITY_VERIFIER_EXPECTED_ISSUER"), "/"),
		ExpectedAudience: os.Getenv("IDENTITY_VERIFIER_AUDIENCE"),
		JWKSURL:          os.Getenv("IDENTITY_VERIFIER_JWKS_URL"),
	}
	cfg.JWKSRefreshInterval = envDurationSeconds("IDENTITY_VERIFIER_JWKS_REFRESH_SECONDS", DefaultJWKSRefreshSeconds)
	cfg.JWKSFetchTimeout = envDurationSeconds("IDENTITY_VERIFIER_JWKS_FETCH_TIMEOUT_SECONDS", DefaultJWKSFetchTimeoutSeconds)
	cfg.RevocationCheckInterval = envDurationSeconds("IDENTITY_VERIFIER_REVOCATION_CHECK_SECONDS", DefaultRevocationCheckSeconds)
	return cfg, nil
}

func envDurationSeconds(key string, fallbackSeconds int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(fallbackSeconds) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return time.Duration(fallbackSeconds) * time.Second
	}
	return time.Duration(n) * time.Second
}
