package app

import (
	"github.com/znasllc-io/memql/component/automations"
)

// automation_run.go wires the automation invoke path (memql#3310) -- the
// RunAutomation surface on MemqlService.Stream.
//
// No build tag: every binary that serves the stream can receive a run request,
// and every binary that carries an automation scheduler can be the target of
// one. Restricting either end by build tag would mean some node pair in the
// mesh silently cannot relay a run, which is the same class of bug as a
// missing routing rule, just in a smaller box.
//
// Called from cluster() rather than from the transport phase because that is
// where the node's own identity (type + id) is resolved, and the relay reports
// both on every frame: the UI needs to say which cluster and node a run
// executed against, and the cluster-e2e assertion for the cross-node hop is
// literally "executed_on_node_id differs from requested_on_node_id".
func (a *App) wireAutomationRunner(nodeType, nodeId string) {
	if a == nil || a.grpcServer == nil {
		return
	}
	if a.automationScheduler == nil {
		// A binary with no automation scheduler has nothing to run. The
		// handler answers with a refusal naming that fact rather than
		// pretending the surface exists.
		return
	}

	relay, err := automations.NewRunRelay(automations.RunRelayOptions{
		Logger:   a.Logger,
		Runner:   a.automationScheduler,
		EventBus: a.eventBus,
		NodeId:   nodeId,
		NodeType: nodeType,
		// The same cross-replica claim the event executor uses. A relayed run
		// broadcasts to every peer, so without it BOTH replicas of a scaled
		// target Deployment would execute the automation -- the same
		// double-fire the guard exists to prevent on the real trigger path.
		Claimer: a.clusterGuard,
	})
	if err != nil {
		// Non-fatal: a node that cannot build a relay still serves every other
		// stream surface. The run handler refuses with "no automation runner".
		a.Logger.Warn("automation run relay not wired", "error", err, "node_type", nodeType)
		return
	}

	// Subscribes the two relay topics. Harmless (and a no-op) when this binary
	// has no event bus: the local-target path never touches the bus, so the
	// relay stays fully functional for same-node runs.
	relay.Start()
	a.grpcServer.SetAutomationRunner(relay)

	a.Logger.Info("automation run relay wired",
		"node_type", nodeType, "node_id", nodeId, "mesh_relay", a.eventBus != nil)
}
