package app

import (
	"context"
	"net/http"

	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/server"
)

// configAndAuth loads service configuration, initializes the per-node
// identity-verifier (JWKS-fetched JWT validator), sets up the base
// HTTP mux, and configures auth middleware.
//
// On verifier-consuming nodes (bff, voice, cognition, agent, planner,
// default -- everything where verifierRequired=true), IDENTITY_VERIFIER_
// BASE_URL is required. Every gRPC call carries an identity-issued
// JWT that the per-node verifier validates against the identity
// service's JWKS feed. Out-of-band operator tooling
// (`make secrets-seed` etc.) authenticates via the operator
// credential -- see operator_stream_interceptor.go.
//
// On the identity binary (verifierRequired=false), configAndAuth
// skips the verifier wiring entirely. Identity IS the JWKS
// authority -- it does not verify against itself; its own auth path
// is HTTP-based (magic-link / OAuth / admin session) and the gRPC
// surface is consumed only by operator tooling.
func (a *App) configAndAuth() {
	_, err := a.overrides.LoadServiceEnvOpt()
	if err != nil {
		a.fatal("failed to load service environment options", "error", err)
	}

	// Note: the engine's partition defaults to "default" via
	// MemQLEngine.Partition() and PartitionFromContext returns "" for
	// missing values, so no per-deployment env var is needed. Per-
	// request partition selection lives exclusively on the gRPC
	// envelope (MemqlClientMessage.partition).

	a.mux = http.NewServeMux()
	a.httpArgs = make([]server.ServerArg, 0, 2)
	a.httpArgs = append(a.httpArgs, server.WithBaseRouter(a.mux))
	a.middlewares = make([]server.MiddlewareFunc, 0, 2)

	if !verifierRequired {
		// Identity binary path: no per-node verifier; identity is
		// the JWKS authority and authenticates HTTP requests via
		// its own magic-link / OAuth / admin-session middleware
		// (wired in transportIdentity). Setting
		// IDENTITY_VERIFIER_BASE_URL on the identity binary is a
		// no-op -- short-circuit before the verifier-construction
		// path so identity does not try to fetch its own JWKS.
		a.Logger.Info("identity binary: per-node verifier intentionally disabled (identity is the JWKS authority)")
		return
	}
	verifierCfg, err := verifier.LoadConfigFromEnv()
	if err != nil {
		a.fatal("failed to load identity verifier configuration", "error", err)
	}
	if err := verifierCfg.Validate(); err != nil {
		a.fatal("invalid identity verifier configuration", "error", err)
	}
	if !verifierCfg.Enabled() {
		a.fatal("identity verifier not configured: set IDENTITY_VERIFIER_BASE_URL to the identity service base URL")
	}

	cache, err := verifier.NewJWKSCache(verifierCfg, a.Logger)
	if err != nil {
		a.fatal("failed to construct identity verifier JWKS cache", "error", err, "jwks_url", verifierCfg.EffectiveJWKSURL())
	}
	// Background refresh runs for the lifetime of the process.
	// Using context.Background here is intentional — the
	// goroutine is naturally bounded by process lifetime, and the
	// lower-level HTTP client carries its own per-request timeout.
	cache.Start(context.Background())

	// PAT verification on bff/voice/etc. is not wired today; the
	// identity service is the authority that holds the PAT store.
	// nil here means tokens with the mql_pat_ prefix are rejected
	// outright on these nodes — they only ever appear when a CLI
	// client hits the identity binary directly.
	v, err := verifier.New(verifierCfg, cache, nil, a.Logger)
	if err != nil {
		a.fatal("failed to construct identity verifier", "error", err)
	}
	a.identityVerifier = v
	a.Logger.Info("identity verifier enabled",
		"jwks_url", verifierCfg.EffectiveJWKSURL(),
		"expected_issuer", verifierCfg.EffectiveIssuer(),
		"expected_audience", verifierCfg.EffectiveAudience(),
	)

	authMiddleware := verifier.HTTPMiddleware(a.identityVerifier, verifier.MiddlewareOptions{
		Logger:      a.Logger,
		PublicPaths: server.PublicPaths(),
		UnauthorizedHandler: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		},
	})
	a.middlewares = append(a.middlewares, authMiddleware)
	a.Logger.Info("identity verifier HTTP middleware configured")
}
