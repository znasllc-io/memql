package auth

import (
	"sort"
	"strings"
)

// The CLUSTER'S MAINTENANCE PRINCIPAL (memql#4366 / memql#4406).
//
// connector_actor.go says of itself: "This is the characterised internal
// actor memql#4366 asks for, FOR THIS CLASS OF WRITER ONLY." This is its
// sibling, for the other class the issue measured -- the engine's own
// housekeeping READS, which span every owner by definition.
//
// # The problem
//
// component/automations' contextWithSystemActor stamps RoleReader,
// deliberately: "system" is not in AllRoles, and Reader keeps
// actor.isClusterOwner FALSE for an absent caller (memql#2801). That is right
// for an automation acting on one user's data, and it is FATAL for a retention
// sweep.
//
// Once a swept concept declares an owned-tier @rowAuthz, the injected
// predicate compares each row's owner against `system:automation:<name>`,
// matches nothing, and the sweep retires nothing -- with no error, no log line,
// and every gate green. The retention window silently stops meaning anything.
// memql#4406 measured that for v1:worker:invocation and refused to declare the
// tier until this principal existed, because the alternative was to discover it
// in production as "why is this table not being pruned".
//
// # The decision: an identity, not an escape hatch
//
// The argument is component/campaigns/worker.go's, quoted rather than
// re-derived because it is the one that settles it:
//
//	The alternative would be an escape hatch in the enforcement layer, which
//	is strictly worse -- a bypass is available to every caller that can reach
//	it, whereas an identity is only as powerful as the queries it is used for.
//
// So a listed automation runs as a NAMED synthetic cluster owner, and the
// queries it uses say so in their own filters (`actor.isClusterOwner==true`).
// That is what makes the arrangement legible at the read instead of only here,
// and it makes the failure mode loud: remove the principal and the query
// returns nothing, with the filter explaining why.
//
// # Why the list is compiled in and not a DSL annotation
//
// An `@maintenance` automation annotation was the obvious shape and is the
// wrong one. MEMQL_DSL_PATH mounts product DSL from disk at boot, so a DSL
// annotation conferring cluster-owner authority would let a product bundle
// grant ITSELF the cluster's maintenance principal -- privilege escalation by
// dropping a file into a volume. This list is compiled in, so a mounted bundle
// cannot reach it.
//
// It is also why MaintenanceActor being EXPORTED confers nothing: the authority
// comes from the list, never from the caller, so a name that is not on it gets
// nil however loudly it asks.
//
// # What keeps it small
//
// Every entry carries its reason, and TestMaintenanceAutomationsAreArgued pins
// the set -- an addition is a visible, argued change rather than a line in a
// map. Two properties an entry must have:
//
//   - The automation is ENGINE-OWNED (it lives in this repo's dsl/ tree). An
//     authored or product automation must never appear.
//   - Its reads span owners BY NATURE -- a sweep, a cross-owner roll-up --
//     rather than merely finding an owner-scoped read inconvenient. The fix for
//     the latter is ContextWithUserActor at the call site, which borrows
//     exactly ONE owner's authority: component/worker's store, the campaigns
//     drain worker, the workbench integration.

// maintenanceUserIdPrefix is the prefix of the synthetic UserId a maintenance
// principal carries.
//
// Distinct from `system:automation:` on purpose. The two principals differ in
// exactly the way that matters -- one is a cluster owner and one is a reader --
// so a `createdBy` stamp, an audit line and a log field all say which ran.
// Reusing the prefix would make the elevated case indistinguishable from the
// ordinary one in every stored record.
//
// It cannot collide with a real user id: ids are minted by the identity service
// and none carry this prefix.
const maintenanceUserIdPrefix = "system:maintenance:"

// maintenanceAutomations maps each automation that runs under the cluster's
// maintenance principal to WHY it needs one.
var maintenanceAutomations = map[string]string{
	"workerInvocationRetentionSweep": "retention sweep over v1:worker:invocation, whose composite owner tier " +
		"(memql#4406) would otherwise hide every row from it -- silently, because a sweep that retires " +
		"nothing looks exactly like a sweep with nothing to retire",
	"auditEventRetentionSweep": "retention sweep over v1:identity:auditEvent, which declares the clusterOwner " +
		"tier (memql#4366). Worse than the one above, because this sweep is OBSERVATION-ONLY: it publishes a " +
		"candidate COUNT, so an unauthorized read does not fail, it reports zero -- and a retention window " +
		"nobody enforces is indistinguishable from one with nothing to do",
	"seedSelfAccount": "the boot seed for v1:accounts:account:self, whose concept declares the composite " +
		"owner tier (epic memql#4800). It reads existingSelfAccount to decide whether the owner's own company " +
		"has been created yet, and the row is CLUSTER-OWNED -- ownerUserId empty -- so the owned branch of the " +
		"tier can never match it and only the cluster-owner escape can see it at all. The failure this prevents " +
		"is sharper than either sweep above, and in the opposite direction: a narrowed read here does not skip " +
		"work, it reports the row ABSENT, and absent means CREATE. The seed would therefore re-run on every " +
		"boot forever, which is exactly the clobbering of a human's edits that decision D3 exists to forbid. " +
		"@createOnly on createClientAccount is the second guard, and it would hold -- but a probe that is " +
		"wrong on every call is not something to leave standing behind a guard",
}

// IsMaintenanceAutomation reports whether this automation runs under the
// cluster's maintenance principal.
func IsMaintenanceAutomation(name string) bool {
	_, ok := maintenanceAutomations[strings.TrimSpace(name)]
	return ok
}

// MaintenanceActor builds the AccessContext a listed automation runs under, or
// nil for any name that is not listed.
//
// RoleOwner is the whole point: the composite tier's escape is
// IsClusterOwner(), which is RoleOwner alone, and there is no other way past it
// (rowauthz_enforce.go). Returning nil rather than a reader for an unlisted name
// keeps the decision here -- a caller cannot half-succeed into a principal that
// looks elevated and is not.
func MaintenanceActor(automationName string) *AccessContext {
	name := strings.TrimSpace(automationName)
	if !IsMaintenanceAutomation(name) {
		return nil
	}
	return &AccessContext{
		UserId: maintenanceUserIdPrefix + name,
		Role:   RoleOwner,
		// Not a principal, so the rank rules do not govern it (D4, epic
		// memql#4832). RoleOwner above is what buys the cluster-owner
		// escape; it is NOT a claim to rank 400, and without this flag a
		// rank-strict concept would read this actor as an owner writing a
		// PEER owner's row and refuse the sweep.
		Unranked: true,
	}
}

// MaintenanceAutomationNames returns the listed automations, sorted. For gates
// and for an error message that has to state what the list holds.
func MaintenanceAutomationNames() []string {
	out := make([]string, 0, len(maintenanceAutomations))
	for name := range maintenanceAutomations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// MaintenanceAutomationReason returns why an automation is on the list.
func MaintenanceAutomationReason(name string) string {
	return maintenanceAutomations[strings.TrimSpace(name)]
}
