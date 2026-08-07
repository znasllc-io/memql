package memql

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// rowauthz_undeclared_gate_test.go -- memql#3173, carrying memql#3077
// (the ratchet) and memql#3135 (the reason slot).
//
// # THE LIST BELOW MAY ONLY SHRINK
//
// `@rowAuthz` (memql#2920) makes a concept DECLARE who may see its rows,
// and every downstream phase -- shadow mode, the owner-stamping gate,
// enforcement -- reaches only the concepts that declare one. Everything
// else is not "safe" and not "unchanged"; it is unmeasured, and shadow
// mode's own report already says so in those words:
//
//	--- NOT MEASURABLE: concept declares no tier ---
//	  These are not 'no change' -- they are 'not measured'.
//
// That undeclared population is where the credential surface lives:
// `badgesForUser`, `authSessionByRefreshTokenHash`, `auditEventsByActor`,
// `activeUsers`, `patIdentityByKeyHash`. The coverage is inverted against
// risk, and before this file the only signal over it was a boot-time
// warning -- a warning that has been true for the same ~170 constructs
// for months, which is not a signal, it is wallpaper.
//
// So the debt is enumerated once, as it stood the day the gate landed,
// and the gate then fails in BOTH directions:
//
//   - ADDING an entry means a new query was written over a concept that
//     still declares nothing. That is new debt on the exact surface this
//     epic exists to close. Declare a tier on the concept instead; if an
//     entry is genuinely unavoidable it must carry its own filed issue as
//     its reason, which is what makes it visibly different from the seed.
//   - REMOVING an entry is the point of the file, and happens by
//     declaring a tier on the concept (or by deleting the query). The
//     gate then fails until the entry is deleted, because a stale entry
//     silently suppresses that construct forever. The sibling gate states
//     the rule: "a stale exemption is as bad as a missing gate"
//     (ownerGateExemptions, memql#2982).
//
// # THIS FILE DECLARES A TIER ON NOTHING
//
// Deliberately, and it is not an oversight to be tidied up later. Each
// identity entry below is a real authorization judgment -- `activeUsers`
// and `badgesForUser` are not obviously owner-scoped, and several of
// these reads are legitimately cross-user admin reads. Burying dozens of
// security decisions inside a tooling change is precisely the outcome
// this gate exists to make impossible to do quietly.
//
// # WHY IT DERIVES FROM THE LOADED REGISTRY
//
// The population is read off the loaded FunctionRegistry and the loaded
// concept registry, never off a scan of the .memql source, so the list is
// a pin on the tree rather than a second hand-maintained copy of it. A
// source scanner would have to re-derive which construct binds which
// concept and whether that concept's `@rowAuthz` survived the parse; the
// loader already carries the outcome. That is the memql#2875 lesson,
// applied here for the same reason OwnerFieldProvenance applies it.

// undeclaredGrandfatherReason is what every seed entry carries.
//
// It says "population, not triage" because that is what happened: the set
// was enumerated in one pass and no single entry on it was individually
// judged. A per-entry fiction ("admin read, intentional") would record a
// claim nobody actually made. The shared marker also makes a NEW entry
// visibly different at a glance -- a new one has to name its own issue.
const undeclaredGrandfatherReason = "memql#3173 seed -- grandfathered as a population, not individually triaged"

// undeclared3178SelfScopedReason covers the two constructs memql#3178
// introduced while splitting the per-user credential lists off a
// caller-supplied id.
//
// They are on this list, and they are deliberately NOT carrying the
// grandfather marker, because they are not grandfathered -- they were added
// after the seed and each names its own issue, which is exactly the
// distinction the marker exists to make visible.
//
// Being listed here does not mean they are unscoped. Both filter on
// `userId==actor.userId`, so they are strictly narrower than the
// `userId==args.userId` constructs they replaced. What keeps them on the
// list is the concept: `v1:identity:identity` still declares no `@rowAuthz`
// tier, so the engine measures nothing about them and this gate's whole
// point is that unmeasured is not the same as safe.
//
// Declaring the tier is NOT this epic's job -- epic rowauthz-enforcement
// Decision D holds that each of the 48 identity declarations is its own
// authorization judgment and burying them inside a tooling change is the
// outcome this gate exists to prevent.
const undeclared3178SelfScopedReason = "memql#3178 -- self-scoped on actor.userId; v1:identity:identity still declares no tier (epic Decision D)"

// undeclared3217SeedSweepReason covers usersForSeedSweep, added by memql#3217
// so the startup per-user seed sweep reads a COMPLETE user set instead of
// activeUsers' newest-50 page.
//
// It is on this list and deliberately not carrying the grandfather marker: it
// postdates the seed and names its own issue, which is the distinction the
// marker exists to draw.
//
// Being listed here is not a claim that it is unscoped debt of the usual kind.
// It cannot be caller-scoped at all -- it runs under the seed materializer's
// system actor at boot, where there is no requesting user for actor.userId to
// name, and a filter naming one would evaluate against an empty actor and
// sweep nobody (the same circularity that keeps activeUsers @serverOnly rather
// than self-scoped, #2800/#2883). What keeps it here is the concept:
// v1:identity:user declares no `@rowAuthz` tier, so the engine measures
// nothing about it, and unmeasured is not the same as safe. Its projection is
// userIdRef -- row.id alone, no @pii field -- which bounds the exposure but
// does not measure it.
const undeclared3217SeedSweepReason = "memql#3217 -- system-actor startup sweep, uncaller-scopable by construction; v1:identity:user still declares no tier (epic Decision D)"

