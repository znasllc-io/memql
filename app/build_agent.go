//go:build agent

package app

import "log/slog"

// Build constructs service dependencies for an agent node.
// Agent nodes perform task execution and AI work. They need: config,
// database, engine, file/storage/STT integrations, AI transport, cluster.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsAgent()
	a.transportAgent()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
