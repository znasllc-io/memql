// Package harness holds the MemQL-native agent harness control plane
// (epic #590). This file is the reconciler's PURE decision core: step
// selection over the DAG, budget-tripped detection, and plan terminal-
// state computation. Everything here is deliberately free of DB, event
// bus, and engine receivers so the controller's logic is unit-testable
// in isolation (the same discipline #582's stepTransitionAllowed and
// #584's inner_loop helpers follow).
//
// The impure half -- subscriptions, claim-before-run conditional writes,
// dispatch to the inner loop, observation/mutation writes -- lives in
// reconciler.go and leans on these helpers for every decision.
package harness

// Step status enum values for v1:harness:step. Mirrors the engine-side
// constants in component/memql/harness_step_validation.go (#582); kept
// in lockstep so the reconciler reasons about the same state machine the
// engine guard enforces.
const (
	StepStatusPending = "pending"
	StepStatusReady   = "ready"
	StepStatusRunning = "running"
	StepStatusBlocked = "blocked"
	StepStatusDone    = "done"
	StepStatusFailed  = "failed"
)

// Plan status enum values for v1:harness:plan (#582).
const (
	PlanStatusOpen      = "open"
	PlanStatusRunning   = "running"
	PlanStatusDone      = "done"
	PlanStatusFailed    = "failed"
	PlanStatusCancelled = "cancelled"
)

// StepView is the minimal projection of a v1:harness:step the reconciler
// reasons about. Loaded from the graph (latest version per id) by the
// controller, then fed to these pure helpers. Kept tiny so tests can
// build a DAG by hand without a database.
type StepView struct {
	ID        string
	PlanID    string
	Status    string
	DependsOn []string
	Attempt   int
	// IdempotencyKey threads through to the inner loop so a re-dispatched
	// step does not repeat an external side effect (#584 hook).
	IdempotencyKey string
	// OwnerUserId is carried forward on re-version writes (raw insert
	// re-versions don't read-merge, so the controller must re-assert the
	// owner to preserve the owned-tier authz key).
	OwnerUserId string
	// Title is likewise re-asserted on re-version writes.
	Title string
	// Input is the per-step input payload fed to the inner loop.
	Input map[string]any
}

// PlanView is the minimal plan projection the controller re-asserts on a
// terminal-status re-version write (raw insert doesn't read-merge, so the
// owned-tier fields must be carried forward).
type PlanView struct {
	ID          string
	OwnerUserId string
	Goal        string
	Status      string
	RootStepId  string
}

// PlanBudget bounds a single plan's execution. The controller enforces
// these and records a `decision` observation when one trips. A non-
// positive value disables that particular limit.
type PlanBudget struct {
	// MaxSteps caps the total number of step dispatches the controller
	// will perform for this plan across its whole life. Guards against a
	// runaway DAG (e.g. a retry storm or a self-extending plan).
	MaxSteps int
	// MaxToolCalls caps the cumulative tool-call count across every step
	// dispatched for this plan.
	MaxToolCalls int
	// MaxWallClockMillis caps the wall-clock time from the plan's first
	// dispatch. Stored as millis so the type stays dependency-free
	// (the controller converts to time.Duration at the boundary).
	MaxWallClockMillis int64
}

// DefaultPlanBudget returns conservative per-plan guardrails. The
// controller uses these when a plan carries no explicit budget.
func DefaultPlanBudget() PlanBudget {
	return PlanBudget{
		MaxSteps:           50,
		MaxToolCalls:       500,
		MaxWallClockMillis: 10 * 60 * 1000, // 10 minutes
	}
}

// BudgetUsage is the running tally the controller maintains per plan
// (derived from the graph -- counted observations / dispatched steps /
// elapsed wall clock -- NOT held as authoritative in-memory state).
type BudgetUsage struct {
	StepsDispatched int
	ToolCalls       int
	ElapsedMillis   int64
}

// BudgetVerdict reports whether a budget tripped and which limit. Empty
// Reason means within budget.
type BudgetVerdict struct {
	Tripped bool
	Reason  string
}

// BudgetTripped reports whether the running usage has crossed any
// configured limit. Pure arithmetic so the guardrail is unit-testable
// without wiring a clock or a DB. The first limit checked that is both
// configured (positive) and exceeded wins, giving a stable Reason.
func BudgetTripped(b PlanBudget, u BudgetUsage) BudgetVerdict {
	if b.MaxSteps > 0 && u.StepsDispatched >= b.MaxSteps {
		return BudgetVerdict{Tripped: true, Reason: "max_steps"}
	}
	if b.MaxToolCalls > 0 && u.ToolCalls >= b.MaxToolCalls {
		return BudgetVerdict{Tripped: true, Reason: "max_tool_calls"}
	}
	if b.MaxWallClockMillis > 0 && u.ElapsedMillis >= b.MaxWallClockMillis {
		return BudgetVerdict{Tripped: true, Reason: "max_wall_clock"}
	}
	return BudgetVerdict{}
}

// dependenciesSatisfied reports whether every id in dependsOn is a step
// whose latest status is done. An unknown dependency id (not present in
// byID) is treated as NOT satisfied -- a dangling edge keeps the step
// pending rather than silently letting it run.
func dependenciesSatisfied(dependsOn []string, byID map[string]StepView) bool {
	for _, dep := range dependsOn {
		d, ok := byID[dep]
		if !ok || d.Status != StepStatusDone {
			return false
		}
	}
	return true
}

