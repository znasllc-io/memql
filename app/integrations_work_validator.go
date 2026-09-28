package app

// integrations_work_validator.go -- the cross-replica claim on the answer
// validator's one model call (integrations/work validator.go).
//
// WHY A CLAIM. The validateGoalAnswer automation's own cluster guard keys on
// the triggering event's fingerprint, and one run transition can reach a
// replica twice with two fingerprints: live on 2026-09-28 the agent and the
// planner each took run updates from the bridge AND again from the durable
// run-delivery path. On an agent, whose route puts Claude Code first, two
// copies of one check are two app sessions for one answer.
//
// WHY ON AGENT NODES ONLY. The agent is the node that can serve the check --
// it holds the machines' streams, so it reaches the owner's app doors and
// fleet directly. On the same cluster the planner's copy failed
// (no_forwarded_authority: an automation context carries no forwarded
// authority for a fleet hop) and the bff's found every door shut (no fleet on
// a bff). A cluster-wide claim would let such a replica win the claim, fail,
// and leave the agent's copy skipped: the check lost where today it succeeds.
// Claiming among agents removes the duplicate sessions and costs no coverage;
// a non-agent copy runs unclaimed exactly as before, and reaches no app door.
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
