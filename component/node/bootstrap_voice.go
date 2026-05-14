package node

import "github.com/visionarys-io/memql/core/common"

// VoiceBootstrap creates dependencies for a Voice node.
// Voice nodes handle the audio I/O pipeline: ASR, TTS, LiveKit.
// They do NOT run the scoring engine or cognition integration.
// They have: Engine + PeerManager + EventBridge + NodeServer.
type VoiceBootstrap struct{}

func (*VoiceBootstrap) NodeDependencies(ctx BootstrapContext) ([]common.Dependency, error) {
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

func (*VoiceBootstrap) Description() string {
	return "voice (audio I/O: ASR, TTS, LiveKit)"
}
