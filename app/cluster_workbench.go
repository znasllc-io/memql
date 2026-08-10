package app

import (
	"os"

	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// wireWorkbenchForwarding installs the cross-node workbench dispatch
// path on agent and workbench binaries. Called from cluster.go after
// the NodeServer + PeerManager are constructed.
//
// Agent node (originator):
//   - When MEMQL_WORKBENCH_REMOTE is truthy, build a ForwardRouter
//     bound to the PeerManager, register it as the inbound response
//     sink on NodeServer + ParentConnector + a workbench-narrowed
//     WorkerDialer, and wire it into the local workbench Integration
//     so handleDispatchHost delegates instead of running locally.
//   - When MEMQL_WORKBENCH_REMOTE is not set, this is a no-op and
//     the integration keeps its single-node MVP behavior (local
//     dispatch against the agent node's own disk).
//
// Workbench node (receiver):
//   - Build a ForwardHandler wrapping the local workbench Integration
//     and install it on NodeServer so inbound
//     WorkbenchForwardRequest envelopes dispatch into the same six
//     action handlers (exec / fs_read / fs_write / fs_list /
//     fs_stat / http_fetch) used in single-node mode.
//
// All other node types: no-op.
func (a *App) wireWorkbenchForwarding(
	nodeIdentity *node.Identity,
	peerMgr *node.PeerManager,
	nodeServer *node.NodeServer,
	parentConnector *node.ParentConnector,
) {
	if nodeIdentity == nil {
		return
	}

	integ := a.lookupWorkbenchIntegration()

	switch nodeIdentity.Type {
	case node.NodeTypeAgent:
		// Only wire the remote path when explicitly enabled. Without
		// the env var the agent keeps the MVP behavior: local dispatch
		// against /var/lib/memql/workbenches on its own disk.
		if !workbenchRemoteEnabled() {
			a.Logger.Info("workbench forwarding: MEMQL_WORKBENCH_REMOTE not set; agent will dispatch locally")
			return
		}
		if peerMgr == nil {
			a.Logger.Warn("workbench forwarding: peerMgr nil on agent; skipping remote wiring")
			return
		}
		forwarder := workbench.NewForwardRouter(peerMgr, a.Logger)
		if nodeServer != nil {
			nodeServer.SetWorkbenchForwardResponseSink(forwarder)
		}
		if parentConnector != nil {
			parentConnector.SetWorkbenchForwardResponseSink(forwarder)
		}
		// Outbound dialer narrowed to workbench peers so the router
		// has a *peerConnection to Send on. Mirrors the cognition /
		// planner -> agent dialer pattern in cluster.go.
		seeds := a.workerPeerSeedsFromEnv()
		if dialer := node.NewWorkerDialer(
			nodeIdentity,
			peerMgr,
			a.engine,
			a.eventBus,
			seeds,
			a.Logger,
		); dialer != nil {
			dialer.SetDialTypes(node.NodeTypeWorkbench)
			dialer.SetWorkbenchForwardResponseSink(forwarder)
			a.Dependencies = append(a.Dependencies, dialer)
		}
		if integ != nil {
			integ.SetForwardRouter(forwarder)
			a.Logger.Info("workbench forwarding: agent will dispatch to remote workbench peers")
		} else {
			a.Logger.Warn("workbench forwarding: integration not found; remote wiring incomplete")
		}

	case node.NodeTypeWorkbench:
		if integ == nil {
			a.Logger.Warn("workbench node: integration plug-in not materialized; cannot wire forward handler")
			return
		}
		if nodeServer == nil {
			a.Logger.Warn("workbench node: NodeServer nil; inbound dispatches will be unhandled")
			return
		}
		handler := workbench.NewForwardHandler(integ, a.Logger)
		nodeServer.SetWorkbenchForwardHandler(handler)
		a.Logger.Info("workbench node: forward handler installed on NodeService.Stream")
	}
}

// lookupWorkbenchIntegration walks the engine's IntegrationProvider
// registry to find the workbench plug-in instance. Returns nil if
// the plug-in didn't register (e.g. anchor missing) or if the
// engine isn't available.
func (a *App) lookupWorkbenchIntegration() *workbench.Integration {
	if a.engine == nil {
		return nil
	}
	provider := a.engine.IntegrationByName("workbench")
	if provider == nil {
		return nil
	}
	if integ, ok := provider.(*workbench.Integration); ok {
		return integ
	}
	return nil
}

// workbenchRemoteEnabled mirrors the integration's env-var parsing
// so the cluster wiring and the dispatch path agree on the toggle.
func workbenchRemoteEnabled() bool {
	v := os.Getenv("MEMQL_WORKBENCH_REMOTE")
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes", "on", "ON", "On":
		return true
	default:
		return false
	}
}
