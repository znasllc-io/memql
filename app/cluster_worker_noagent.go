//go:build !agent && !planner && (identity || workbench || mcp || edge)

package app

import "github.com/znasllc-io/memql/component/node"

// Only agents, BFFs and planners originate worker-model forwards. Other builds
// retain the shared catalog without claiming a callable fleet transport.
func (a *App) wireWorkerForwarding(
	_ *node.Identity,
	_ *node.PeerManager,
	_ *node.NodeServer,
	_ *node.ParentConnector,
) {
}
