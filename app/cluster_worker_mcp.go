//go:build mcp

package app

import (
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/worker"
	agentworker "github.com/znasllc-io/memql/integrations/agent/worker"
)

// MCP holds no machine streams. Reuse the normal owner-scoped dispatcher and
// its authenticated agent hop; execution and consent still belong to Cockpit.
func (a *App) wireWorkerForwarding(identity *node.Identity, peers *node.PeerManager, server *node.NodeServer, parent *node.ParentConnector) {
	if identity == nil || peers == nil || a.engine == nil {
		return
	}
	forward := agentworker.NewForwardRouter(peers, a.Logger)
	if server != nil {
		server.SetWorkerForwardResponseSink(forward)
	}
	if parent != nil {
		parent.SetWorkerForwardResponseSink(forward)
	}
	dialer := a.existingWorkerDialer()
	if dialer == nil {
		dialer = node.NewWorkerDialer(identity, peers, a.engine, a.eventBus, a.workerPeerSeedsFromEnv(), a.Logger)
		if dialer != nil {
			dialer.SetDialTypes(node.NodeTypeAgent)
			a.Dependencies = append(a.Dependencies, dialer)
		}
	} else {
		dialer.IncludeDialTypes(node.NodeTypeAgent)
	}
	if dialer != nil {
		dialer.SetWorkerForwardResponseSink(forward)
	}
	registry := worker.NewRegistry(a.Logger, nil)
	dispatcher, err := agentworker.NewDispatcher(agentworker.Options{Logger: a.Logger, Registry: registry, Engine: a.engine, Store: &agentworker.EngineStore{Engine: a.engine}, Auditor: workerAuditorForApp(a), SelfNodeId: identity.ID, Remote: forward})
	if err != nil {
		a.fatal("MCP worker dispatcher: build failed", "error", err)
		return
	}
	integration := agentworker.NewIntegration(dispatcher, registry, a.engine, a.Logger)
	if work := a.lookupWorkIntegration(); work != nil {
		integration.ScopeRequester = work.AskComputerScope
		dispatcher.SetComputerPermissions(work)
	}
	if err := a.engine.RegisterIntegration(integration); err != nil {
		a.fatal("MCP worker integration: register failed", "error", err)
		return
	}
	if wb := a.lookupWorkbenchIntegration(); wb != nil {
		wb.SetFleetDispatcher(dispatcher)
	}
	if providers := a.engine.Providers(); providers != nil {
		store := &agentworker.EngineStore{Engine: a.engine}
		providers.SetFleetInference(agentworker.NewRemoteFleetInference(store, forward, identity.ID, a.Logger))
		providers.SetAppInference(agentworker.NewRemoteAppInference(store, store, forward, identity.ID, a.Logger))
	}
	a.Logger.Info("MCP host tools wired through the agent holding each machine", "node_id", identity.ID)
}
