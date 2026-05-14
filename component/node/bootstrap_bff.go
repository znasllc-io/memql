package node

import "github.com/visionarys-io/memql/core/common"

// BFFBootstrap creates dependencies for a BFF (Backend For Frontend) node.
// BFF nodes serve domain-specific frontends.
// They have: Engine + PeerManager + EventBridge + NodeServer.
// They do NOT have: SI providers, voice pipeline.
type BFFBootstrap struct{}

func (*BFFBootstrap) NodeDependencies(ctx BootstrapContext) ([]common.Dependency, error) {
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

func (*BFFBootstrap) Description() string {
	return "bff (backend for frontend, domain-specific)"
}
