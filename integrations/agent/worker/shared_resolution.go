//go:build agent || planner

package worker

// SHARED RESOLUTION for a USER's call (epic memql#5146, design D6).
//
// ===========================================================================
// WHAT CHANGES AND WHAT DOES NOT
// ===========================================================================
// Before this, a user's model call could land only on a machine they owned:
// PlanModel reads WorkersForOwner, which cannot see anybody else's. That is
// what made "local by default" per PERSON rather than per COMPANY -- a business
// with one Mac Studio in the office had no way to make it serve the team.
//
// This adds the second half: a user's call may also land on a machine whose
// owner offered it to the cluster AND whose cockpit is willing to serve it.
// Nothing about the OWNERSHIP boundary moves. A machine reaches this list only
// because its owner put it there, and the refusal for every machine that does
// not names which of the two consents is missing.
//
// ===========================================================================
// preferOwnMachines
// ===========================================================================
// A caller's OWN machines come first, always, and the ordering is not a
// preference an operator can turn off. Three reasons, and the third is the one
// that would be missed:
//
//   - a person's own machine is the one they are paying for and the one whose
//     load they can see;
//   - a shared machine belongs to somebody who agreed to help, not to be the
//     default, and burning their GPU while the caller's own laptop is idle is
//     a poor way to treat the offer;
//   - and it keeps the common case UNCHANGED. Somebody with their own machine
//     sees exactly the routing they saw before this epic, so a fleet that
//     starts sharing does not silently reroute work that was already working.
//
// It is applied as a STABLE partition rather than a sort key, so within each
// half the policy's own strategy still decides -- roundRobin still rotates,
// leastLoaded still rations. Own-first is a boundary between two groups, not a
// new comparator inside them.

