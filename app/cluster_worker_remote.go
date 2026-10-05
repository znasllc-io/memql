//go:build !agent && !identity && !workbench && !mcp && !edge

package app

import (
	"github.com/znasllc-io/memql/component/node"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// The BFF embeds memory and the planner compiles work; neither owns WorkerService streams.
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
	if identity == nil || (identity.Type != node.NodeTypePlanner && identity.Type != node.NodeTypeBFF) || peers == nil || a.engine == nil {
		return
	}
	dialer := a.existingWorkerDialer()
	if dialer == nil {
		a.Logger.Warn("remote fleet inference is not wired: its agent worker dialer is missing")
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
	store := &agentworker.EngineStore{Engine: a.engine}
	providers.SetFleetInference(agentworker.NewRemoteFleetInference(store, forward, identity.ID, a.Logger))
	// The store is the delegation-policy reader too: `app:*` on the planner
	// follows the owner's appOrder, exactly as it does on the agent.
	providers.SetAppInference(agentworker.NewRemoteAppInference(store, store, forward, identity.ID, a.Logger))
	a.Logger.Info("remote fleet and app inference wired through the agent holding each machine", "node_id", identity.ID)
}