// undeclaredEntry is the map's value type, spelled as an alias so the
// literal below reads exactly as memql#3135 specified it while the
// classifier can still name the type in a signature.
//
// #3135 is the whole reason the value is a struct rather than a bare
// concept id. The sibling gate's value IS its tracking reason; the parked
// #3077 attempt repurposed that slot for the concept id, gaining a real
// re-bind check and losing the reason field. Both are load-bearing and
// both are kept here: the concept id catches a construct silently
// re-bound to a different concept, and the reason is what stops an entry
// from being added with no accounting for why.
type undeclaredEntry = struct {
	concept string
	reason  string
}

// undeclaredRowAuthzConstructs pins every query construct whose bound
// concept declares no `@rowAuthz` tier. Shrink-only -- read the header
// before touching it.
var undeclaredRowAuthzConstructs = map[string]struct {
	concept string
	reason  string
}{
	// v1:agents:agent
	"activeAgents":          {"v1:agents:agent", undeclaredGrandfatherReason},
	"activeAgentsForUser":   {"v1:agents:agent", undeclaredGrandfatherReason},
	"agentById":             {"v1:agents:agent", undeclaredGrandfatherReason},
	"agentOwner":            {"v1:agents:agent", undeclaredGrandfatherReason},
	"agentRoleSlugsInUse":   {"v1:agents:agent", undeclaredGrandfatherReason},
	"agentsForRegistry":     {"v1:agents:agent", undeclaredGrandfatherReason},
	"allAgents":             {"v1:agents:agent", undeclaredGrandfatherReason},
	"assistantAgentForUser": {"v1:agents:agent", undeclaredGrandfatherReason},

	// v1:agents:agentAuthorization -- DEBT PAID, entry DELETED (memql#3177).
	// The seed entry here was `agentAuthorizationsForUser`. The concept now
	// declares `@rowAuthz(owner="userId")` and the construct is
	// `agentAuthorizationsForSelf` filtering `userId==actor.userId`, so it is
	// MEASURED rather than unmeasured and this file's own rule applies: a
	// stale entry silently suppresses that construct forever. This comment is
	// not an entry -- the map is one shorter than it was, which is the only
	// direction it may move.

	// v1:agents:agentRole
	"activeAgentRoles": {"v1:agents:agentRole", undeclaredGrandfatherReason},
	"agentRoleBySlug":  {"v1:agents:agentRole", undeclaredGrandfatherReason},

	// v1:agents:avatarPersona
	"avatarPersonaById": {"v1:agents:avatarPersona", undeclaredGrandfatherReason},
	"avatarPersonas":    {"v1:agents:avatarPersona", undeclaredGrandfatherReason},

	// v1:agents:skill
	"activeSkills":      {"v1:agents:skill", undeclaredGrandfatherReason},
	"activeSkillsFull":  {"v1:agents:skill", undeclaredGrandfatherReason},
	"skillBySlug":       {"v1:agents:skill", undeclaredGrandfatherReason},
	"skillNeedsRefresh": {"v1:agents:skill", undeclaredGrandfatherReason},

	// v1:agents:skillChangeEvent
	"skillChangeEventsForAgent": {"v1:agents:skillChangeEvent", undeclaredGrandfatherReason},

	// v1:authoring:bundle
	"activeAuthoringBundles":           {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"authoringBundleById":              {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"authoringBundleForPlan":           {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"authoringBundleForResponsibility": {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"authoringBundlesForOwner":         {"v1:authoring:bundle", undeclaredGrandfatherReason},
	"systemActiveAuthoringBundles":     {"v1:authoring:bundle", undeclaredGrandfatherReason},

	// v1:cluster:cluster
	"existingCluster": {"v1:cluster:cluster", undeclaredGrandfatherReason},

	// v1:cluster:deployment
	"deploymentById":        {"v1:cluster:deployment", undeclaredGrandfatherReason},
	"deploymentsForCluster": {"v1:cluster:deployment", undeclaredGrandfatherReason},
	"supersededDeployments": {"v1:cluster:deployment", undeclaredGrandfatherReason},

	// v1:cluster:deploymentNodeSpec
	"nodeSpecsForDeployment": {"v1:cluster:deploymentNodeSpec", undeclaredGrandfatherReason},

	// v1:cluster:node
	"clusterNodes":         {"v1:cluster:node", undeclaredGrandfatherReason},
	"nodesForDeployment":   {"v1:cluster:node", undeclaredGrandfatherReason},
	"nodesNotInDeployment": {"v1:cluster:node", undeclaredGrandfatherReason},
	"staleClusterNodes":    {"v1:cluster:node", undeclaredGrandfatherReason},

	// v1:cluster:nodeType
	"clusterNodeTypes": {"v1:cluster:nodeType", undeclaredGrandfatherReason},

	// v1:cluster:spawnEvent
	"clusterSpawnEvents": {"v1:cluster:spawnEvent", undeclaredGrandfatherReason},

	// v1:cognition:audioOverride
	"audioOverridesForSpace": {"v1:cognition:audioOverride", undeclaredGrandfatherReason},

	// v1:cognition:participant
	"activeHumanParticipants": {"v1:cognition:participant", undeclaredGrandfatherReason},
	"groupGAForSpace":         {"v1:cognition:participant", undeclaredGrandfatherReason},
	"participantByAgentSpace": {"v1:cognition:participant", undeclaredGrandfatherReason},
	"siParticipantForSpace":   {"v1:cognition:participant", undeclaredGrandfatherReason},
	"spaceParticipants":       {"v1:cognition:participant", undeclaredGrandfatherReason},

	// v1:cognition:participant:presence
	"spaceParticipantPresence": {"v1:cognition:participant:presence", undeclaredGrandfatherReason},

	// v1:cognition:session
	"participantSession": {"v1:cognition:session", undeclaredGrandfatherReason},

	// v1:cognition:utterance
	"agentInteractionCount":       {"v1:cognition:utterance", undeclaredGrandfatherReason},
	"feedbackAnnouncementForPlan": {"v1:cognition:utterance", undeclaredGrandfatherReason},
	"greetingUtterance":           {"v1:cognition:utterance", undeclaredGrandfatherReason},
	"hasAIResponseForReply":       {"v1:cognition:utterance", undeclaredGrandfatherReason},
	"spaceUtterances":             {"v1:cognition:utterance", undeclaredGrandfatherReason},

	// v1:cognition:videoOverride
	"videoOverridesForSpace": {"v1:cognition:videoOverride", undeclaredGrandfatherReason},

	// v1:common:attachment
	"attachmentById": {"v1:common:attachment", undeclaredGrandfatherReason},

	// v1:common:media
	"spaceMedia": {"v1:common:media", undeclaredGrandfatherReason},

	// v1:data:log
	"validationLog": {"v1:data:log", undeclaredGrandfatherReason},

	// v1:data:policy
	"policy": {"v1:data:policy", undeclaredGrandfatherReason},

	// v1:data:record
	"detectConflicts": {"v1:data:record", undeclaredGrandfatherReason},
	"recordsByState":  {"v1:data:record", undeclaredGrandfatherReason},
	"usableRecords":   {"v1:data:record", undeclaredGrandfatherReason},

	// v1:forge:project
	"activeProjects": {"v1:forge:project", undeclaredGrandfatherReason},
	"projectById":    {"v1:forge:project", undeclaredGrandfatherReason},
	"projectBySlug":  {"v1:forge:project", undeclaredGrandfatherReason},

	// v1:forge:request
	"approvalQueue":   {"v1:forge:request", undeclaredGrandfatherReason},
	"myRequests":      {"v1:forge:request", undeclaredGrandfatherReason},
	"projectRequests": {"v1:forge:request", undeclaredGrandfatherReason},
	"requestById":     {"v1:forge:request", undeclaredGrandfatherReason},
	"validationQueue": {"v1:forge:request", undeclaredGrandfatherReason},

	// v1:forge:requestEvent
	"requestEvents": {"v1:forge:requestEvent", undeclaredGrandfatherReason},

	// v1:identity:accessRequest
	"accessRequestById":            {"v1:identity:accessRequest", undeclaredGrandfatherReason},
	"expiredPendingAccessRequests": {"v1:identity:accessRequest", undeclaredGrandfatherReason},
	"pendingAccessRequests":        {"v1:identity:accessRequest", undeclaredGrandfatherReason},

	// v1:identity:accountEntitlement
	"accountEntitlement": {"v1:identity:accountEntitlement", undeclaredGrandfatherReason},

	// v1:identity:auditEvent
	"auditEventsByActor":  {"v1:identity:auditEvent", undeclaredGrandfatherReason},
	"auditEventsByTarget": {"v1:identity:auditEvent", undeclaredGrandfatherReason},
	"expiredAuditEvents":  {"v1:identity:auditEvent", undeclaredGrandfatherReason},
	"recentAuditEvents":   {"v1:identity:auditEvent", undeclaredGrandfatherReason},

	// v1:identity:authCode
	"authCodeByCodeHash":       {"v1:identity:authCode", undeclaredGrandfatherReason},
	"expiredConsumedAuthCodes": {"v1:identity:authCode", undeclaredGrandfatherReason},

	// v1:identity:authSession
	"authSessionByPreviousRefreshTokenHash": {"v1:identity:authSession", undeclaredGrandfatherReason},
	"authSessionByRefreshTokenHash":         {"v1:identity:authSession", undeclaredGrandfatherReason},
	"authSessionByTokenHash":                {"v1:identity:authSession", undeclaredGrandfatherReason},
	"authSessionsForSubject":                {"v1:identity:authSession", undeclaredGrandfatherReason},

	// v1:identity:clusterSettings
	"clusterSettingsCurrent": {"v1:identity:clusterSettings", undeclaredGrandfatherReason},

	// v1:identity:delegation
	"activeDelegationsByIdentitySubject": {"v1:identity:delegation", undeclaredGrandfatherReason},
	"activeDelegationsForAgent":          {"v1:identity:delegation", undeclaredGrandfatherReason},
	"delegationsByIdentity":              {"v1:identity:delegation", undeclaredGrandfatherReason},
	"expiredActiveDelegations":           {"v1:identity:delegation", undeclaredGrandfatherReason},

	// v1:identity:identity
	"badgeByKeyHash":             {"v1:identity:identity", undeclaredGrandfatherReason},
	"badgesForSelf":              {"v1:identity:identity", undeclared3178SelfScopedReason},
	"nodeTokenIdentities":        {"v1:identity:identity", undeclaredGrandfatherReason},
	"nodeTokenIdentityByBinding": {"v1:identity:identity", undeclaredGrandfatherReason},
	"nodeTokenIdentityById":      {"v1:identity:identity", undeclaredGrandfatherReason},
	"patIdentitiesForSelf":       {"v1:identity:identity", undeclared3178SelfScopedReason},
	"patIdentitiesForUser":       {"v1:identity:identity", undeclaredGrandfatherReason},
	"patIdentityById":            {"v1:identity:identity", undeclaredGrandfatherReason},
	"patIdentityByKeyHash":       {"v1:identity:identity", undeclaredGrandfatherReason},
	"workerTokenByKeyHash":       {"v1:identity:identity", undeclaredGrandfatherReason},
	"workerTokensForUser":        {"v1:identity:identity", undeclaredGrandfatherReason},

	// v1:identity:invitation
	"invitationById":                {"v1:identity:invitation", undeclaredGrandfatherReason},
	"invitationByPreviousTokenHash": {"v1:identity:invitation", undeclaredGrandfatherReason},
	"invitationByTokenHash":         {"v1:identity:invitation", undeclaredGrandfatherReason},

	// v1:identity:magicLinkRequest
	"expiredMagicLinkRequests":    {"v1:identity:magicLinkRequest", undeclaredGrandfatherReason},
	"magicLinkRequestByTokenHash": {"v1:identity:magicLinkRequest", undeclaredGrandfatherReason},

	// v1:identity:oauthClient
	"oAuthClientByClientId": {"v1:identity:oauthClient", undeclaredGrandfatherReason},

	// v1:identity:user
	"activeUsers":               {"v1:identity:user", undeclaredGrandfatherReason},
	"currentUser":               {"v1:identity:user", undeclaredGrandfatherReason},
	"searchUsers":               {"v1:identity:user", undeclaredGrandfatherReason},
	"userActiveSpace":           {"v1:identity:user", undeclaredGrandfatherReason},
	"userByEmail":               {"v1:identity:user", undeclaredGrandfatherReason},
	"userById":                  {"v1:identity:user", undeclaredGrandfatherReason},
	"userByIdSystem":            {"v1:identity:user", undeclaredGrandfatherReason},
	"userCount":                 {"v1:identity:user", undeclaredGrandfatherReason},
	"userDisplayById":           {"v1:identity:user", undeclaredGrandfatherReason},
	"usersActiveInSpace":        {"v1:identity:user", undeclaredGrandfatherReason},
	"usersForSeedSweep":         {"v1:identity:user", undeclared3217SeedSweepReason},
	"usersInDeletionCooldown":   {"v1:identity:user", undeclaredGrandfatherReason},
	"usersScheduledForDeletion": {"v1:identity:user", undeclaredGrandfatherReason},

	// v1:identity:workerPairingCode
	"workerPairingCodeByHash": {"v1:identity:workerPairingCode", undeclaredGrandfatherReason},

	// v1:knowledge:documentChunk
	"allDocumentChunkDomains": {"v1:knowledge:documentChunk", undeclaredGrandfatherReason},
	"documentChunksForDomain": {"v1:knowledge:documentChunk", undeclaredGrandfatherReason},

	// v1:library:artifact
	"libraryArtifactById":         {"v1:library:artifact", undeclaredGrandfatherReason},
	"libraryArtifacts":            {"v1:library:artifact", undeclaredGrandfatherReason},
	"libraryArtifactsByKind":      {"v1:library:artifact", undeclaredGrandfatherReason},
	"libraryArtifactsByLens":      {"v1:library:artifact", undeclaredGrandfatherReason},
	"libraryWorkspaceLiveSources": {"v1:library:artifact", undeclaredGrandfatherReason},

	// v1:planner:plan
	"activePlansForUser":               {"v1:planner:plan", undeclaredGrandfatherReason},
	"allPlans":                         {"v1:planner:plan", undeclaredGrandfatherReason},
	"awaitingFeedbackPlansPastTimeout": {"v1:planner:plan", undeclaredGrandfatherReason},
	"historicalPlanMetrics":            {"v1:planner:plan", undeclaredGrandfatherReason},
	"planById":                         {"v1:planner:plan", undeclaredGrandfatherReason},
	"plansForResponsibility":           {"v1:planner:plan", undeclaredGrandfatherReason},
	"plansForSpace":                    {"v1:planner:plan", undeclaredGrandfatherReason},
	"runningPlansForUser":              {"v1:planner:plan", undeclaredGrandfatherReason},
	"strandedCandidatePlans":           {"v1:planner:plan", undeclaredGrandfatherReason},
	"waitingPlansForUser":              {"v1:planner:plan", undeclaredGrandfatherReason},

	// v1:planner:responsibility
	"activeResponsibilities":            {"v1:planner:responsibility", undeclaredGrandfatherReason},
	"activeResponsibilitiesAcrossUsers": {"v1:planner:responsibility", undeclaredGrandfatherReason},
	"dueResponsibilities":               {"v1:planner:responsibility", undeclaredGrandfatherReason},
	"responsibilitiesForUser":           {"v1:planner:responsibility", undeclaredGrandfatherReason},
	"responsibilityById":                {"v1:planner:responsibility", undeclaredGrandfatherReason},

	// v1:planner:task
	"tasksForPlan": {"v1:planner:task", undeclaredGrandfatherReason},

	// v1:planner:taskState
	"taskStateById": {"v1:planner:taskState", undeclaredGrandfatherReason},

	// v1:platform:globalVariable
	"globalVariable":  {"v1:platform:globalVariable", undeclaredGrandfatherReason},
	"globalVariables": {"v1:platform:globalVariable", undeclaredGrandfatherReason},
	"userDefaults":    {"v1:platform:globalVariable", undeclaredGrandfatherReason},

	// v1:platform:inboundRequest
	"inboundRequestByDedupeKey": {"v1:platform:inboundRequest", undeclaredGrandfatherReason},
	"inboundRequestsByStatus":   {"v1:platform:inboundRequest", undeclaredGrandfatherReason},

	// v1:platform:missingCapability
	"missingCapabilitiesByStatus":    {"v1:platform:missingCapability", undeclaredGrandfatherReason},
	"missingCapabilityByKindAndName": {"v1:platform:missingCapability", undeclaredGrandfatherReason},

	// v1:platform:outboundRequest
	"outboundRequestsByStatus": {"v1:platform:outboundRequest", undeclaredGrandfatherReason},

	// v1:platform:policyTrace
	"allPolicyTraces":       {"v1:platform:policyTrace", undeclaredGrandfatherReason},
	"policyTracesForPolicy": {"v1:platform:policyTrace", undeclaredGrandfatherReason},

	// v1:rbac:capability
	"activeCapabilities":          {"v1:rbac:capability", undeclaredGrandfatherReason},
	"capabilitiesForResourceType": {"v1:rbac:capability", undeclaredGrandfatherReason},
	"capabilitiesForRole":         {"v1:rbac:capability", undeclaredGrandfatherReason},
	"capabilityGrant":             {"v1:rbac:capability", undeclaredGrandfatherReason},

	// v1:rbac:role
	"activeRoles": {"v1:rbac:role", undeclaredGrandfatherReason},
	"roleBySlug":  {"v1:rbac:role", undeclaredGrandfatherReason},

	// v1:router:budget
	"routerBudgets": {"v1:router:budget", undeclaredGrandfatherReason},

	// v1:safety:approvalRequest
	"activeApprovalsByCorrelationKey": {"v1:safety:approvalRequest", undeclaredGrandfatherReason},
	"approvalRequestById":             {"v1:safety:approvalRequest", undeclaredGrandfatherReason},

	// v1:safety:classification
	"allSafetyClassifications": {"v1:safety:classification", undeclaredGrandfatherReason},

	// v1:safety:outputScreening
	"allOutputScreenings": {"v1:safety:outputScreening", undeclaredGrandfatherReason},

	// v1:telephony:consent
	"consentOptOut": {"v1:telephony:consent", undeclaredGrandfatherReason},

	// v1:telephony:number
	"allNumbers":         {"v1:telephony:number", undeclaredGrandfatherReason},
	"numberByE164":       {"v1:telephony:number", undeclaredGrandfatherReason},
	"numbersByPartition": {"v1:telephony:number", undeclaredGrandfatherReason},

	// v1:workbench:workspace
	"provisionedWorkspaces": {"v1:workbench:workspace", undeclaredGrandfatherReason},
	"workspaceForPlan":      {"v1:workbench:workspace", undeclaredGrandfatherReason},

	// v1:worker:invocation
	"expiredWorkerInvocations": {"v1:worker:invocation", undeclaredGrandfatherReason},
	"invocationsForPlan":       {"v1:worker:invocation", undeclaredGrandfatherReason},
	"invocationsForUser":       {"v1:worker:invocation", undeclaredGrandfatherReason},

	// v1:worker:registration
	"workerByIdentityId": {"v1:worker:registration", undeclaredGrandfatherReason},
	"workersForUser":     {"v1:worker:registration", undeclaredGrandfatherReason},
}

// undeclaredWorld is the state the pinned list is judged against: what
// the LOADED tree says today.
type undeclaredWorld struct {
	// queryConcept maps every query construct in the registry to the
	// concept its signature binds ("" when it binds none). A construct
	// ABSENT from this map is not a query in the tree any more.
	queryConcept map[string]string
	// conceptLoaded reports whether a concept id resolves in the loaded
	// concept registry at all.
	//
	// Kept separate from conceptDeclares on purpose. Folding "the concept
	// is gone" into "the concept does not declare a tier" is the exact
	// defect this gate was rebuilt to avoid -- see classifyUndeclared.
	conceptLoaded map[string]bool
	// conceptDeclares reports whether a LOADED concept carries a tier.
	conceptDeclares map[string]bool
}

// deriveUndeclaredWorld reads the world off the loaded registries.
func deriveUndeclaredWorld(registry *FunctionRegistry) undeclaredWorld {
	w := undeclaredWorld{
		queryConcept:    map[string]string{},
		conceptLoaded:   map[string]bool{},
		conceptDeclares: map[string]bool{},
	}
	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "query" {
			continue
		}
		concept := strings.TrimSpace(fn.BoundConcept)
		w.queryConcept[fn.Name] = concept
		if concept == "" {
			continue
		}
		c, err := memoryNodes.Get(concept)
		if err != nil || c == nil {
			continue
		}
		w.conceptLoaded[concept] = true
		w.conceptDeclares[concept] = c.RowAuthz != nil
	}
	return w
}

// undeclaredDebt is the population the list pins: query constructs bound
// to a loaded concept that declares no tier, as construct -> concept.
func undeclaredDebt(w undeclaredWorld) map[string]string {
	debt := map[string]string{}
	for name, concept := range w.queryConcept {
		if concept == "" || !w.conceptLoaded[concept] || w.conceptDeclares[concept] {
			continue
		}
		debt[name] = concept
	}
	return debt
}

// undeclaredFindings is one classification pass. Every field is a list of
// human-readable lines; empty everywhere means the list and the tree
// agree exactly.
type undeclaredFindings struct {
	// newDebt: derived, not listed. The list must grow to cover it, or --
	// preferably -- the concept must declare a tier.
	newDebt []string
	// debtPaid: listed, and the concept now declares a tier. Delete it.
	debtPaid []string
	// gone: listed, and the construct no longer exists. Delete it.
	gone []string
	// unresolvable: listed, the construct still exists, and its concept
	// could not be resolved at all. NOT the same as debt paid -- see
	// classifyUndeclared.
	unresolvable []string
	// rebound: listed against one concept, now bound to another.
	rebound []string
	// missingReason: listed with an empty reason (memql#3135).
	missingReason []string
}

func (f undeclaredFindings) empty() bool {
	return len(f.newDebt) == 0 && len(f.debtPaid) == 0 && len(f.gone) == 0 &&
		len(f.unresolvable) == 0 && len(f.rebound) == 0 && len(f.missingReason) == 0
}

// classifyUndeclared compares the pinned list against the derived world.
//
// # THE CLASSIFICATION THAT MUST NOT BE MADE
//
// A pinned entry that is no longer in the derived debt set has stopped
// being debt for one of several reasons, and they are NOT interchangeable:
//
//   - the concept declares a tier now -- debt paid, delete the entry;
//   - the construct is gone -- delete the entry;
//   - the construct is still there but its concept does not RESOLVE.
//
// The third is not "debt paid" and the entry must not be deleted for it.
// An earlier build of this gate treated any entry absent from the derived
// set as paid, so a single concept failing to resolve would have reported
// its constructs as debt paid and, taken at face value, emptied the whole
// ratchet in one commit -- the list is the only record of the population,
// so deleting it on a bad signal is unrecoverable by inspection.
//
// In the ordinary case a dropped concept takes its queries down with it
// and the run never reaches here: the loader skips those constructs and
// the gate's skip guard fatals first. This arm covers the case where it
// does not, and it exists because the cost of the two errors is wildly
// asymmetric -- an entry wrongly kept costs one investigation, an entry
// wrongly deleted costs the ratchet.
func classifyUndeclared(pinned map[string]undeclaredEntry, w undeclaredWorld) undeclaredFindings {
	var f undeclaredFindings
	debt := undeclaredDebt(w)

	for name, concept := range debt {
		if _, listed := pinned[name]; !listed {
			f.newDebt = append(f.newDebt, fmt.Sprintf("%s (%s)", name, concept))
		}
	}

	for name, entry := range pinned {
		if strings.TrimSpace(entry.reason) == "" {
			f.missingReason = append(f.missingReason, fmt.Sprintf("%s (%s)", name, entry.concept))
		}

		concept, isQuery := w.queryConcept[name]
		switch {
		case !isQuery:
			f.gone = append(f.gone, fmt.Sprintf(
				"%s -- listed against %s, but no query construct by that name is in the tree", name, entry.concept))
		case concept == "":
			f.unresolvable = append(f.unresolvable, fmt.Sprintf(
				"%s -- still a query, but it now binds no concept at all (listed against %s)", name, entry.concept))
		case !w.conceptLoaded[concept]:
			f.unresolvable = append(f.unresolvable, fmt.Sprintf(
				"%s -- still a query bound to %s, but that concept does not resolve in the loaded tree", name, concept))
		case concept != entry.concept:
			f.rebound = append(f.rebound, fmt.Sprintf(
				"%s -- listed against %s, now bound to %s", name, entry.concept, concept))
		case w.conceptDeclares[concept]:
			f.debtPaid = append(f.debtPaid, fmt.Sprintf(
				"%s -- %s declares a tier now", name, concept))
		}
	}

	for _, l := range [][]string{f.newDebt, f.debtPaid, f.gone, f.unresolvable, f.rebound, f.missingReason} {
		sort.Strings(l)
	}
	return f
}

// TestUndeclaredRowAuthzPopulationOnlyShrinks is the ratchet.
func TestUndeclaredRowAuthzPopulationOnlyShrinks(t *testing.T) {
	// Load SKIPS are load-bearing, exactly as they are for the sibling
	// owner gate (memql#2909): a construct that failed to parse is a
	// construct this gate cannot see, and silence must not read as a
	// pass. Worse here than there -- a skipped construct would be
	// classified as "gone" and its entry deleted, converting a parse
	// error into a permanent hole in the ratchet.
	conceptCount, conceptSkips, err := LoadUnifiedConceptsWithSkips(nil)
	if err != nil {
		t.Fatalf("LoadUnifiedConceptsWithSkips: %v", err)
	}
	if len(conceptSkips) > 0 {
		t.Fatalf("%d concept(s) failed to load, so this gate cannot see them: %v",
			len(conceptSkips), conceptSkips)
	}
	if conceptCount == 0 {
		t.Fatal("no concepts loaded")
	}

	registry := newFunctionRegistry()
	report := newLoadReport()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry(), report); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if len(report.Skipped) > 0 {
		t.Fatalf("%d construct(s) were skipped at load, so this gate cannot see them and would "+
			"report their pinned entries as gone:\n  %v", len(report.Skipped), report.Skipped)
	}

	world := deriveUndeclaredWorld(registry)
	if len(world.queryConcept) == 0 {
		t.Fatal("no query constructs loaded -- the derivation saw nothing, so every pinned entry " +
			"would classify as gone. That is a broken run, not a paid-off ratchet.")
	}
	if len(undeclaredDebt(world)) == 0 && len(undeclaredRowAuthzConstructs) > 0 {
		t.Fatal("the derived undeclared set is EMPTY while the pinned list is not. Either every " +
			"concept in the tree now declares a tier -- in which case this whole file should be " +
			"deleted in a change that says so -- or the derivation broke. Do not empty the list " +
			"on this signal alone; it is the only record of the population.")
	}

	f := classifyUndeclared(undeclaredRowAuthzConstructs, world)

	if len(f.newDebt) > 0 {
		t.Errorf(`%d query construct(s) read a concept that declares no @rowAuthz tier and are not on the list:

  %s

This list only shrinks. A construct here is a new read over an unmeasured concept -- the
population memql#2920's tier was introduced to close, on the surface where the credential
concepts live.

Two fixes, and they are different decisions:
  - Declare a tier on the concept (@rowAuthz(...)) -- the one that actually pays the debt,
    and the reason this gate exists.
  - Add the construct here WITH ITS OWN FILED ISSUE as the reason, if a declaration is
    genuinely blocked. The seed entries share a grandfather marker precisely so a new
    entry carrying an issue number stands out from them.`,
			len(f.newDebt), strings.Join(f.newDebt, "\n  "))
	}

	if len(f.debtPaid) > 0 {
		t.Errorf(`%d entry(ies) name a concept that DECLARES a tier now:

  %s

Delete them. The debt is paid, and a stale entry is as bad as a missing gate -- it
suppresses that construct for whoever writes the next query over it.`,
			len(f.debtPaid), strings.Join(f.debtPaid, "\n  "))
	}

	if len(f.gone) > 0 {
		t.Errorf(`%d entry(ies) name a construct that is no longer in the tree:

  %s

Delete them.`, len(f.gone), strings.Join(f.gone, "\n  "))
	}

	if len(f.unresolvable) > 0 {
		t.Errorf(`%d entry(ies) name a construct whose bound concept does not resolve:

  %s

This is NOT "debt paid" and these entries must not be deleted on this signal. A concept
that fails to resolve makes its constructs unjudgeable, not safe. Find out why the concept
is missing first -- an entry wrongly kept costs one investigation, an entry wrongly deleted
costs the ratchet's only record of the population.`,
			len(f.unresolvable), strings.Join(f.unresolvable, "\n  "))
	}

	if len(f.rebound) > 0 {
		t.Errorf(`%d entry(ies) are listed against one concept but bound to another:

  %s

The construct moved. Re-point the entry only after checking the new concept -- a construct
re-bound to a different undeclared concept is a different authorization question from the
one that was grandfathered.`, len(f.rebound), strings.Join(f.rebound, "\n  "))
	}

	if len(f.missingReason) > 0 {
		t.Errorf(`%d entry(ies) carry no reason:

  %s

Every entry accounts for itself (memql#3135): the shared grandfather marker for the seed,
a filed issue number for anything added since. An entry with no reason is how a ratchet
turns into decoration.`, len(f.missingReason), strings.Join(f.missingReason, "\n  "))
	}

	if f.empty() {
		t.Logf("%d pinned construct(s) over %d undeclared concept(s); list and tree agree exactly",
			len(undeclaredRowAuthzConstructs), countUndeclaredConcepts(world))
	}
}

// countUndeclaredConcepts reports how many distinct concepts the pinned
// population spans, for the pass-case log line.
func countUndeclaredConcepts(w undeclaredWorld) int {
	seen := map[string]bool{}
	for _, concept := range undeclaredDebt(w) {
		seen[concept] = true
	}
	return len(seen)
}

// ---- classifier unit tests ----
//
// These run the classifier against a synthetic world rather than the
// tree, so each direction of the gate is proven to bite without anyone
// having to edit .memql files to see it fail.

// worldOf builds a synthetic world. concepts maps a concept id to whether
// it declares a tier; a concept id absent from it does not resolve.
func worldOf(queries map[string]string, concepts map[string]bool) undeclaredWorld {
	w := undeclaredWorld{
		queryConcept:    map[string]string{},
		conceptLoaded:   map[string]bool{},
		conceptDeclares: map[string]bool{},
	}
	for name, concept := range queries {
		w.queryConcept[name] = concept
	}
	for concept, declares := range concepts {
		w.conceptLoaded[concept] = true
		w.conceptDeclares[concept] = declares
	}
	return w
}

func TestUndeclaredGateCatchesNewDebt(t *testing.T) {
	f := classifyUndeclared(
		map[string]undeclaredEntry{"listed": {"v1:x:thing", "seed"}},
		worldOf(
			map[string]string{"listed": "v1:x:thing", "brandNew": "v1:x:thing"},
			map[string]bool{"v1:x:thing": false},
		),
	)
	if len(f.newDebt) != 1 || !strings.Contains(f.newDebt[0], "brandNew") {
		t.Fatalf("a new query over an undeclared concept must be reported as new debt, got %+v", f)
	}
	if len(f.debtPaid) != 0 || len(f.gone) != 0 {
		t.Fatalf("the listed construct is still debt and must produce no staleness finding, got %+v", f)
	}
}

func TestUndeclaredGateCatchesStaleEntries(t *testing.T) {
	f := classifyUndeclared(
		map[string]undeclaredEntry{
			"nowDeclared": {"v1:x:declared", "seed"},
			"deleted":     {"v1:x:thing", "seed"},
			"stillDebt":   {"v1:x:thing", "seed"},
		},
		worldOf(
			map[string]string{"nowDeclared": "v1:x:declared", "stillDebt": "v1:x:thing"},
			map[string]bool{"v1:x:declared": true, "v1:x:thing": false},
		),
	)
	if len(f.debtPaid) != 1 || !strings.Contains(f.debtPaid[0], "nowDeclared") {
		t.Errorf("an entry whose concept now declares a tier must be reported as debt paid, got %+v", f)
	}
	if len(f.gone) != 1 || !strings.Contains(f.gone[0], "deleted") {
		t.Errorf("an entry whose construct is gone must be reported as gone, got %+v", f)
	}
	// The two reasons are reported separately because they are different
	// facts about the tree, which is what lets the failure message name
	// which one applies.
	if strings.Contains(strings.Join(f.gone, " "), "nowDeclared") ||
		strings.Contains(strings.Join(f.debtPaid, " "), "deleted") {
		t.Errorf("the two staleness reasons must not be conflated, got %+v", f)
	}
}

// TestUndeclaredGateDoesNotClassifyMissingConceptAsDebtPaid pins the
// defect described on classifyUndeclared: an unresolvable concept read as
// "debt paid" would invite deleting the whole ratchet in one commit.
func TestUndeclaredGateDoesNotClassifyMissingConceptAsDebtPaid(t *testing.T) {
	pinned := map[string]undeclaredEntry{
		"orphanA": {"v1:x:vanished", "seed"},
		"orphanB": {"v1:x:vanished", "seed"},
	}
	// The constructs still load; their concept does not resolve.
	f := classifyUndeclared(pinned, worldOf(
		map[string]string{"orphanA": "v1:x:vanished", "orphanB": "v1:x:vanished"},
		map[string]bool{"v1:x:other": false},
	))

	if len(f.debtPaid) != 0 {
		t.Fatalf("a MISSING concept classified as debt paid: %v\nTaken at face value that empties "+
			"the ratchet -- the entries would be deleted for a fact that was never established.", f.debtPaid)
	}
	if len(f.gone) != 0 {
		t.Fatalf("the constructs still exist, so 'gone' is also the wrong answer: %v", f.gone)
	}
	if len(f.unresolvable) != 2 {
		t.Fatalf("both entries must be reported as unresolvable, got %+v", f)
	}
	for _, line := range f.unresolvable {
		if !strings.Contains(line, "does not resolve") {
			t.Errorf("the finding must say the concept did not resolve, got %q", line)
		}
	}
}

func TestUndeclaredGateCatchesRebind(t *testing.T) {
	f := classifyUndeclared(
		map[string]undeclaredEntry{"moved": {"v1:x:thing", "seed"}},
		worldOf(
			map[string]string{"moved": "v1:x:other"},
			map[string]bool{"v1:x:thing": false, "v1:x:other": false},
		),
	)
	if len(f.rebound) != 1 || !strings.Contains(f.rebound[0], "v1:x:other") {
		t.Fatalf("a construct re-bound to another concept must be reported, got %+v", f)
	}
	// Reported ONCE, as a re-bind. The construct is listed by name, so it
	// is not unlisted debt; saying both would read as two problems when it
	// is one, and the re-bind line is the one that names what changed.
	if len(f.newDebt) != 0 {
		t.Fatalf("a re-bind is one finding, not two, got %+v", f)
	}
}

func TestUndeclaredGateCatchesMissingReason(t *testing.T) {
	f := classifyUndeclared(
		map[string]undeclaredEntry{
			"noReason":    {"v1:x:thing", ""},
			"blankReason": {"v1:x:thing", "   "},
			"ok":          {"v1:x:thing", undeclaredGrandfatherReason},
		},
		worldOf(
			map[string]string{"noReason": "v1:x:thing", "blankReason": "v1:x:thing", "ok": "v1:x:thing"},
			map[string]bool{"v1:x:thing": false},
		),
	)
	if len(f.missingReason) != 2 {
		t.Fatalf("an entry added without a reason must fail the gate, got %+v", f.missingReason)
	}
	if strings.Contains(strings.Join(f.missingReason, " "), "\"ok\"") {
		t.Fatalf("the entry carrying a reason must not be flagged, got %+v", f.missingReason)
	}
}
