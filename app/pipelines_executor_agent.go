//go:build agent

package app

import (
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// pipelines_executor_agent.go -- the agent node registers the substrate's
// executor (epic memql#5478, #5493/#5494): the one pl.Executor the pipelines
// driver (integrations_pipelines_agent.go, also agent-only) calls for every
// step it drives.
//
// Wired in the CLUSTER phase, after wireWorkbenchForwarding, because the
// executor forwards over the workbench ForwardRouter that phase builds -- the
// one the node stream delivers WorkbenchForwardResponses to; a second router
// would never hear its replies -- and after the transport phase built the
// worker dispatcher the fleet path dispatches through.

// wirePipelinesExecutor builds the executor and registers it. It always
// registers one on an agent node: a node missing a half answers the steps
// that need it with pipeline_runner_unavailable, saying which half, rather
// than leaving every step to the driver's "no runner registered".
func (a *App) wirePipelinesExecutor(nodeIdentity *node.Identity) {
	if nodeIdentity == nil || nodeIdentity.Type != node.NodeTypeAgent {
		return
	}
	cfg := pipelinesteps.ConfigFromEnv(nil)

	// A nil INTERFACE when there is no router -- never a typed nil, which the
	// executor would read as a route.
	var forwarder pipelinesteps.Forwarder
	if wb := a.lookupWorkbenchIntegration(); wb != nil {
		if router := wb.ForwardRouter(); router != nil {
			forwarder = router
		}
	}
	if forwarder == nil {
		a.Logger.Warn("pipelines executor: no route to a workbench replica (MEMQL_WORKBENCH_REMOTE unset, or no workbench integration); "+
			"cluster steps on this node fail pipeline_runner_unavailable",
			"component", "pipelinesteps")
	}

	var fleet pipelinesteps.FleetRouter
	if integ := a.lookupWorkerIntegration(); integ != nil && integ.Dispatcher() != nil {
		// The Library store is nil until the app's store lands with the
		// workbench wiring (#5495); until then a fleet step's log and
		// artifacts are notes beside it, and its output still reaches the log
		// store.
		fleet = pipelinesteps.NewFleet(cfg, integ.Dispatcher(), nil, a.pipelinesTokenMinterFor(), currentPipelinesLineSink, a.Logger)
	} else {
		a.Logger.Warn("pipelines executor: no worker dispatcher on this node; steps that name a need fail pipeline_runner_unavailable",
			"component", "pipelinesteps")
	}

	pipelines.RegisterExecutor(pipelinesteps.NewExecutor(cfg, forwarder, fleet, a.Logger))
	a.Logger.Info("pipelines executor registered: cluster steps forward to a workbench replica, steps naming a need go to the owner's machines",
		"component", "pipelinesteps",
		"runCeiling", cfg.RunCeiling.String(),
		"workbenchRoute", forwarder != nil,
		"fleet", fleet != nil)
}

// currentPipelinesLineSink is the log store, as the node installed it when a
// step's capture opens; nil before boot installs one.
func currentPipelinesLineSink() pipelinesteps.LineSink {
	if s := logger.CurrentSink(); s != nil {
		return s
	}
	return nil
}
