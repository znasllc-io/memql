//go:build workbench

package app

import "log/slog"

// Build constructs service dependencies for a workbench node.
//
// The workbench node hosts the sandboxed per-Plan Linux working
// environment that agents call into for headless work (shell exec,
// filesystem, http_fetch). It is the cluster-mode counterpart to
// the agent-node-integrated MVP: the same integration code, lifted
// into a dedicated binary so workbench compute can scale + be
// isolated independently.
//
// The workbench node:
//   - Loads core integrations (database, knowledge seed, identity
//     verifier, etc.) via the plug-in path. The workbench integration
//     anchors in app/plugins_core.go too, so it's available here.
//   - Stands up a minimal gRPC transport (NodeService for cross-node
//     dispatch; the standard MemqlService is included via
//     transportMinimal for health checks + admin queries).
//   - Joins the peer mesh as node type "workbench" so the agent
//     node's WorkbenchForwardRouter can discover it via PeerManager.
func Build(serviceLogger *slog.Logger, version string, overrides Overrides) *App {
	a := newApp(serviceLogger, version, overrides)

	a.configAndAuth()
	a.databaseAndConcepts()
	a.engineAndBus()
	a.integrationsWorkbench()
	a.transportMinimal()
	a.cluster()

	a.Dependencies = append(a.Dependencies, a.grpcServer)
	a.Dependencies = append(a.Dependencies, a.httpServer)

	return a
}
