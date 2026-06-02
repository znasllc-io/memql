//go:build identity

package app

import "log/slog"

// Build constructs service dependencies for an identity node — the
// in-house authentication provider. Identity nodes host the public
// auth web pages, the admin web app, the OAuth-style token
// endpoints, and the JWKS feed every other node uses to verify
// access tokens.
//
// Identity nodes need: config, database (for v1:identity:auditEvent
// rows + magic-link / access-request rows), engine, core
// integrations, base transport, plus the identity-specific routes.
// They don't need: SI providers, voice pipeline, agent reply
// pipeline. Smaller binary, narrower attack surface.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsIdentity()
	a.transportBase()
	// Deploy-control gRPC service: depends on the gRPC server existing
	// (transportBase) + the engine being ready. Lands before
	// transportIdentity so the service is registered before serving.
	a.setupDeployControlService()
	a.transportIdentity()
	a.createHTTPServer()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
