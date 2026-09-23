package app

import (
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// integrations_procedure.go joins procedure learning to its runtime
// collaborators (epic memql#5408).
//
// ===========================================================================
// GATE 1 IS INSTALLED HERE OR IT NEVER RUNS (gap G7)
// ===========================================================================
// A learned procedure is recorded as re-runnable -- given the goal signature
// that makes it findable, and let onto the certification ladder above
// candidate -- only when its source passes Gate 1, the isolated compile +
// bind. The plug-in used to look for that compile by type-asserting the
// engine its PluginContext hands it, and *memql.MemQLEngine has no such
// method: the compile is a function of the engine, which CognitionEngineAdapter
// wraps. So Gate 1 never ran on any node, every procedure was recorded as not
// re-runnable, and none was ever findable -- with every test green, because a
// procedure that is merely "not re-runnable" is a well-formed answer.
//
// UNTAGGED, and called from integrationsCore, because the plug-in registers on
// every node type and a lift runs wherever a run succeeds.
//
// The REGISTERED instance, never a second one: capability dispatch goes
// through the instance the registry built, so a gate installed on another
// would be configured and never consulted.
func (a *App) wireProcedureIntegration() {
	integ := a.lookupProcedureIntegration()
	if integ == nil {
		// A node whose engine did not materialize the plug-in has no lift to
		// gate. Debug, not a warning: a test engine or a stripped build.
		if a.Logger != nil {
			a.Logger.Debug("procedure integration not wired: the plug-in did not materialize", "component", "procedure")
		}
		return
	}
	integ.SetCompiler(&CognitionEngineAdapter{Engine: a.engine})
	if a.Logger != nil {
		a.Logger.Info("procedure integration wired: Gate 1 compiles every learned procedure", "component", "procedure")
	}
}

// lookupProcedureIntegration returns the registered plug-in instance, or nil.
func (a *App) lookupProcedureIntegration() *procedure.Integration {
	if a == nil || a.engine == nil {
		return nil
	}
	provider := a.engine.IntegrationByName("procedure")
	if provider == nil {
		return nil
	}
	integ, _ := provider.(*procedure.Integration)
	return integ
}
