package memql

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// rowauthz_undeclared_gate_test.go makes the UNMEASURED population of the
// rowAuthz programme visible and monotonically shrinking (memql#3077).
//
// # The hole this closes
//
// Phase 3 SHADOW-COMPUTES the declared tier -- it injects no predicate and
// changes no query result yet, which TestRowAuthzIsInert pins. So this gate
// tracks the population that has DECLARED a tier, not the population that is
// enforced. Measured on this tree, declared is a minority of the query surface;
// every other query is bound to a concept that declares no `@rowAuthz` tier at
// all. Those constructs are NOT "safe" and NOT
// "unchanged" -- they are UNMEASURED, and TestRowAuthzShadowReport says so in
// exactly those words:
//
//	--- NOT MEASURABLE: concept declares no tier (N constructs) ---
//	  Shadow mode computes a predicate only where a tier is declared.
//	  These are not 'no change' -- they are 'not measured'.
//
// The coverage is inverted against risk: the largest undeclared namespace is
// `v1:identity:`, which is the credential surface -- authSessionByRefreshTokenHash,
// badgesForUser, agentAuthorizationsForUser, auditEventsByActor and friends.
//
// An enforced tier over a minority of the tree, with the majority invisible and
// no pressure on the number, is worse than no tier at all -- for the reason
// rowauthz_owner_gate_test.go states about its own failure mode: "the
// declaration records a guarantee nothing provides -- and it reads as safe, so
// an auditor seeing the tier stops looking."
//
// Before this gate the only signal was a boot-time warning. A warning that has
// been true for 168 constructs for months is not a signal, it is wallpaper.
//
// # THE LIST MAY ONLY SHRINK
//
// That is the whole mechanism, and it is the same idiom as `ownerGateExemptions`
// in rowauthz_owner_gate_test.go and TestRowAuthzIsInert's allow-list:
//
//   - A query over an undeclared concept that is NOT on the list fails the
//     build. The author declares a tier on the concept, or adds the construct
//     with an issue number recording the decision.
//   - An entry that no longer applies fails the build and must be DELETED.
//     A stale entry is as bad as a missing gate: it names a construct that may
//     no longer exist and suppresses it forever.
//
// The debt becomes a named, decreasing number instead of a warning nobody
// reads. Nothing here asserts the SIZE of the list -- only its membership --
// so shrinking it never requires editing a magic number, and growing it always
// requires an explicit edit.
//
// # What this gate deliberately does NOT do
//
// It declares a tier on nothing. Each of the undeclared identity constructs is
// a real authorization judgment -- `activeUsers` and `badgesForUser` are not
// obviously owner-scoped, and several are legitimately cross-user admin reads.
// Making those calls is downstream work this gate makes visible and
// sequenceable; doing it here would bury dozens of security decisions inside a
// tooling change. Related and deliberately separate: memql#3029 (the
// `owner="id"` self-owned form, which several identity concepts are blocked
// on), memql#2991 (the update/delete counterpart), memql#3059 (raw `insert()`
// forgeability on the write path).