// dependencyDeadEnd reports whether a pending step can never become ready
// because one of its dependencies is in a terminal-failure state (failed)
// or is a dangling reference that no step will ever satisfy. A dependency
// that is still in flight (pending/ready/running/blocked) is NOT a dead
// end -- the plan can still progress.
func dependencyDeadEnd(dependsOn []string, byID map[string]StepView) bool {
	for _, dep := range dependsOn {
		d, ok := byID[dep]
		if !ok {
			return true // dangling edge: never satisfiable
		}
		if d.Status == StepStatusFailed {
			return true
		}
	}
	return false
}

// SelectRunnable returns the steps that are immediately runnable for a
// plan: status==ready. Crucially this is a snapshot read -- the
// controller still CLAIMS each (atomic ready->running flip) before
// dispatch, so two concurrent reconciles selecting the same step do not
// both execute it (only one claim wins). The returned slice preserves
// the input order so dispatch is deterministic for tests.
func SelectRunnable(steps []StepView) []StepView {
	out := make([]StepView, 0, len(steps))
	for _, s := range steps {
		if s.Status == StepStatusReady {
			out = append(out, s)
		}
	}
	return out
}

// PromotablePending returns the steps that should transition pending ->
// ready (or blocked -> ready) because their dependencies are now all
// done. This is what lets a blocked step become ready and run once its
// blocker completes (acceptance criterion). The controller flips each
// returned step to ready via mutationAddHarnessStep's state machine
// (pending->ready / blocked->ready are legal edges per #582).
func PromotablePending(steps []StepView) []StepView {
	byID := indexByID(steps)
	out := make([]StepView, 0, len(steps))
	for _, s := range steps {
		if s.Status != StepStatusPending && s.Status != StepStatusBlocked {
			continue
		}
		if dependenciesSatisfied(s.DependsOn, byID) {
			out = append(out, s)
		}
	}
	return out
}

// indexByID builds an id->step lookup over the latest version of each
// step. Callers pass the already-deduped latest-per-id slice.
func indexByID(steps []StepView) map[string]StepView {
	byID := make(map[string]StepView, len(steps))
	for _, s := range steps {
		byID[s.ID] = s
	}
	return byID
}

// PlanTerminalVerdict reports the terminal plan status implied by the
// current step set, or empty when the plan is not yet terminal.
type PlanTerminalVerdict struct {
	// Status is one of PlanStatusDone / PlanStatusFailed, or "" when the
	// plan is still in flight.
	Status string
}

// ComputePlanTerminal decides whether a plan has reached a terminal
// state from its step statuses:
//
//   - all steps done            -> plan done
//   - no step can ever run again
//     (every step is done/failed/blocked AND at least one is
//     failed/blocked) -> plan failed
//   - otherwise                 -> still in flight (empty verdict)
//
// "Blocked with no path forward" counts as failure: a blocked step whose
// blocker can never complete (its blocker failed) is a dead end. A blocked
// (or pending) step whose dependencies ARE now all done is NOT stuck --
// it is promotable next tick, so it counts as in-flight here. This keeps a
// plan from being declared failed in the same tick its blocker completes,
// before the promotion lands.
//
// An empty step set is NOT terminal -- a freshly-created plan with no
// steps yet stays open until its steps land.
func ComputePlanTerminal(steps []StepView) PlanTerminalVerdict {
	if len(steps) == 0 {
		return PlanTerminalVerdict{}
	}
	byID := indexByID(steps)
	allDone := true
	anyFailedOrBlocked := false
	anyInFlight := false
	for _, s := range steps {
		switch s.Status {
		case StepStatusDone:
			// terminal-success; does not block plan completion.
		case StepStatusPending:
			allDone = false
			// A pending step is always advancing unless its deps can never
			// satisfy. With deps now done it promotes next tick; without
			// deps it is immediately promotable. Either way it is in-flight
			// as long as no dependency is in a terminal-failure state.
			if dependencyDeadEnd(s.DependsOn, byID) {
				anyFailedOrBlocked = true
			} else {
				anyInFlight = true
			}
		case StepStatusBlocked:
			allDone = false
			// A blocked step only advances when it HAS dependencies and they
			// are all done (the blocked->ready edge). No deps, unsatisfied
			// deps, or a dead-end dependency means genuinely stuck.
			if len(s.DependsOn) > 0 && dependenciesSatisfied(s.DependsOn, byID) {
				anyInFlight = true
			} else {
				anyFailedOrBlocked = true
			}
		case StepStatusFailed:
			allDone = false
			anyFailedOrBlocked = true
		default:
			// ready / running -- the plan can still progress.
			allDone = false
			anyInFlight = true
		}
	}
	if allDone {
		return PlanTerminalVerdict{Status: PlanStatusDone}
	}
	// If anything can still run, the plan is not terminal yet.
	if anyInFlight {
		return PlanTerminalVerdict{}
	}
	// Nothing in flight and not all done: the remainder is failed/blocked
	// with no path forward.
	if anyFailedOrBlocked {
		return PlanTerminalVerdict{Status: PlanStatusFailed}
	}
	return PlanTerminalVerdict{}
}
