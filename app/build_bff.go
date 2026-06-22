//go:build bff

package app

import "log/slog"

// Build constructs service dependencies for a BFF (Backend For Frontend) node.
// BFF nodes serve domain-specific frontends.
// They need: config, database, engine, core integrations, base transport, cluster.
// They do NOT need: AI providers, voice pipeline, file processing.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsBFF()
	a.transportBFF()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
