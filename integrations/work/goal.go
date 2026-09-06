package work

import (
	"context"
	"fmt"
	"os"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// goal.go -- createGoal and cancelGoal (design record section H).

const (
	goalConcept = "v1:work:goal"
	runConcept  = "v1:work:run"

	// runStatusCompiling and friends are v1:work:run.status. Spelled here
	// rather than imported from component/work because they are the
	// CONCEPT's enum, not a decision -- the pure package has no opinion
	// about them.
	runStatusCompiling = "compiling"
	runStatusRunning   = "running"
	runStatusWaiting   = "waiting"
	runStatusFailed    = "failed"
	runStatusAbandoned = "abandoned"

	// terminalStatuses is what "live" is the complement of.
	runStatusSucceeded = "succeeded"
	runStatusCancelled = "cancelled"

	modeLive   = "live"
	modeReplay = "replay"
	modeFork   = "fork"

	// compilingAutomationName is the run's automationName BEFORE compile has
	// chosen a template.
	//
	// It is a SENTINEL rather than an empty string, and the difference is
	// legibility: automationName is what the run rail shows and what
	// workRunsForAutomation groups by, so an empty one renders as a run of
	// nothing and sorts in with every other run that has no name. This says
	// what is true -- the template is not chosen yet.
	//
	// It is a KNOWN RESIDUAL that nothing can replace it once compile
	// decides: updateWorkRun does not accept automationName. See the gaps
	// note at the bottom of this file.
	compilingAutomationName = "work.compile"
)

// isTerminalRunStatus reports whether a run has stopped for good.
func isTerminalRunStatus(s string) bool {
	switch s {
	case runStatusSucceeded, runStatusFailed, runStatusCancelled, runStatusAbandoned:
		return true
	}
	return false
}

// handleCreateGoal opens a goal, opens its first run in `compiling`, and
// dispatches compile.
//
// THE ORDER OF THE THREE WRITES IS NOT INTERCHANGEABLE. The run is created
// the moment the goal is accepted -- in `compiling`, before any template is
// known -- so that the model calls COMPILATION ITSELF makes have a home from
// the first one (design section B, "run"). A run opened after compile would
// leave those calls unjournaled and uncounted, which is precisely the spend
// the ceilings exist to bound.
func (i *Integration) handleCreateGoal(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	statement := argString(args, "statement")
	if statement == "" {
		return nil, fmt.Errorf("work: createGoal needs a statement")
	}
	requestedVia := argString(args, "requestedVia")
	if err := validRequestedVia(requestedVia); err != nil {
		return nil, err
	}

	st := i.store()
	now := i.clock().UTC()
	goalId := newRowId(goalConcept)
	runId := newRowId(runConcept)
	owner := strings.TrimSpace(ac.UserId)
	input := argMap(args, "input")
	ceilings := argMap(args, "ceilings")

	// The goal is written under the CALLER's own actor, unchanged.
	// createWorkGoal stamps ownerUserId from actor.userId (the field is
	// @serverSet), so the owner reaches the row through the actor and
	// through nothing else -- there is no argument to forge.
	if err := st.createGoalRow(ctx, goalSeed{
		GoalId:       goalId,
		Statement:    statement,
		Origin:       "user",
		AccountIds:   argStrings(args, "accountIds"),
		Input:        input,
		Ceilings:     ceilings,
		RequestedVia: requestedVia,
	}); err != nil {
		return nil, err
	}

	// The run is written under the goal owner's BORROWED authority. On this
	// path the owner IS the caller, so the stamp changes nothing today --
	// and it is here anyway, because the value is read off the row the
	// caller just wrote under their own actor and every other run-opening
	// path in this package (fork, replay, and the compiler when it lands)
	// reaches this function's shape from a place where they differ. Making
	// the borrow the rule rather than the exception is what stops the one
	// path that needs it being the one path that forgets.
	runCtx := ownerActor(ctx, owner)
	if err := st.createRunRow(runCtx, runSeed{
		RunId:          runId,
		GoalId:         goalId,
		AutomationName: compilingAutomationName,
		Input:          input,
		TriggeredBy:    "manual",
		Mode:           modeLive,
		Status:         runStatusCompiling,
		NodeId:         selfNodeId(),
		StartedAt:      now,
		OwnerUserId:    owner,
	}); err != nil {
		return nil, err
	}

	// The RUN rides the context from here (memql#4999), so every model call
	// the compile pass makes is journaled against this run. context.WithoutCancel
	// inside dispatchCompile preserves values, so the stamp survives onto the
	// detached goroutine along with the actor and the budget scopes.
	//
	// Mode is live: this run reads no journal. It WRITES one, which is what
	// makes a later replay of it possible at all.
	ctx = common.ContextWithRun(ctx, common.RunContext{
		RunId:       runId,
		GoalId:      goalId,
		Mode:        common.RunModeLive,
		OwnerUserId: owner,
	})

	dispatched := i.dispatchCompile(ctx, CompileRequest{
		GoalId:      goalId,
		RunId:       runId,
		OwnerUserId: owner,
		Statement:   statement,
		Input:       input,
		Ceilings:    ceilings,
	})

	return i.resultNode(map[string]any{
		"goalId":            goalId,
		"runId":             runId,
		"status":            runStatusCompiling,
		"compileDispatched": dispatched,
	}), nil
}

// dispatchCompile hands the run to the compile surface on a DETACHED
// goroutine, and reports whether there was one to hand it to.
//
// # The budget scope is stamped HERE, not inside compile
//
// memql.ContextWithBudgetScope is what makes the per-run and per-goal ceilings
// reachable by the LLM guard at the provider chokepoint (ai_guard.go). Compile
// is the FIRST thing that can reach a model on this goal's behalf, so a scope
// applied later would leave exactly the calls made before a template exists
// uncounted -- and those are the calls a runaway compile would make.
//
// # The context is deliberately NOT the caller's
//
// The caller's context dies when the builtin returns, and compile outlives it
// by design (the attachment handler's runAnalysisAsync pattern). So the
// detached work gets a background context carrying the borrowed actor and the
// budget scope, and nothing else.
//
// # A nil compiler is an ANSWER, and the run stays in `compiling`
//
// A node with no compile surface reports compileDispatched:false and says so
// in the log. The run is then the sweep's: it has a startedAt and no
// heartbeat, so the abandoned pass closes it with a sentence naming the node.
// Inventing a plan here would be worse in every direction.
func (i *Integration) dispatchCompile(ctx context.Context, req CompileRequest) bool {
	c := i.compilerRef()
	if c == nil {
		i.log().Warn("work: a goal was accepted on a node with no compile surface; the run stays in compiling until the abandoned sweep closes it",
			"component", "work.goal", "goal", req.GoalId, "run", req.RunId)
		return false
	}
	base := ownerActor(context.WithoutCancel(ctx), req.OwnerUserId)
	base = memql.ContextWithBudgetScope(base, compileBudgetScopes(req)...)
	go c.Compile(base, req)
	return true
}

// compileBudgetScopes names the two ceilings a compile spends against.
//
// Split out as a value so it is testable: the guard reads its scopes off the
// context through an UNEXPORTED key, so a test can observe THAT a context
// carries scopes but not WHICH. Naming them here makes the second half
// assertable too, and there is exactly one caller.
func compileBudgetScopes(req CompileRequest) []string {
	return []string{
		memql.BudgetScopeId("run", req.RunId),
		memql.BudgetScopeId("goal", req.GoalId),
	}
}

// handleCancelGoal asks every live run of one of the caller's goals to stop.
//
// CANCELLATION IS REQUESTED, NOT DONE (the builtin's own description). The
// flag is set and the run notices at its next step boundary, so a step already
// in flight finishes and is journaled rather than being abandoned mid-effect
// -- which for a step that has already written outside the graph is the
// difference between a receipt and an orphan.
//
// KNOWN RESIDUAL: the goal ROW is not closed. dsl/work/mutations.memql has no
// update-shaped goal writer, so status/closedAt/closeReason cannot be written
// from here. The reply says `goalClosed:false` rather than implying otherwise;
// the gaps note at the bottom of this file names the mutation that would fix
// it.
func (i *Integration) handleCancelGoal(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	goalId := argString(args, "goalId")
	if goalId == "" {
		return nil, fmt.Errorf("work: cancelGoal needs a goalId")
	}
	st := i.store()

	// Read the goal under the CALLER's own actor. A goal they cannot read is
	// a goal they cannot cancel, and the owned tier is what decides that --
	// there is no second check here, and there must not be one.
	goal, err := st.goalForOwner(ctx, goalId)
	if err != nil {
		return nil, err
	}
	if goal == nil {
		return nil, fmt.Errorf("work: no goal %q is readable by this caller", goalId)
	}
	owner := rowString(goal, "ownerUserId")

	runs, err := st.runsForGoal(ctx, goalId)
	if err != nil {
		return nil, err
	}

	reason := argString(args, "reason")
	asked := 0
	var failed []string
	for _, run := range runs {
		runId := rowString(run, "id")
		if runId == "" || isTerminalRunStatus(rowString(run, "status")) {
			continue
		}
		// Borrowed authority: the write guard ignores the clusterOwner arm,
		// so this write must run AS the owner. The value came off the goal
		// row this caller already read under their own actor, so it can
		// never name a user the caller could not act as.
		fields := map[string]any{
			"cancelRequested": true,
			"cancelledBy":     strings.TrimSpace(ac.UserId),
		}
		if reason != "" {
			fields["errorMessage"] = reason
		}
		if err := st.updateRun(ownerActor(ctx, owner), runId, fields); err != nil {
			// One run that will not take the flag must not stop the rest:
			// the others still deserve to be asked now.
			i.log().Warn("work: could not ask a run to stop",
				"component", "work.goal", "goal", goalId, "run", runId, "err", err)
			failed = append(failed, runId)
			continue
		}
		asked++
	}

	// The goal closes AFTER its runs have been asked to stop, and the order
	// is the guarantee: a goal closed first would be a goal reported finished
	// while its runs were still executing. Cancellation of a run is REQUESTED
	// -- the run notices at its next step boundary -- so "closed" here means
	// the intent is recorded everywhere it needs to be, not that everything
	// has already stopped.
	closed := true
	if err := st.closeGoalRow(ownerActor(ctx, owner), goalId, "closed", i.clock().UTC(), reason); err != nil {
		// The runs were still asked, which is the half that matters most, so
		// this reports rather than fails: a caller who is told the goal is
		// still open can close it again, and a caller told nothing happened
		// would re-issue a cancellation the runs have already taken.
		i.log().Warn("work: runs were asked to stop but the goal row would not close",
			"component", "work.goal", "goal", goalId, "err", err)
		closed = false
	}

	out := map[string]any{
		"goalId":     goalId,
		"runsAsked":  asked,
		"goalClosed": closed,
	}
	if len(failed) > 0 {
		out["runsRefused"] = failed
	}
	return i.resultNode(out), nil
}

// validRequestedVia checks the closed enum v1:work:goal.requestedVia declares.
//
// Checked HERE rather than left to the concept because the value arrives from
// a caller. A rejected enum on the write path costs a refused mutation whose
// error names the concept; refusing at the door names the argument.
func validRequestedVia(v string) error {
	switch v {
	case "", "api", "ask", "nexus", "responsibility", "library", "materializer":
		return nil
	}
	return fmt.Errorf("work: requestedVia %q is not one of api, ask, nexus, responsibility, library, materializer", v)
}

// newRowId mints a canonical row id for a concept.
func newRowId(concept string) string {
	return concept + ":" + id.NewShortId()
}

// selfNodeId names this replica, for the run row and the abandoned sweep's
// message.
func selfNodeId() string {
	if v := strings.TrimSpace(os.Getenv("MEMQL_NODE_ID")); v != "" {
		return v
	}
	if host, err := os.Hostname(); err == nil {
		return strings.TrimSpace(host)
	}
	return ""
}

// trim is the one spelling of whitespace trimming in this package.
func trim(s string) string { return strings.TrimSpace(s) }

// ---------------------------------------------------------------------------
// DSL GAPS THIS PACKAGE CANNOT CLOSE ON ITS OWN
// ---------------------------------------------------------------------------
//
// dsl/work/ is owned elsewhere; these are the writers epic A2 needs and does
// not have. Each is stated where the code hits it, and collected here so the
// list is findable:
//
//  1. CLOSED. closeWorkGoal(goalId, status, closedAt, closeReason) exists
//     and cancelGoal calls it. Compile moving a goal from `open` to `active`
//     still has no caller. Epic A3 lands `updateWorkGoal`, the same write
//     with a wider argument list; this drops and re-points to it then.
//
//  2. updateWorkRun must accept automationName, templateConstructId,
//     templateVersion and variables. Compile chooses a template AFTER the run
//     is open (that ordering is the design), and there is currently no way to
//     record the choice on the run it was made for.
//
//  3. createWorkRun must accept replayPolicy and variables. Without
//     replayPolicy a `permissive` replay is not expressible, so replayRun
//     REFUSES that argument rather than silently making a strict run; without
//     variables a fork cannot carry its overrides, so forkRun refuses those
//     too. Both refusals are in fork.go.
//
//  4. Two @serverOnly reads the sweeps need: `workRunsInFlight` (every run at
//     a non-terminal status, whoever owns it) and an expired-journal read
//     over modelCall / observation. Until they exist sweep.go asks the
//     database directly and applies AdmitSourceRow to every row as fetched --
//     which is correct but is the hand-rolled path the DSL exists to avoid.
