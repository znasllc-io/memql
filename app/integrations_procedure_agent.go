//go:build agent

package app

import (
	"github.com/znasllc-io/memql/component/work"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// wireProcedureAgentSeams installs the replay's agent-only seams (epic
// memql#5408, D4, D16): the MACHINE dispatcher and its half of the prober,
// and the APP FALLBACK.
//
// AGENT-ONLY, AND FOR TWO REASONS THAT ARE THE SAME REASON. The machine is
// reached through the worker's dispatchHost, and the worker streams terminate
// on the agent node; the app takes a goal back as a session, and the
// app-session delegate that opens one is installed on the agent node
// (integrations_worker_agent.go). A replay that serves a goal runs here too --
// the agent node executes compiled work runs (integrations_work_dispatch.go).
//
// Called from wireProcedureIntegration, in integrationsCore -- BEFORE the
// transport phase registers the worker integration and the session delegate.
// That is safe because nothing is resolved now: the worker's handler is found
// by name when a step runs, and the fallback reaches the delegate through the
// router when a goal is handed back.
func (a *App) wireProcedureAgentSeams(integ *procedure.Integration, prober *procedureProber) {
	machine := newProcedureMachineDispatcher(a.engine)
	integ.SetDispatcher(work.TargetMachine, machine)
	prober.setHost(work.TargetMachine, machine.probeHost)
	integ.SetAppFallback(newProcedureAppFallback(a.engine))
	a.procedureLog().Info("procedure replay: the machine dispatcher, its prober and the app fallback are installed",
		"component", "procedure")
}
