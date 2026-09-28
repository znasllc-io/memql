package app

// integrations_work_validator.go -- the cross-replica claim on the answer
// validator's one model call (integrations/work validator.go).
//
// EVERY NODE TYPE, because the validateGoalAnswer automation fires on every
// node that loads the work automations and the cluster guard decides which one
// runs it. That guard keys on the triggering event's fingerprint, and one run
// transition can reach a replica twice with two fingerprints -- live on
// 2026-09-28 the planner took each run update from the bridge and again from
// the durable run-delivery path -- so without this the agent and the planner
// each checked one answer, and on a route that puts an app first each check
// could open its own session.
//
// The guard's own ClaimWithTTL, not StrictClaimer: a database the claim
// cannot reach degrades to a bounded number of unclaimed checks -- a duplicate
// model call at worst, never a duplicate side effect -- rather than to no
// checks at all.

import (
	"github.com/znasllc-io/memql/component/automations"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

func (a *App) wireWorkValidatorClaim() {
	work := a.lookupWorkIntegration()
	if work == nil {
		return
	}
	if a.clusterGuard == nil {
		a.Logger.Warn("answer validator claim not wired: no cluster execution guard on this node, so two replicas handed one run transition may each check it",
			"component", "work.validator")
		return
	}
	work.SetValidatorClaimer(a.clusterGuard)
}

// The guard IS the claimer the work integration declares; a signature change
// on either side is caught here rather than as a validator that silently runs
// unclaimed.
var _ workspine.RunClaimer = (*automations.ClusterExecutionGuard)(nil)
