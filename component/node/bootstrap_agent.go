package node

import "github.com/visionarys-io/memql/core/common"

// AgentBootstrap creates dependencies for an Agent node.
// Agent nodes perform task execution and AI work.
// They have: Engine + PeerManager + EventBridge + AI providers + tools + NodeServer.
// They do NOT have: HTTP server for external clients, voice pipeline.
type AgentBootstrap struct{}

func (*AgentBootstrap) NodeDependencies(ctx BootstrapContext) ([]common.Dependency, error) {
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

func (*AgentBootstrap) Description() string {
	return "agent (task execution, AI work, tool calling)"
}
