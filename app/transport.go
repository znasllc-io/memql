package app

import (
	"os"
	"strings"

	memqlgrpc "github.com/znasllc-io/memql/component/grpc"
	"github.com/znasllc-io/memql/component/identity/verifier"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/component/server/memqlws"
)

// transportBase creates the gRPC server, WebSocket bridge, and gateway middleware.
// Called by all tag-specific transport methods before wiring their endpoints.
func (a *App) transportBase() {
	// === gRPC Server ===
	memqlGRPCAddr := strings.TrimSpace(os.Getenv("MEMQL_GRPC_ADDRESS"))
	a.grpcServer = memqlgrpc.NewServer(memqlGRPCAddr, a.Logger)
	a.grpcServer.SetEngine(a.engine)
	a.grpcServer.SetEventBus(a.eventBus)
	a.grpcServer.SetWiring(a.wiring)
	a.grpcServer.SetConceptRegistry(a.registry)

	// MemQL Sense -- language intelligence service.
	senseAdapter := memqlengine.NewSenseAdapter(a.engine)
	a.grpcServer.SetSense(sense.New(senseAdapter))

	// Configure gRPC authentication. The interceptor chain reads
	// outside-in:
	//
	//   operator-aware -- Authorization: Operator <master_key>
	//                   admits out-of-band operator tooling
	//                   (`make secrets-seed`, `scripts/secrets
	//                   health`) as a synthetic cluster owner.
	//                   Falls through on any other scheme.
	//   pair-aware    -- Authorization: Pair <code> redeems a
	//                   worker pairing code without an existing
	//                   JWT. Falls through on any other scheme.
	//   guest-aware   -- Authorization: Guest <token> admits an
	//                   invitation-bearing visitor.
	//   session-revocation -- looks up the bearer's tokenHash in
	//                   v1:identity:authSession and rejects
	//                   revoked tokens.
	//   base          -- identity-issued JWT verification via JWKS.
	//
	// On the identity binary the chain collapses to operator-aware
	// alone. Identity is the JWKS authority; its gRPC surface
	// exists for operator tooling, not for end-user clients. End
	// users authenticate through the HTTP magic-link / OAuth /
	// admin paths wired by transportIdentity.
	if a.identityVerifier != nil {
		base := verifier.StreamInterceptor(a.identityVerifier, a.Logger)
		sessionChecked := memqlgrpc.NewSessionRevocationStreamInterceptor(base, a.engine, a.Logger)
		guestChecked := memqlgrpc.NewGuestAwareStreamInterceptor(sessionChecked, a.engine, a.Logger)
		operatorChecked := memqlgrpc.NewOperatorAwareStreamInterceptor(guestChecked, a.Logger)
		// Voice-agent (Initiative C): shared-secret token, pinned to
		// the VoiceAgent* message surface only. Sits outside the
		// JWT-verified chain because the Python voice-agent is a
		// service-account caller without an identity JWT yet -- a
		// follow-up will swap to identity-issued tokens but the
		// surface-pin stays the same.
		voiceAgentChecked := memqlgrpc.NewVoiceAgentStreamInterceptor(operatorChecked, voiceAgentSharedToken(), a.Logger)
		a.grpcServer.SetStreamInterceptor(voiceAgentChecked)
		// Hand the verifier to the gRPC server so the in-stream
		// rotation handler (RotateAuthMsg) can re-verify a refreshed
		// bearer against the same JWKS + audience + issuer the
		// interceptor enforces at stream open. Without this the
		// handler responds with error="verifier_unconfigured" and
		// the cockpit falls back to reconnect-with-fresh-token.
		a.grpcServer.SetVerifier(a.identityVerifier)
	} else {
		// verifierRequired=false branch (identity binary). Operator
		// is the only admit path; everything else is rejected.
		a.grpcServer.SetStreamInterceptor(memqlgrpc.NewOperatorAwareStreamInterceptor(memqlgrpc.NewRejectAllStreamInterceptor(a.Logger), a.Logger))
	}

	// Install session-revocation HTTP middleware after the verifier
	// auth middleware (which ran earlier in configAndAuth). Engine is
	// required so this only runs when a backing store is available.
	if a.identityVerifier != nil && a.engine != nil {
		a.middlewares = append(a.middlewares, memqlgrpc.NewSessionRevocationHTTPMiddleware(a.engine, a.Logger))
	}

	// Email delivery is wired through the email integration plug-in
	// (see integrations/email/plugin.go). guest_handlers.go looks up
	// the integration on the engine's registry -- no direct wiring here.

	// === WebSocket Bridge ===
	wsEnvOpts, err := memqlws.LoadEnvOptions(memqlws.EnvLoader{})
	if err != nil {
		a.fatal("failed to load memql websocket env options", "error", err)
	}
	wsOptions := memqlws.Options{
		Logger:   a.Logger,
		Endpoint: a.grpcServer.Address(),
	}
	if err := wsEnvOpts.Apply(&wsOptions); err != nil {
		a.fatal("invalid memql websocket env options", "error", err)
	}
	memqlGateway, err := memqlgrpc.NewGateway(memqlgrpc.GatewayOptions{
		Logger:   a.Logger,
		Endpoint: a.grpcServer.Address(),
	})
	if err != nil {
		a.fatal("failed to initialize memql gateway", "error", err)
	}
	a.middlewares = append(a.middlewares, memqlGateway.Middleware())

	wsBridge, err := memqlws.New(wsOptions)
	if err != nil {
		a.fatal("failed to initialize memql websocket handler", "error", err)
	}
	for _, path := range server.MemqlWebsocketPaths() {
		a.mux.HandleFunc("GET "+path, wsBridge.ServeHTTP)
	}
}

// createHTTPServer finalizes middleware and creates the HTTP server.
// Called at the end of each tag-specific transport method after endpoints are wired.
func (a *App) createHTTPServer() {
	if len(a.middlewares) > 0 {
		a.httpArgs = append(a.httpArgs, server.WithStandardMiddlewares(a.middlewares...))
	}

	httpServer, err := a.overrides.NewHTTPServer(a.httpArgs...)
	if err != nil {
		a.fatal("failed to create http server", "error", err, "server", server.ComponentName)
	}

	if httpServer != nil {
		httpServer.SetAutomationScheduler(a.automationScheduler)
		httpServer.SetAutomationLoader(a.automationLoader)
		httpServer.SetMemoryEngine(a.engine)
		httpServer.SetWiring(a.wiring)
	}

	a.httpServer = httpServer
}
