package app

// integrations_work_validator.go -- which nodes serve the answer validator's
// check (integrations/work validator.go), and the claim among them.
//
// AGENT NODES SERVE IT, AND ONLY THEY. The validateGoalAnswer automation runs
// wherever the succeeded run's event lands, and it lands on every node: the
// run event is broadcast, and the agent and the planner also consume it from
// the durable run-delivery path. Live on 2026-09-28 every check ran on the
// planner AND on the agent. The planner's copy failed (no_forwarded_authority:
// an automation carries no forwarded authority for a fleet hop) and journaled
// a failed model call onto the very run it was checking; the agent's opened
// the app session. So the designation is EXPLICIT, not inferred from what a
// node cannot reach today: once planner calls reach an app by forwarding to
// the agent holding the machine (the planner/app-source design, section 3a),
// "the planner cannot serve it" stops being true, and an inferred rule would
// quietly become a second session for one answer. A node that is not
// designated skips before it reads or claims anything, saying the agent
// serves it.
//
// Why the agent: it executes goal runs, so the succeeded status is written --
// and its event fires first -- on an agent; it consumes run delivery, so a
// broadcast it missed still reaches it; and it holds the machines' streams,
// so it reaches every source a check's route can name -- the owner's app
// doors and fleet directly, vendors like any node.
//
// THE CLAIM IS AMONG AGENT REPLICAS. One transition can reach an agent twice
// (the broadcast and the durable delivery carry different fingerprints, so the
// automation's own guard does not collapse them), and two agent replicas can
// each take one. The guard's own ClaimWithTTL, not StrictClaimer: a database
// the claim cannot reach degrades to a bounded number of unclaimed checks -- a
// duplicate model call at worst, never a duplicate side effect -- rather than
// to no checks at all.

import (
	"github.com/znasllc-io/memql/component/automations"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

func (a *App) wireAnswerChecks() {
	work := a.lookupWorkIntegration()
	if work == nil {
		a.Logger.Warn("answer checks not served: the work integration did not materialize on this agent node, so no node checks a goal run's answer",
			"component", "work.validator")
		return
	}
	if a.clusterGuard == nil {
		// Served unclaimed rather than not served: a check that may run twice
		// is better than no check.
		a.Logger.Warn("answer checks served unclaimed: no cluster execution guard on this node, so two agent replicas handed one run transition may each check it",
			"component", "work.validator")
		work.ServeAnswerChecks(nil)
		return
	}
	work.ServeAnswerChecks(a.clusterGuard)
}

// The guard IS the claimer the work integration declares; a signature change
// on either side is caught here rather than as a validator that silently runs
// unclaimed.
var _ workspine.RunClaimer = (*automations.ClusterExecutionGuard)(nil)
