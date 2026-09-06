//go:build !agent && !planner && !bff && !identity && !workbench && !mcp && !edge

package app

import "log/slog"

// Build constructs service dependencies for the default build (BFF).
// When no build tag is specified, the binary runs as a BFF node.
// BFF nodes serve domain-specific frontends.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsCore()
	a.transportBase()
	a.createHTTPServer()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
