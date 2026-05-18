package node

import "github.com/znasllc-io/memql/core/common"

// WorkbenchBootstrap creates dependencies for a Workbench node.
// Workbench nodes host the sandboxed per-Plan Linux working
// environment that agent nodes call into for headless work.
// They have: Engine + PeerManager + EventBridge + NodeServer.
// They do NOT have: HTTP server beyond healthchecks, voice pipeline,
// external client access, AiForward routing.
type WorkbenchBootstrap struct{}

func (*WorkbenchBootstrap) NodeDependencies(ctx BootstrapContext) ([]common.Dependency, error) {
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

func (*WorkbenchBootstrap) Description() string {
	return "workbench (per-Plan sandboxed Linux execution)"
}
