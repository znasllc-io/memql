//go:build !agent

package app

import (
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// wireProcedureAgentSeams installs nothing off the agent node, and says so.
//
// The machine dispatcher needs the worker's dispatchHost, whose streams
// terminate on the agent node, and the app fallback needs the app-session
// delegate the agent node installs; neither exists in this binary. A replay
// that lands here with a machine-local procedure refuses the machine target
// by name (no_machine_dispatcher), and one that diverges fails its run with
// procedure_fallback_unavailable, rather than reaching for a surface this node
// does not have.
func (a *App) wireProcedureAgentSeams(_ *procedure.Integration, _ *procedureProber) {
	a.procedureLog().Info("procedure replay: no machine dispatcher and no app fallback on this node; both are agent-only",
		"component", "procedure")
}
