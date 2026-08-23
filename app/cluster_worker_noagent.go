//go:build !agent

package app

import "github.com/znasllc-io/memql/component/node"

// wireWorkerForwarding is a no-op off the agent build. The cross-node machine
// dispatch it installs is agent-to-agent by construction (memql#4352), and
// integrations/agent/worker is `//go:build agent`, so the real implementation
// cannot be linked here. cluster.go calls this unconditionally rather than
// guarding the call site, which keeps the topology wiring readable as one
// sequence and puts the node-type decision in one place -- the real function's
// own early return.
func (a *App) wireWorkerForwarding(
	_ *node.Identity,
	_ *node.PeerManager,
	_ *node.NodeServer,
	_ *node.ParentConnector,
) {
}
