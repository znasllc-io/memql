//go:build agent

package app

import (
	"github.com/znasllc-io/memql/component/node"
	workerservice "github.com/znasllc-io/memql/component/worker"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// wireWorkerForwarding installs the cross-node machine-dispatch path on agent
// binaries (memql#4352). Called from cluster.go after the NodeServer and
// PeerManager exist.
//
// WHY IT IS ONE NODE TYPE ON BOTH SIDES, unlike every other forward in the
// mesh. AI forwarding goes bff -> agent; workbench forwarding goes agent ->
// workbench; deploy control goes bff -> identity. This one goes agent ->
// AGENT. A cockpit machine's WorkerService stream terminates on whichever
// replica it connected to, and the turn wanting to use it is served on
// whichever replica the mesh routed the request to. Both are agents, so every
// agent replica is simultaneously an originator and a receiver, and the wiring
// installs both halves unconditionally.
//
// THERE IS NO ENABLE FLAG, deliberately -- and this is the one place it
// differs from wireWorkbenchForwarding, which gates on MEMQL_WORKBENCH_REMOTE.
// The workbench flag exists because running a workbench call locally is a
// LEGITIMATE alternative (the agent has a disk). Here there is no alternative:
// this replica does not hold the machine's stream, so a local dispatch is not
// a degraded path, it is a call that cannot work. A flag would only let an
// operator turn a working fleet into a broken one.
//
// With one replica the wiring is inert: the router's candidates all carry this
// node's own id, so nothing is ever forwarded.
func (a *App) wireWorkerForwarding(
	nodeIdentity *node.Identity,
	peerMgr *node.PeerManager,
	nodeServer *node.NodeServer,
	parentConnector *node.ParentConnector,
) {
	if nodeIdentity == nil || nodeIdentity.Type != node.NodeTypeAgent {
		return
	}
	integ := a.lookupWorkerIntegration()
	if integ == nil || integ.Dispatcher() == nil {
		a.Logger.Warn("worker forwarding: the agentworker integration is not registered; " +
			"machines held by another replica will be skipped rather than reached")
		return
	}
	if peerMgr == nil {
		a.Logger.Warn("worker forwarding: no PeerManager on this node; " +
			"machines held by another replica will be skipped rather than reached")
		return
	}

	// The RECEIVING half. Installed first: a replica that can send but not
	// receive is a mesh where forwards flow one way, which is harder to
	// diagnose than one where they do not flow at all.
	if nodeServer != nil {
		handler := agentworker.NewForwardHandler(a.workerRegistry(), integ.Dispatcher().FleetStore(), a.Logger)
		nodeServer.SetWorkerForwardHandler(handler)
	} else {
		a.Logger.Warn("worker forwarding: NodeServer is nil; inbound machine dispatches will be unhandled")
	}

	// The SENDING half.
	forwarder := agentworker.NewForwardRouter(peerMgr, a.Logger)
	if nodeServer != nil {
		nodeServer.SetWorkerForwardResponseSink(forwarder)
	}
	if parentConnector != nil {
		parentConnector.SetWorkerForwardResponseSink(forwarder)
	}
	// An outbound dialer narrowed to agent peers, so the router has a live
	// connection to Send on. isSelf() keeps this replica out of its own
	// target set.
	seeds := a.workerPeerSeedsFromEnv()
	if dialer := node.NewWorkerDialer(
		nodeIdentity,
		peerMgr,
		a.engine,
		a.eventBus,
		seeds,
		a.Logger,
	); dialer != nil {
		dialer.SetDialTypes(node.NodeTypeAgent)
		dialer.SetWorkerForwardResponseSink(forwarder)
		a.Dependencies = append(a.Dependencies, dialer)
	}
	integ.Dispatcher().SetRemoteDispatcher(forwarder)

	// LOCAL INFERENCE (epic memql#4676). Installed here rather than in
	// integrations_worker_agent.go because it needs the FORWARD: a model call
	// must reach a machine held by a sibling replica, and a fleet that could
	// only see this node's own streams would report half the user's machines
	// as having no models -- which is indistinguishable at the surface from
	// `no_local_model_available`, the refusal that means "your fleet is
	// asleep". A user looking at a laptop they can see is on would be told to
	// wake it up.
	if providers := a.engine.Providers(); providers != nil {
		providers.SetFleetInference(agentworker.NewFleetInference(integ.Dispatcher(), forwarder, a.Logger))
		a.Logger.Info("fleet inference: this replica can run models on the user's machines",
			"node_id", nodeIdentity.ID)
	}

	a.Logger.Info("worker forwarding: this replica can reach machines held by its peers",
		"node_id", nodeIdentity.ID)
}

// workerRegistry returns the agent node's in-memory worker registry, or nil
// when the worker service was not wired.
func (a *App) workerRegistry() *workerservice.Registry {
	if a == nil || a.workerService == nil {
		return nil
	}
	// a.workerService is typed `any` on App so the non-agent builds compile
	// without the worker package; the assertion is where that erasure is paid
	// back.
	svc, ok := a.workerService.(*workerservice.Service)
	if !ok {
		return nil
	}
	return svc.Registry()
}
