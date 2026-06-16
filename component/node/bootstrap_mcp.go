package node

import "github.com/znasllc-io/memql/core/common"

// MCPBootstrap creates dependencies for an MCP (Model Context Protocol) node
// -- the protocol head that exposes a deployment's tool surface to external
// MCP hosts (epic memql#1529). Like the workbench node it is a minimal mesh
// citizen: Engine + PeerManager + EventBridge + NodeServer (+ ParentConnector
// when an upstream is configured). It does NOT dial workers or run the
// SIForward outbound routing the BFF does; it speaks MCP over its own
// transport (stdio in Phase 0) and reads the engine in-process.
type MCPBootstrap struct{}

func (*MCPBootstrap) NodeDependencies(ctx BootstrapContext) ([]common.Dependency, error) {
	peerMgr := NewPeerManager(ctx.Identity, ctx.Logger)
	installStatusWriter(peerMgr, ctx)
	eventBridge := NewEventBridge(ctx.Identity, ctx.EventBus, peerMgr, ctx.Logger)
	if ctx.Wiring != nil {
		eventBridge.SetWiring(ctx.Wiring)
	}
	nodeServer := NewNodeServer(ctx.Identity, peerMgr, ctx.Logger)
	nodeServer.SetEventInbound(eventBridge)

	deps := []common.Dependency{peerMgr, eventBridge, nodeServer}
	if pconn := NewParentConnector(ctx.Identity, peerMgr, ctx.Logger); pconn != nil {
		pconn.SetEventInbound(eventBridge)
		deps = append(deps, pconn)
	}
	return deps, nil
}

func (*MCPBootstrap) Description() string {
	return "mcp (Model Context Protocol server -- engine tool surface to external MCP hosts)"
}
