//go:build planner

package app

import (
	"github.com/znasllc-io/memql/component/node"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// Planner compilation makes model calls but owns no WorkerService streams.
// Its existing agent dialer carries these model requests and their responses,
// so catalog visibility and callable local inference describe the same fleet.
//
// THE APP DOOR RIDES THE SAME FORWARD (the planner/app-source design, section
// 3a). A route whose triage, compile or compose step prefers `app:claude-code`
// used to pass over it on every planner call, because an app session travels
// over a stream only an agent holds. Now the call is forwarded, resolved, to
// the agent holding the machine; that agent runs the app gate and the session.
// Only the chat door crosses -- a tool-needing STEP is never handed over from
// here (memql.AppDoor.Runnable is local-only, and the planner holds nothing).
func (a *App) wireWorkerForwarding(identity *node.Identity, peers *node.PeerManager, server *node.NodeServer, parent *node.ParentConnector) {
	if identity == nil || identity.Type != node.NodeTypePlanner || peers == nil || a.engine == nil {
		return
	}
	dialer := a.existingWorkerDialer()
	if dialer == nil {
		a.Logger.Warn("planner fleet inference is not wired: its agent worker dialer is missing")
		return
	}
	providers := a.engine.Providers()
	if providers == nil {
		return
	}
	forward := agentworker.NewForwardRouter(peers, a.Logger)
	if server != nil {
		server.SetWorkerForwardResponseSink(forward)
	}
	if parent != nil {
		parent.SetWorkerForwardResponseSink(forward)
	}
	dialer.SetWorkerForwardResponseSink(forward)
	providers.SetFleetInference(agentworker.NewRemoteFleetInference(&agentworker.EngineStore{Engine: a.engine}, forward, identity.ID, a.Logger))
	providers.SetAppInference(agentworker.NewRemoteAppInference(&agentworker.EngineStore{Engine: a.engine}, forward, identity.ID, a.Logger))
	a.Logger.Info("planner fleet and app inference wired through the agent holding each machine", "node_id", identity.ID)
}
