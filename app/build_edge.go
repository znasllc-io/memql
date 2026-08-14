//go:build edge

package app

import "log/slog"

// Build constructs service dependencies for an EDGE node -- the surface that
// serves this cluster's web surfaces: every hosted SPA and website, and the
// memQL Portal itself, which is site #1 and takes no special path.
//
// It is a node type rather than a handler bolted onto the bff for three
// reasons. A website-hosting cluster should be able to drop to four nodes
// (identity + bff + edge + Postgres), which it cannot if the edge rides the
// API node. A site deploy must never share fate with the API. And per-node-type
// binaries selected by build tag is the pattern this repository already uses
// for exactly this kind of separation.
//
// Edge nodes need config/auth, database/concepts (they read v1:platform:site),
// engine/bus, core integrations (storage, for bundles) and the cluster mesh.
// They do NOT need the voice pipeline, the cognition pipeline, file processing
// or the agent tool surface.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsCore()
	a.transportEdge()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
