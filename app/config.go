package app

import (
	"context"
	"net/http"
	"os"

	"github.com/znasllc-io/memql/component/config"
	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/identity/verifier"
	"github.com/znasllc-io/memql/component/metrics"
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

	// Fail-fast required-var validation (#2108). Each node validates only
	// the vars its own MEMQL_NODE_TYPE requires (the multi-node default:
	// a voice pod must fail if the LiveKit vars are missing; a bff pod
	// must not). This runs FIRST so a misconfigured node exits with one
	// clear, actionable error instead of panicking deep in init. The
	// genesis envelope + local-env override have already been applied to
	// the process environment by main.go, so os.LookupEnv is the source
	// of truth here. The verifier's MEMQL_IDENTITY_VERIFIER_BASE_URL check
	// below is a separate, node-shape-specific gate and complements this.
	a.validateRequiredEnv()

	// Note: the engine's partition defaults to "default" via
	// MemQLEngine.Partition() and PartitionFromContext returns "" for
	// missing values, so no per-deployment env var is needed. Per-
	// request partition selection lives exclusively on the gRPC
	// envelope (MemqlClientMessage.partition).

	a.mux = http.NewServeMux()
	a.httpArgs = make([]server.ServerArg, 0, 2)
	a.httpArgs = append(a.httpArgs, server.WithBaseRouter(a.mux))
	// Prometheus scrape endpoint. Mounted on every node type (auth
	// rejects + JWKS keyset metrics live here, memql#1523). Public via
	// server.MetricsPaths(), so the verifier HTTP middleware below does
	// not gate it -- an in-cluster scrape can't present a bearer token.
	for _, p := range server.MetricsPaths() {
		a.handleRoute("GET "+p, metrics.Handler())
	}
	a.middlewares = make([]server.MiddlewareFunc, 0, 4)
	// Panic recovery is the outermost layer of the HTTP chain. A panic
	// in any downstream middleware or handler is caught here, logged
	// with stack + error id, and surfaced to the client as a 500
	// envelope -- never the raw panic value (which can include
	// sensitive locals from closures). Must be the first middleware
	// because the chain is composed back-to-front: middlewares[0]
	// wraps everything that follows.
	a.middlewares = append(a.middlewares, server.PanicRecoveryMiddleware(a.Logger))
	// Security headers (X-Frame-Options, X-Content-Type-Options,
	// Referrer-Policy, Permissions-Policy, HSTS-on-TLS) on every
	// HTTP response, not just the identity web UI. The identity web
	// stack still mounts its own CSP + frame-ancestors policy on
	// HTML routes; this middleware adds the API-safe headers
	// uniformly.
	a.middlewares = append(a.middlewares, server.SecurityHeadersMiddleware)

	if !verifierRequired {
		// Identity binary path: no per-node verifier, because identity is
		// the JWKS authority and must not verify against itself. Setting
		// MEMQL_IDENTITY_VERIFIER_BASE_URL on the identity binary is a
		// no-op -- short-circuit before the verifier-construction
		// path so identity does not try to fetch its own JWKS.
		//
		// Returning here means NO HTTP auth middleware is installed, so
		// server.PublicPaths() is never consulted on this binary and every
		// route HandlerWithOptions registers is reachable unauthenticated.
		//
		// This comment used to say identity "authenticates HTTP requests via
		// its own magic-link / OAuth / admin-session middleware (wired in
		// transportIdentity)". That is true of identity's OWN routes and false
		// as a statement about the binary's HTTP surface: transportIdentity
		// calls svc.RegisterRoutes(a.mux) and wraps nothing. A reader checking
		// whether identity was authenticated found a comment saying yes, in a
		// function that does not do it -- which is how two automations routes
		// went unauthenticated unnoticed (#2937, #2908).
		//
		// That surface is now declared rather than incidental: createHTTPServer
		// asserts the OpenAPI contract plus everything app code mounts via
		// a.handleRoute is in PublicPaths() or HandlerAuthorizedPaths(), and
		// refuses to boot otherwise (#2939). Seven paths on identity with no base
		// path configured; nine when SERVER_PUBLIC_PATH adds prefixed spellings.
		//
		// That is NOT every route this binary serves, and the difference is
		// exactly what the next reader needs: routes handed to the mux through
		// one of the enumerated handoffs -- svc.RegisterRoutes named above,
		// server.RegisterConceptsEndpoint -- are not covered, nor are routes
		// mounted by middleware ahead of the mux. See memql#3004. Overstating
		// the coverage here would repeat, in the same spot, the mistake this
		// very comment was rewritten to correct.
		a.Logger.Info("identity binary: per-node verifier intentionally disabled (identity is the JWKS authority); no HTTP auth middleware -- unauthenticated surface asserted in createHTTPServer")
		return
	}

	// Master auth toggle (MEMQL_IDENTITY_ENABLED, default true). On a
	// verifier-consuming node an explicit false DISABLES authentication for
	// troubleshooting: skip the verifier entirely and let transportBase
	// install the local-dev no-auth admit path (every request is treated as
	// the cluster owner). Loud + metered by design -- this must NEVER be set
	// in staging/production. The verifier URL stays configured (so fail-fast
	// env validation still passes); the toggle, not a blanked URL, is the
	// off switch.
	//
	// This takes the SAME early return as the identity binary above, so it has
	// the same consequence: no HTTP auth middleware, PublicPaths() never
	// consulted, routes reachable unauthenticated. createHTTPServer's assertion
	// covers this path too (#2939), but over the CONTRACT routes only -- not the
	// helper-registered routes as on identity.
	//
	// The reason is NOT that the undeclared routes are safe here. They are not,
	// and it is worth being exact, because a comfortable half-truth at this
	// return is what #2939 is about:
	//
	//   - "everything is the cluster owner" is true of gRPC and not of HTTP --
	//     the local-dev admit path is a STREAM interceptor.
	//   - attachments and audio do 401 on the missing actor, but polyphon's
	//     room-token and status handlers check nothing at all, and the gateway
	//     middleware sits AHEAD of the mux and admits POST /memql/query as the
	//     synthetic cluster owner.
	//
	// So on this node the HTTP surface is simply unauthenticated. That is the
	// toggle's documented meaning, which is why it is loudly warned and must
	// never be set in staging or production.
	//
	// The scope is contract-only because a per-route "this is safe
	// unauthenticated" declaration asserts a property that is false here by
	// construction -- nothing is authenticated in this mode -- and demanding it
	// would fail-fast a posture whose whole purpose is to run without auth. The
	// contract check still runs as a floor. The identity binary gets the wider
	// scope because there the declarations mean something: that node serves an
	// unauthenticated HTTP surface in PRODUCTION.
	if !config.IdentityAuthEnabled() {
		a.authDisabled = true
		metrics.SetAuthEnabled(false)
		a.Logger.Warn("SECURITY: authentication DISABLED on this node (MEMQL_IDENTITY_ENABLED=false) -- running local-dev no-auth identity; EVERY request is treated as cluster owner with no credential. Troubleshooting only; never enable in staging/production.")
		return
	}
	metrics.SetAuthEnabled(true)

	verifierCfg, err := verifier.LoadConfigFromEnv()
	if err != nil {
		a.fatal("failed to load identity verifier configuration", "error", err)
	}
	if err := verifierCfg.Validate(); err != nil {
		a.fatal("invalid identity verifier configuration", "error", err)
	}
	if !verifierCfg.Enabled() {
		a.fatal("identity verifier not configured: set MEMQL_IDENTITY_VERIFIER_BASE_URL to the identity service base URL")
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
		// Third tier (memql#3062): reachable without a memQL bearer because the
		// route verifies a vendor HMAC itself. Bounded to one path segment, and
		// only on a path the mux routes to that handler (memql#3128).
		SelfAuthenticatedPaths: server.SelfAuthenticatedPaths(),
		UnauthorizedHandler: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		},
	})
	a.middlewares = append(a.middlewares, authMiddleware)
	a.Logger.Info("identity verifier HTTP middleware configured")
}

// validateRequiredEnv loads the secrets/variable registry and exits the
// process via a.fatal with ONE actionable error if any var this node
// type requires at boot is missing. Keyed on MEMQL_NODE_TYPE so each
// node in the mesh validates only its own required set (#2108). A
// manifest that cannot be loaded is treated as non-fatal here -- the
// embedded snapshot always loads, so a load error means a caller-
// supplied override path is broken, which is logged and skipped rather
// than blocking boot on the validator itself.
func (a *App) validateRequiredEnv() {
	manifest, err := genesis.LoadManifest("")
	if err != nil {
		a.Logger.Warn("boot var validation skipped: could not load secrets registry", "error", err)
		return
	}
	nodeType := genesis.ResolveNodeType()
	missing := genesis.MissingRequired(nodeType, manifest, os.LookupEnv)
	if msg := genesis.MissingRequiredError(nodeType, missing); msg != "" {
		a.fatal(msg, "node_type", nodeType, "missing", missing)
	}
}