import (
	"context"
	"fmt"
	"strings"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// PlanUserModelWithShared orders the machines eligible to serve one user's call
// on one model: their own first, then the ones shared with the cluster.
//
// The caller's own machines are planned exactly as PlanModel plans them --
// under the caller's own routing policy, which is theirs to state. The shared
// half is NOT re-ordered by that policy, for the reason PlanSharedModel gives:
// a routing policy expresses how somebody wants THEIR machines used, and
// applying it to another person's machine would let a preference travel across
// the ownership boundary this file exists to hold.
func (r *Router) PlanUserModelWithShared(
	ctx context.Context,
	actingUserId string,
	modelId string,
	needs ModelNeeds,
) (RoutePlan, error) {
	if r == nil || r.store == nil {
		return RoutePlan{Policy: DefaultPolicy()}, fmt.Errorf("worker router: no fleet store configured")
	}
	if strings.TrimSpace(actingUserId) == "" {
		// No acting user is SYSTEM work, and system work has no own half at
		// all. Falling through to the shared plan is the honest answer rather
		// than an empty one.
		return r.PlanSharedModel(ctx, modelId, needs)
	}

	own, err := r.PlanModel(ctx, actingUserId, modelId, needs)
	if err != nil {
		return own, err
	}

	shared, ok := r.store.(SharedFleetStore)
	if !ok {
		// A node whose store cannot read across owners serves the caller's own
		// machines and says nothing about anybody else's. That is a NARROWER
		// answer, not a wrong one, and narrowing is the safe direction here.
		return own, nil
	}
	all, err := shared.SharedInferenceWorkers(ctx)
	if err != nil {
		// The own half is a complete answer to a narrower question, so a failed
		// cross-owner read degrades to it rather than failing the call. A
		// person whose own laptop can serve the turn should not be refused
		// because a read about somebody else's machine went wrong.
		if r.logger != nil {
			r.logger.Warn("worker router: could not read the shared fleet; serving the caller's own machines only",
				"acting_user_id", actingUserId, "error", err)
		}
		return own, nil
	}

	// ownSeen is every registration the owner-scoped plan already judged --
	// kept OR ruled out. A machine in Rejected must not be re-admitted through
	// the shared list (that would route around the own plan's reasoning). A
	// machine the own plan never returned at all is different: share is for a
	// DIFFERENT account, and recovering the caller's own hardware must not
	// wait on cluster consent.
	ownSeen := map[string]bool{}
	for _, c := range own.Candidates {
		ownSeen[c.RegistrationId] = true
	}
	for id := range own.Rejected {
		ownSeen[id] = true
	}

	ownRecovered := make([]Candidate, 0)
	sharedKept := make([]Candidate, 0, len(all))
	rejected := map[string]string{}
	pendingShareNoise := map[string]string{}
	for id, why := range own.Rejected {
		rejected[id] = why
	}
	for _, c := range all {
		if ownSeen[c.RegistrationId] {
			// Already judged by the own plan -- kept or ruled out. Skipping
			// rather than de-duplicating later keeps the own-first boundary exact.
			continue
		}
		if sameSubjectId(c.OwnerUserId, actingUserId) {
			// The caller's own machine the owner-scoped read never returned.
			// Still THEIR hardware: evaluate without ServesCluster. Pairing a
			// machine must unlock Ask / Materialize / Nexus for that user without
			// Fleet share + inference.serve:cluster (those consents are only for
			// a different account).
			switch {
			case !c.RevokedAt.IsZero():
				rejected[c.RegistrationId] = "revoked"
			case !workerservice.StreamHeld(c.ConnectedNodeId, c.RevokedAt):
				rejected[c.RegistrationId] = "offline"
			case !c.SupportsCapability(workerservice.ModelCapability):
				rejected[c.RegistrationId] = "missing capability " + workerservice.ModelCapability
			default:
				ownRecovered = append(ownRecovered, c)
			}
			continue
		}
		// A shared-list row with no ownerUserId cannot be attributed. Do not
		// dress that as a cluster-share refusal -- that sentence sent owners
		// to turn on share for a machine that may already be theirs under a
		// different session identity (prod: passkey user vs email user).
		if strings.TrimSpace(c.OwnerUserId) == "" {
			rejected[c.RegistrationId] = "registration missing ownerUserId"
			continue
		}
		switch {
		case !c.ServesCluster():
			// Record foreign share refusals only when we may need them as the
			// sole signal. Owner Ask with a live owned worker must not drown in
			// SharingRefusal lines for other private machines (24ad under a
			// different identity).
			pendingShareNoise[c.RegistrationId] = c.SharingRefusal()
		case !c.RevokedAt.IsZero():
			rejected[c.RegistrationId] = "revoked"
		case !workerservice.StreamHeld(c.ConnectedNodeId, c.RevokedAt):
			rejected[c.RegistrationId] = "offline"
		case !c.SupportsCapability(workerservice.ModelCapability):
			rejected[c.RegistrationId] = "missing capability " + workerservice.ModelCapability
		default:
			sharedKept = append(sharedKept, c)
		}
	}

	// Recovered own machines follow the caller's routing policy, same as PlanModel.
	recoveredPlan := narrowToModel(RoutePlan{
		Policy:     own.Policy,
		Candidates: ownRecovered,
		Rejected:   map[string]string{},
		Total:      len(ownRecovered),
	}, modelId, needs)
	for id, why := range recoveredPlan.Rejected {
		rejected[id] = why
	}

	// The shared half is narrowed to the model and ordered under the DEFAULT
	// policy, never the caller's -- see the doc comment.
	sharedPlan := narrowToModel(RoutePlan{
		Policy:     DefaultPolicy(),
		Candidates: sharedKept,
		Rejected:   map[string]string{},
		Total:      len(sharedKept),
	}, modelId, needs)
	for id, why := range sharedPlan.Rejected {
		rejected[id] = why
	}

	// OWN FIRST (plan + recovered), then shared. Stable concatenation so each
	// half keeps the order its own policy gave it.
	out := own
	out.Candidates = append(append(append([]Candidate{}, own.Candidates...), recoveredPlan.Candidates...), sharedPlan.Candidates...)
	// Prefer live owned connected workers: when any own candidate remains,
	// drop foreign SharingRefusal noise from the rejected map so owner Ask
	// never surfaces 24ad-under-jmendivil as if it were the user's machine.
	hasOwnCandidate := false
	for _, c := range out.Candidates {
		if OwnMachineFirst(c, actingUserId) {
			hasOwnCandidate = true
			break
		}
	}
	if !hasOwnCandidate {
		for id, why := range pendingShareNoise {
			rejected[id] = why
		}
	}
	out.Rejected = rejected
	out.Total = own.Total + len(all)
	return out, nil
}

// OwnMachineFirst reports whether a candidate belongs to the acting user.
//
// It is the whole of `preferOwnMachines` as a predicate, exported so the
// decision record can say which half a pick came from -- "it chose a shared
// machine" and "it chose one of yours" are different answers to the same
// question, and only one of them needs explaining.
func OwnMachineFirst(c Candidate, actingUserId string) bool {
	return sameSubjectId(c.OwnerUserId, actingUserId)
}

// sameSubjectId compares two identity subjects tolerantly of the bare/canonical
// split, which is the one comparison in this package that a naive == gets
// wrong: the row carries a canonical id and a token's subject may be bare.
func sameSubjectId(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	return a == b || trimIdPrefix(a) == trimIdPrefix(b)
}

func trimIdPrefix(v string) string {
	if i := strings.LastIndex(v, ":"); i >= 0 {
		return v[i+1:]
	}
	return v
}
