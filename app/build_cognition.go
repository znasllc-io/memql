//go:build cognition

package app

import "log/slog"

// Build constructs service dependencies for a cognition node.
// Cognition nodes handle conversation intelligence: turn-taking
// scoring, response generation, presence tracking. They run the
// Polyphon ScoreEngine but NOT the audio pipeline (that's on voice
// nodes). They need: config, database, engine, score engine,
// cognition integration, base transport, cluster.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.initPolyphonScoreEngine()
	a.integrationsCognition()
	a.transportBase()
	a.createHTTPServer()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
