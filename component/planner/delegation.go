package planner

import (
	"context"
	"fmt"
	"strings"
)

// delegation.go decides whether a Task runs in-process or is handed
// to a local app on one of the user's own machines (memql#4362).
//
// D6: delegation is a PREFERENCE WITH A FALLBACK, never a
// requirement. The planner asks whether a machine with an allowed,
// signed-in app is online right now; if none is, the task runs
// in-process. A plan never waits for a laptop to wake up, and that is
// the single most important property here -- a delegation design that
// can block is one that turns "my laptop was asleep" into "my plan
// hung", which is far worse than the cost it was trying to save.

// DelegationDecision is the outcome of the triage.
type DelegationDecision struct {
	// Delegate is true when the Task should be created with
	// executionSurface=containerExecutor.
	Delegate bool
	// Backend is the executorBackend name ("cockpit-app:<appId>")
	// when Delegate is true, empty otherwise.
	Backend string
	// WorkerId is the machine the probe found. Advisory: by the time
	// the Task dispatches, selection runs again and may land
	// elsewhere. Recorded so a reader can see WHICH machine made the
	// decision look reasonable.
	WorkerId string
	// Reason is recorded on the Task either way. A task that ran
	// in-process when the user expected delegation is a support
	// question, and "no machine online with claude-code" answers it
	// where an absent field does not.
	Reason string
}

// Delegation reasons. Closed set so the portal can render them
// without parsing prose.
const (
	DelegationReasonDelegated          = "delegated"
	DelegationReasonPolicyOff          = "delegation_not_enabled"
	DelegationReasonKindNotEligible    = "kind_not_eligible"
	DelegationReasonNoAppOrder         = "no_apps_configured"
	DelegationReasonNoMachine          = "no_machine_with_app_online"
	DelegationReasonConcurrencyReached = "max_concurrent_sessions_reached"
	DelegationReasonNoOwner            = "task_has_no_owner"
)

// DelegationPreference is the subset of v1:worker:delegationPolicy
// the decision needs.
type DelegationPreference struct {
	Enabled               bool
	EligibleKinds         []string
	AppOrder              []string
	MaxConcurrentSessions int
}

// DelegationProbe answers the two live questions the decision cannot
// answer from the policy alone.
type DelegationProbe interface {
	// FindMachineForApp returns the registration id of an online
	// machine owned by ownerUserId that has appId allowed and signed
	// in, or "" when none is.
	FindMachineForApp(ctx context.Context, ownerUserId, appId string) string
	// LiveSessionCount returns how many app sessions the user
	// currently has open across every machine.
	LiveSessionCount(ctx context.Context, ownerUserId string) int
}

// DecideDelegation runs the triage.
//
// The ORDER of the checks is what makes the recorded reason useful.
// Cheap, local facts are checked before live probes, so a user who
// simply has delegation switched off gets told that -- rather than
// "no machine online", which would send them to check a laptop that
// was never going to be consulted.
func DecideDelegation(ctx context.Context, ownerUserId, kind string, pref DelegationPreference, probe DelegationProbe) DelegationDecision {
	if strings.TrimSpace(ownerUserId) == "" {
		return DelegationDecision{Reason: DelegationReasonNoOwner}
	}
	if !pref.Enabled {
		return DelegationDecision{Reason: DelegationReasonPolicyOff}
	}
	if !kindEligible(kind, pref.EligibleKinds) {
		return DelegationDecision{Reason: DelegationReasonKindNotEligible}
	}
	if len(pref.AppOrder) == 0 {
		return DelegationDecision{Reason: DelegationReasonNoAppOrder}
	}
	if probe == nil {
		return DelegationDecision{Reason: DelegationReasonNoMachine}
	}

	// The concurrency cap is checked BEFORE machine selection, not
	// after. Selecting first and then refusing would burn a probe and
	// -- worse -- report "no machine online" for a user who has
	// several, which is the wrong thing to go investigate.
	max := pref.MaxConcurrentSessions
	if max <= 0 {
		max = 1
	}
	if probe.LiveSessionCount(ctx, ownerUserId) >= max {
		return DelegationDecision{Reason: DelegationReasonConcurrencyReached}
	}

	// appOrder is an ORDER, not a set: the first app the user listed
	// that is actually available wins. An app absent from the list is
	// never selected even when a machine has it, because the list is
	// how the user says which of their subscriptions they want spent.
	for _, appId := range pref.AppOrder {
		appId = strings.TrimSpace(appId)
		if appId == "" {
			continue
		}
		workerId := probe.FindMachineForApp(ctx, ownerUserId, appId)
		if workerId == "" {
			continue
		}
		return DelegationDecision{
			Delegate: true,
			Backend:  BackendCockpitApp + ":" + appId,
			WorkerId: workerId,
			Reason:   DelegationReasonDelegated,
		}
	}
	return DelegationDecision{Reason: DelegationReasonNoMachine}
}

// BackendCockpitApp mirrors the backend name the agent-side executor
// registers. Declared here as well so the planner can build the
// executorBackend string without importing an agent-tagged package --
// the planner node emits Tasks, the agent node runs them.
const BackendCockpitApp = "cockpit-app"

// ExecutionSurface values on v1:planner:task.
const (
	ExecutionSurfaceInProcess         = "inProcess"
	ExecutionSurfaceContainerExecutor = "containerExecutor"
)

// Surface returns the executionSurface the decision implies.
func (d DelegationDecision) Surface() string {
	if d.Delegate {
		return ExecutionSurfaceContainerExecutor
	}
	return ExecutionSurfaceInProcess
}

// Describe renders the decision for a log line.
func (d DelegationDecision) Describe() string {
	if !d.Delegate {
		return fmt.Sprintf("in-process (%s)", d.Reason)
	}
	return fmt.Sprintf("%s on %s", d.Backend, d.WorkerId)
}

// kindEligible reports whether the policy lists this task kind. An
// EMPTY list allows nothing rather than everything: opting into
// delegation should not silently opt every task kind in with it.
func kindEligible(kind string, eligible []string) bool {
	for _, k := range eligible {
		if strings.EqualFold(strings.TrimSpace(k), strings.TrimSpace(kind)) {
			return true
		}
	}
	return false
}
