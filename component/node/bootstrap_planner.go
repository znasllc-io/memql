package node

import "github.com/visionarys-io/memql/core/common"

// PlannerBootstrap creates dependencies for a Planner node.
// Planner nodes handle task planning and orchestration.
// They have: Engine + PeerManager + EventBridge + NodeServer.
// They do NOT have: HTTP server, voice pipeline, external client access.
type PlannerBootstrap struct{}

func (*PlannerBootstrap) NodeDependencies(ctx BootstrapContext) ([]common.Dependency, error) {
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

func (*PlannerBootstrap) Description() string {
	return "planner (task planning, orchestration)"
}
