package agents

// goals.go -- the seam that opens a work goal, and the two entry points that
// use it (memql#5048).
//
// `agent(name, prompt)` and the `produceArtifact` TOOL are the two live,
// agent-reachable surfaces that used to mint a `v1:planner:plan`. Neither
// EXECUTED anything -- their own comments said the planner loop picked the
// Plan up off a graph event and ran it. The work spine's unit is a run, so
// each now opens a goal naming the template that does the work, and the run
// dispatcher (memql#5054) executes it.
//
// # Why a seam rather than writing the rows here
//
// `createWorkGoal` and `createWorkRun` are @serverOnly, and
// `auth.OriginFromContext` answers OriginClient for any context nobody
// stamped -- so an unstamped write is REFUSED with one warning nothing above
// it hears. The stamp is allowlisted per PACKAGE to integrations/work, which
// is deliberately a small package with one stamping site; integrations/agents
// is neither. So this declares what it needs and app wiring supplies it,
// exactly as the reactive loop's `responsibilityGoals` seam does.

import (
	"context"
	"strings"
)

// WorkGoals is the narrow surface this package needs from integrations/work.
// *work.Integration satisfies it.
type WorkGoals interface {
	OpenDirectGoal(ctx context.Context, g DirectGoal) (goalId, runId string, err error)
}

// DirectGoal mirrors integrations/work.DirectGoal. It is redeclared rather
// than imported for the dependency reason above: integrations/agents must not
// import integrations/work, or the seam buys nothing.
type DirectGoal struct {
	OwnerUserId    string
	Statement      string
	AutomationName string
	Input          map[string]any
	RequestedVia   string
	TriggeredBy    string
}

// Template names for the two deterministic templates in
// dsl/agents/automations.memql.
//
// Spelled as constants because a typo here is not a compile error: it becomes
// a run that opens, is claimed, and fails as `automation_not_runnable` --
// which reads as a broken goal rather than a misspelled string.
const (
	templateInvokeAgent     = "invokeAgent"
	templateProduceArtifact = "produceArtifact"
)

// SetWorkGoals installs the goal opener. Called once from app wiring.
//
// A nil opener is REFUSED at the call site rather than degraded: without it
// `agent()` and `produceArtifact` have no way to do anything at all, and
// answering an ack for work that was never opened is the failure both of
// these spent their whole lives having.
func (i *Integration) SetWorkGoals(g WorkGoals) {
	if i == nil {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.workGoals == nil {
		i.workGoals = g
	}
}

func (i *Integration) workGoalsRef() WorkGoals {
	if i == nil {
		return nil
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.workGoals
}

// resolveGoalOwner picks the user a goal is opened for.
//
// The chain preserves what the Plan path did: the caller's owner when there is
// one, the AGENT's owner otherwise, and the integration's system actor last --
// which is exactly what `createInvocationPlan` stamped as requestedBy for a
// platform agent dispatched with no user behind the call.
//
// It never returns empty. A goal with a blank owner is legal on the concept
// (a cluster-owned goal), but every read of the run and its observations is
// owner-scoped, so a blank one here would open work that nobody -- including
// the operator looking for it -- can see.
func resolveGoalOwner(callerOwner, agentOwner string) string {
	if o := strings.TrimSpace(callerOwner); o != "" {
		return o
	}
	if o := strings.TrimSpace(agentOwner); o != "" {
		return o
	}
	return systemActorId
}
