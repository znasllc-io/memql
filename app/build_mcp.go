//go:build mcp

package app

import "log/slog"

// Build constructs service dependencies for an MCP (Model Context Protocol)
// node -- the protocol head that exposes a memQL deployment's tool surface to
// external MCP hosts (Claude Desktop / Claude Code and others). It is a new
// node role selected by the `mcp` build tag (epic memql#1529), a protocol
// head over the engine's existing tool surface, mirroring app/build_bff.go.
//
// MCP nodes need: config/auth, database/concepts, engine/bus, core
// integrations, the MCP transport (stdio in Phase 0), and the cluster mesh
// wiring -- so health/lifecycle is consistent with the other roles. They do
// NOT serve the domain frontend, the voice pipeline, or file processing.
//
// Phase 0 (memql#1530) stands up the skeleton: the binary builds, boots,
// connects to the engine, and serves an empty MCP surface. The reflected tool
// surface, capability-tier / role gates, authoring, and resources/prompts
// land in later phases (#1531-#1536).
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsCore()
	a.transportMCP()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