// TestRowAuthzUndeclaredPopulationIsGated is the shrink-only gate.
//
// It enumerates from the LOADED REGISTRY -- the same walk TestRowAuthzShadowReport
// uses to produce its "NOT MEASURABLE" section -- rather than from a
// hand-maintained duplicate of the tree, so the two can never disagree about
// what is undeclared.
func TestRowAuthzUndeclaredPopulationIsGated(t *testing.T) {
	// WithSkips, not the plain wrapper: LoadUnifiedConcepts discards the skip
	// list (unified_loader.go:56-58), and a dropped concept takes every query
	// bound to it out of `current` -- where this gate reports them as "debt
	// paid" and tells the author to DELETE the entries. Measured: dropping one
	// concept made two listed queries render as debt paid, and dropping them
	// from the list too made the gate PASS with two unlisted queries over an
	// undeclared concept. Same reasoning as rowauthz_owner_gate_test.go:61-72.
	_, conceptSkips, err := LoadUnifiedConceptsWithSkips(nil)
	if err != nil {
		t.Fatalf("LoadUnifiedConceptsWithSkips: %v", err)
	}
	if len(conceptSkips) > 0 {
		t.Fatalf("%d concept(s) were skipped at load, so every query bound to them is invisible "+
			"to this gate and would be reported as debt paid:\n  %v", len(conceptSkips), conceptSkips)
	}
	registry := newFunctionRegistry()
	report := newLoadReport()
	if _, _, err := LoadUnifiedFunctions(nil, registry, memoryNodes.DefaultRegistry(), report); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if len(report.Skipped) > 0 {
		t.Fatalf("%d construct(s) were skipped at load, so this gate cannot see them and would "+
			"report a PASS on a query that simply failed to parse:\n  %v",
			len(report.Skipped), report.Skipped)
	}

	// current: construct name -> bound concept, for every query whose concept
	// declares no tier.
	current := map[string]string{}
	totalQueries := 0
	var unresolvable []string
	for _, fn := range registry.List() {
		if fn == nil || fn.FunctionKind != "query" {
			continue
		}
		totalQueries++
		if strings.TrimSpace(fn.BoundConcept) == "" {
			continue // unbound: not a rowAuthz question
		}
		concept, cErr := memoryNodes.Get(fn.BoundConcept)
		if cErr != nil || concept == nil {
			// Recorded, not skipped. A silent continue here drops the construct
			// from `current`, and the stale classifier below then renders it as
			// "debt paid -- DELETE it". Measured: forcing every Get to fail made
			// the gate report all 168 entries as debt paid and instruct the
			// author to empty the ratchet in one commit.
			unresolvable = append(unresolvable, fmt.Sprintf("%s (bound to %s)", fn.Name, fn.BoundConcept))
			continue
		}
		if concept.RowAuthz == nil {
			current[fn.Name] = fn.BoundConcept
		}
	}

	// A sweep that resolves nothing passes vacuously, and a vacuous pass on a
	// security gate is the failure this whole programme is about. Guards the
	// SWEEP, not the debt -- the debt size is deliberately unasserted.
	if totalQueries < 50 {
		t.Fatalf("only %d query construct(s) found in the registry -- the tree failed to load "+
			"and this gate would pass for the wrong reason", totalQueries)
	}
	if len(unresolvable) > 0 {
		sort.Strings(unresolvable)
		t.Fatalf(`%d query construct(s) name a concept that does not resolve:

  %s

This is a load failure, not progress. Every one of them is invisible to the debt accounting
below, and without this guard the stale check would report them as "debt paid" and tell you to
delete their entries.`, len(unresolvable), strings.Join(unresolvable, "\n  "))
	}

	// 1. Anything undeclared and unlisted is NEW debt. Fail.
	var added []string
	for name, concept := range current {
		if _, listed := undeclaredRowAuthzConstructs[name]; !listed {
			added = append(added, fmt.Sprintf("%s (%s)", name, concept))
		}
	}
	if len(added) > 0 {
		sort.Strings(added)
		t.Errorf(`%d query construct(s) are bound to a concept that declares no @rowAuthz tier,
and are not on the grandfathered list:

  %s

A query over an undeclared concept is not "safe" and not "unchanged" -- it is UNMEASURED.
Shadow mode computes a predicate only where a tier is declared, so nothing in the rowAuthz
programme can say anything about this construct at all.

Two ways forward, and they are different decisions:
  - Declare the tier on the concept: @rowAuthz(owner="<field>") / (via="<spec>") /
    (clusterOwner) / (public). This is the one that reduces the debt, and it is usually right
    for a new concept where you already know who owns a row. The order matters: (public) is
    listed LAST because it is the cheapest way to go green and the only one that asserts no
    restriction -- on a credential lookup it is the wrong answer, and it retires the debt
    permanently rather than shrinking it.
  - If the tier is a real authorization question you are not answering right now -- several of
    the existing entries are genuinely cross-user admin reads -- add the construct to
    undeclaredRowAuthzConstructs, and record why in the commit or the linked issue. Nothing
    here can enforce that, and none of the seeded entries carry one, so it is a convention a
    reviewer has to hold, not something a green run proves.

Do not add an entry just to go green. The list may only shrink; every entry is a debt someone
has to pay, and one added without a filed decision is a debt with no owner.`,
			len(added), strings.Join(added, "\n  "))
	}

	// 2. Anything listed that no longer applies is STALE. Fail, and say which of
	//    the three reasons applies -- re-bound, gone, or debt paid. "stale"
	//    alone leaves the author guessing whether they fixed it or deleted it.
	//    A concept that failed to RESOLVE is not one of the three; it is caught
	//    above, because falling through to "debt paid" is the reassuring answer
	//    and the wrong one.
	var stale []string
	for name, listedConcept := range undeclaredRowAuthzConstructs {
		nowConcept, stillUndeclared := current[name]
		if stillUndeclared {
			if nowConcept != listedConcept {
				stale = append(stale, fmt.Sprintf(
					"%s (listed against %s, now bound to %s -- update the entry)",
					name, listedConcept, nowConcept))
			}
			continue
		}
		if _, getErr := registry.Get(name); getErr != nil {
			stale = append(stale, fmt.Sprintf("%s (the construct no longer exists)", name))
			continue
		}
		stale = append(stale, fmt.Sprintf(
			"%s (%s now declares a tier -- debt paid)", name, listedConcept))
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf(`%d entr(y/ies) in undeclaredRowAuthzConstructs no longer apply:

  %s

DELETE them. This list may only shrink -- that is the entire point of it. A stale entry is as
bad as a missing gate: it names a construct that may no longer exist, and it suppresses that
construct forever if it ever comes back.

If an entry says "debt paid", that is the good case: a concept gained a tier and the gate is
telling you to bank the progress.`,
			len(stale), strings.Join(stale, "\n  "))
	}
}

