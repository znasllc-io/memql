package node

import "github.com/visionarys-io/memql/core/common"

// CognitionBootstrap creates dependencies for a Cognition node.
// Cognition nodes handle voice turn-taking and conversation management.
// They have: Engine + PeerManager + EventBridge + Polyphon + voice integrations + NodeServer.
// They do NOT have: HTTP server for external clients, auth middleware, file uploads.
type CognitionBootstrap struct{}

func (*CognitionBootstrap) NodeDependencies(ctx BootstrapContext) ([]common.Dependency, error) {
	peerMgr := NewPeerManager(ctx.Identity, ctx.Logger)
	installStatusWriter(peerMgr, ctx)
	eventBridge := NewEventBridge(ctx.Identity, ctx.EventBus, peerMgr, ctx.Logger)
	if ctx.Wiring != nil {
		eventBridge.SetWiring(ctx.Wiring)
	}
	nodeServer := NewNodeServer(ctx.Identity, peerMgr, ctx.Logger)
	nodeServer.SetEventInbound(eventBridge)

	// Polyphon cognition (scoring engine) and voice integrations are wired by main.go
	// when it detects cognition node type. This bootstrap provides
	// the node-specific dependencies only.

	deps := []common.Dependency{peerMgr, eventBridge, nodeServer}
	if pconn := NewParentConnector(ctx.Identity, peerMgr, ctx.Logger); pconn != nil {
		pconn.SetEventInbound(eventBridge)
		deps = append(deps, pconn)
	}
	return deps, nil
}

func (*CognitionBootstrap) Description() string {
	return "cognition (voice turn-taking, conversation management, Polyphon pipeline)"
}
