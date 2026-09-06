//go:build planner

package app

import "github.com/znasllc-io/memql/integrations/planner"

// integrations_work_compile.go -- joins the two halves of epic A2's compile
// (memql#4966).
//
// integrations/work declares `Compiler` as a seam and integrations/planner
// implements it, and until this call is made NOTHING JOINS THEM: createGoal
// opens the goal and its first run, finds no compiler, and returns
// compileDispatched:false, leaving the run in `compiling` forever. From
// outside that is indistinguishable from a goal that was accepted and then
// ignored -- and the wait-and-abandon sweep deliberately does not touch a run
// in `compiling`, so nothing else would move it either.
//
// It runs on the PLANNER node only, which is what section H of the design
// record says: "The planner node keeps compile, the reactive loop and the
// sweeps; the agent node runs steps."

func (a *App) wireWorkCompiler() {
	if a.plannerIntegration == nil {
		return
	}
	work := a.lookupWorkIntegration()
	if work == nil {
		// A planner node whose work plug-in did not materialize. Loud,
		// because every goal created on this node will sit in `compiling`
		// and the symptom -- "my goal never started" -- names nothing.
		a.Logger.Warn("work compile not wired: the work integration did not materialize on this planner node; goals will open a run and never compile",
			"component", "work")
		return
	}
	// a.plannerIntegration is `any` on the App (the field is shared with
	// builds that do not link this package), so the assertion is where the
	// type comes back.
	pi, ok := a.plannerIntegration.(*planner.PlannerIntegration)
	if !ok || pi == nil {
		a.Logger.Warn("work compile not wired: the stashed planner integration is not the expected type", "component", "work")
		return
	}
	// The work integration is BOTH the seam compile is installed on and the
	// writer it records through -- the planner is not in the call-origin
	// allowlist and must not write @serverOnly constructs itself.
	compiler := pi.WorkCompiler(work)
	if compiler == nil {
		a.Logger.Warn("work compile not wired: the planner integration has no agent loop", "component", "work")
		return
	}
	work.SetCompiler(compiler)
	a.Logger.Info("work compile wired to the planner's authoring pipeline", "component", "work")
}