// undeclaredRowAuthzConstructs is the grandfathered debt: every query construct
// whose bound concept declares no @rowAuthz tier as of memql#3077.
//
// SHRINK-ONLY. Read the file header before adding to it. An entry added without
// a filed decision is a debt with no owner, and the gate cannot tell the two
// apart -- only a reviewer can.
//
// Keyed by construct name, valued by the bound concept, so the gate can also
// catch a construct that stays undeclared while being re-bound to a DIFFERENT
// concept -- which is a change of authorization surface wearing the same name.
//
// Grouped by concept purely for readability. The grouping carries no meaning to
// the gate; membership is the only thing asserted.
var undeclaredRowAuthzConstructs = map[string]string{

	// v1:agents:agent
	"activeAgents":          "v1:agents:agent",
	"activeAgentsForUser":   "v1:agents:agent",
	"agentById":             "v1:agents:agent",
	"agentOwner":            "v1:agents:agent",
	"agentRoleSlugsInUse":   "v1:agents:agent",
	"allAgents":             "v1:agents:agent",
	"assistantAgentForUser": "v1:agents:agent",

	// v1:agents:agentAuthorization
	"agentAuthorizationsForUser": "v1:agents:agentAuthorization",

	// v1:agents:agentRole
	"activeAgentRoles": "v1:agents:agentRole",
	"agentRoleBySlug":  "v1:agents:agentRole",

	// v1:agents:avatarPersona
	"avatarPersonaById": "v1:agents:avatarPersona",
	"avatarPersonas":    "v1:agents:avatarPersona",

	// v1:agents:skill
	"activeSkills":      "v1:agents:skill",
	"activeSkillsFull":  "v1:agents:skill",
	"skillBySlug":       "v1:agents:skill",
	"skillNeedsRefresh": "v1:agents:skill",

	// v1:agents:skillChangeEvent
	"skillChangeEventsForAgent": "v1:agents:skillChangeEvent",

	// v1:authoring:bundle
	"activeAuthoringBundles":           "v1:authoring:bundle",
	"authoringBundleById":              "v1:authoring:bundle",
	"authoringBundleForPlan":           "v1:authoring:bundle",
	"authoringBundleForResponsibility": "v1:authoring:bundle",
	"authoringBundlesForOwner":         "v1:authoring:bundle",
	"systemActiveAuthoringBundles":     "v1:authoring:bundle",

	// v1:cluster:cluster
	"existingCluster": "v1:cluster:cluster",

	// v1:cluster:deployment
	"deploymentById":        "v1:cluster:deployment",
	"deploymentsForCluster": "v1:cluster:deployment",
	"supersededDeployments": "v1:cluster:deployment",

	// v1:cluster:deploymentNodeSpec
	"nodeSpecsForDeployment": "v1:cluster:deploymentNodeSpec",

	// v1:cluster:node
	"clusterNodes":         "v1:cluster:node",
	"nodesForDeployment":   "v1:cluster:node",
	"nodesNotInDeployment": "v1:cluster:node",
	"staleClusterNodes":    "v1:cluster:node",

	// v1:cluster:nodeType
	"clusterNodeTypes": "v1:cluster:nodeType",

	// v1:cluster:spawnEvent
	"clusterSpawnEvents": "v1:cluster:spawnEvent",

	// v1:cognition:audioOverride
	"audioOverridesForSpace": "v1:cognition:audioOverride",

	// v1:cognition:participant
	"activeHumanParticipants": "v1:cognition:participant",
	"groupGAForSpace":         "v1:cognition:participant",
	"participantByAgentSpace": "v1:cognition:participant",
	"siParticipantForSpace":   "v1:cognition:participant",
	"spaceParticipants":       "v1:cognition:participant",

	// v1:cognition:participant:presence
	"spaceParticipantPresence": "v1:cognition:participant:presence",

	// v1:cognition:session
	"participantSession": "v1:cognition:session",

	// v1:cognition:utterance
	"agentInteractionCount":       "v1:cognition:utterance",
	"feedbackAnnouncementForPlan": "v1:cognition:utterance",
	"greetingUtterance":           "v1:cognition:utterance",
	"hasAIResponseForReply":       "v1:cognition:utterance",
	"spaceUtterances":             "v1:cognition:utterance",

	// v1:cognition:videoOverride
	"videoOverridesForSpace": "v1:cognition:videoOverride",

	// v1:common:attachment
	"attachmentById": "v1:common:attachment",

	// v1:common:media
	"spaceMedia": "v1:common:media",

	// v1:data:log
	"validationLog": "v1:data:log",

	// v1:data:policy
	"policy": "v1:data:policy",

	// v1:data:record
	"detectConflicts": "v1:data:record",
	"recordsByState":  "v1:data:record",
	"usableRecords":   "v1:data:record",

	// v1:forge:project
	"activeProjects": "v1:forge:project",
	"projectById":    "v1:forge:project",
	"projectBySlug":  "v1:forge:project",

	// v1:forge:request
	"approvalQueue":   "v1:forge:request",
	"myRequests":      "v1:forge:request",
	"projectRequests": "v1:forge:request",
	"requestById":     "v1:forge:request",
	"validationQueue": "v1:forge:request",

	// v1:forge:requestEvent
	"requestEvents": "v1:forge:requestEvent",

	// v1:identity:accessRequest
	"accessRequestById":            "v1:identity:accessRequest",
	"expiredPendingAccessRequests": "v1:identity:accessRequest",
	"pendingAccessRequests":        "v1:identity:accessRequest",

	// v1:identity:accountEntitlement
	"accountEntitlement": "v1:identity:accountEntitlement",

	// v1:identity:auditEvent
	"auditEventsByActor":  "v1:identity:auditEvent",
	"auditEventsByTarget": "v1:identity:auditEvent",
	"expiredAuditEvents":  "v1:identity:auditEvent",
	"recentAuditEvents":   "v1:identity:auditEvent",

	// v1:identity:authCode
	"authCodeByCodeHash":       "v1:identity:authCode",
	"expiredConsumedAuthCodes": "v1:identity:authCode",

	// v1:identity:authSession
	"authSessionByPreviousRefreshTokenHash": "v1:identity:authSession",
	"authSessionByRefreshTokenHash":         "v1:identity:authSession",
	"authSessionByTokenHash":                "v1:identity:authSession",
	"authSessionsForSubject":                "v1:identity:authSession",

	// v1:identity:clusterSettings
	"clusterSettingsCurrent": "v1:identity:clusterSettings",

	// v1:identity:delegation
	"activeDelegationsByIdentitySubject": "v1:identity:delegation",
	"activeDelegationsForAgent":          "v1:identity:delegation",
	"delegationsByIdentity":              "v1:identity:delegation",
	"expiredActiveDelegations":           "v1:identity:delegation",

	// v1:identity:identity
	"badgeByKeyHash":             "v1:identity:identity",
	"badgesForUser":              "v1:identity:identity",
	"nodeTokenIdentities":        "v1:identity:identity",
	"nodeTokenIdentityByBinding": "v1:identity:identity",
	"nodeTokenIdentityById":      "v1:identity:identity",
	"patIdentitiesForUser":       "v1:identity:identity",
	"patIdentityById":            "v1:identity:identity",
	"patIdentityByKeyHash":       "v1:identity:identity",
	"workerTokenByKeyHash":       "v1:identity:identity",
	"workerTokensForUser":        "v1:identity:identity",

	// v1:identity:invitation
	"invitationById":                "v1:identity:invitation",
	"invitationByPreviousTokenHash": "v1:identity:invitation",
	"invitationByTokenHash":         "v1:identity:invitation",

	// v1:identity:magicLinkRequest
	"expiredMagicLinkRequests":    "v1:identity:magicLinkRequest",
	"magicLinkRequestByTokenHash": "v1:identity:magicLinkRequest",

	// v1:identity:oauthClient
	"oAuthClientByClientId": "v1:identity:oauthClient",

	// v1:identity:user
	"activeUsers":               "v1:identity:user",
	"currentUser":               "v1:identity:user",
	"searchUsers":               "v1:identity:user",
	"userActiveSpace":           "v1:identity:user",
	"userByEmail":               "v1:identity:user",
	"userById":                  "v1:identity:user",
	"userByIdSystem":            "v1:identity:user",
	"userCount":                 "v1:identity:user",
	"userDisplayById":           "v1:identity:user",
	"usersActiveInSpace":        "v1:identity:user",
	"usersInDeletionCooldown":   "v1:identity:user",
	"usersScheduledForDeletion": "v1:identity:user",

	// v1:identity:workerPairingCode
	"workerPairingCodeByHash": "v1:identity:workerPairingCode",

	// v1:knowledge:documentChunk
	"allDocumentChunkDomains": "v1:knowledge:documentChunk",
	"documentChunksForDomain": "v1:knowledge:documentChunk",

	// v1:library:artifact
	"libraryArtifactById":         "v1:library:artifact",
	"libraryArtifacts":            "v1:library:artifact",
	"libraryArtifactsByKind":      "v1:library:artifact",
	"libraryArtifactsByLens":      "v1:library:artifact",
	"libraryWorkspaceLiveSources": "v1:library:artifact",

	// v1:planner:plan
	"activePlansForUser":               "v1:planner:plan",
	"allPlans":                         "v1:planner:plan",
	"awaitingFeedbackPlansPastTimeout": "v1:planner:plan",
	"historicalPlanMetrics":            "v1:planner:plan",
	"planById":                         "v1:planner:plan",
	"plansForResponsibility":           "v1:planner:plan",
	"plansForSpace":                    "v1:planner:plan",
	"runningPlansForUser":              "v1:planner:plan",
	"strandedCandidatePlans":           "v1:planner:plan",
	"waitingPlansForUser":              "v1:planner:plan",

	// v1:planner:responsibility
	"activeResponsibilities":            "v1:planner:responsibility",
	"activeResponsibilitiesAcrossUsers": "v1:planner:responsibility",
	"dueResponsibilities":               "v1:planner:responsibility",
	"responsibilitiesForUser":           "v1:planner:responsibility",
	"responsibilityById":                "v1:planner:responsibility",

	// v1:planner:task
	"tasksForPlan": "v1:planner:task",

	// v1:planner:taskState
	"taskStateById": "v1:planner:taskState",

	// v1:platform:globalVariable
	"globalVariable":  "v1:platform:globalVariable",
	"globalVariables": "v1:platform:globalVariable",
	"userDefaults":    "v1:platform:globalVariable",

	// v1:platform:inboundRequest
	"inboundRequestByDedupeKey": "v1:platform:inboundRequest",
	"inboundRequestsByStatus":   "v1:platform:inboundRequest",

	// v1:platform:missingCapability
	"missingCapabilitiesByStatus":    "v1:platform:missingCapability",
	"missingCapabilityByKindAndName": "v1:platform:missingCapability",

	// v1:platform:outboundRequest
	"outboundRequestsByStatus": "v1:platform:outboundRequest",

	// v1:platform:policyTrace
	"allPolicyTraces":       "v1:platform:policyTrace",
	"policyTracesForPolicy": "v1:platform:policyTrace",

	// v1:rbac:capability
	"activeCapabilities":          "v1:rbac:capability",
	"capabilitiesForResourceType": "v1:rbac:capability",
	"capabilitiesForRole":         "v1:rbac:capability",
	"capabilityGrant":             "v1:rbac:capability",

	// v1:rbac:role
	"activeRoles": "v1:rbac:role",
	"roleBySlug":  "v1:rbac:role",

	// v1:router:budget
	"routerBudgets": "v1:router:budget",

	// v1:safety:approvalRequest
	"activeApprovalsByCorrelationKey": "v1:safety:approvalRequest",
	"approvalRequestById":             "v1:safety:approvalRequest",

	// v1:safety:classification
	"allSafetyClassifications": "v1:safety:classification",

	// v1:safety:outputScreening
	"allOutputScreenings": "v1:safety:outputScreening",

	// v1:telephony:consent
	"consentOptOut": "v1:telephony:consent",

	// v1:telephony:number
	"allNumbers":         "v1:telephony:number",
	"numberByE164":       "v1:telephony:number",
	"numbersByPartition": "v1:telephony:number",

	// v1:workbench:workspace
	"provisionedWorkspaces": "v1:workbench:workspace",
	"workspaceForPlan":      "v1:workbench:workspace",

	// v1:worker:invocation
	"expiredWorkerInvocations": "v1:worker:invocation",
	"invocationsForPlan":       "v1:worker:invocation",
	"invocationsForUser":       "v1:worker:invocation",

	// v1:worker:registration
	"workerByIdentityId": "v1:worker:registration",
	"workersForUser":     "v1:worker:registration",
}
