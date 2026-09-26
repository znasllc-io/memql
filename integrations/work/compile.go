package work

// Compilation crosses the same graph-event boundary as execution: the BFF
// persists a compiling run, planner replicas observe it, and PostgreSQL picks
// one owner. A missed event is recovered by SweepWaiting. Local callers use
// this same claim, so an event racing the direct call cannot compile twice.

import (
	"context"
	"fmt"
	"github.com/znasllc-io/memql/component/auth"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

const compileClaimName = "work.run.compile"
const compileHeartbeatInterval = DefaultAbandonedAfterSeconds * time.Second / 4

// dispatchCompile hands the run to a compile surface and reports whether one
// took it.
//
// # Local compiler
//
// When this replica has a Compiler (planner), claim the run and compile on a
// detached goroutine. All model-call context is reconstructed from stored
// rows; an event carries only the run id and the owner needed for scoped reads.
//
// # Event forward (bff)
//
// When this replica has no Compiler but EnableCompileViaEvent was set, the
// run row's own graph event IS the handoff: a planner's HandleRunEvent picks
// up `compiling` the way an agent's HandleRunEvent picks up `running`.
// Returning true here is what stops createGoal from implying the work is
// stranded.
//
// # Neither
//
// hasCompileSurface refused before the writes. Reaching here with neither is
// a programmer error; we still return false rather than inventing a plan.
func (i *Integration) dispatchCompile(ctx context.Context, hint CompileRequest) bool {
	if hint.RunId == "" || hint.OwnerUserId == "" {
		return false
	}
	c := i.compilerRef()
	if c == nil {
		if i.compileViaEventEnabled() {
			i.log().Info("work: compile handed to the cluster via the run graph event",
				"component", "work.goal", "goal", hint.GoalId, "run", hint.RunId, "node", selfNodeId())
			return true
		}
		i.log().Error("work: dispatchCompile reached with no compile surface; the caller should have refused before writing the run",
			"component", "work.goal", "goal", hint.GoalId, "run", hint.RunId)
		return false
	}
	base := ownerActor(context.WithoutCancel(ctx), hint.OwnerUserId)
	// Finish ownership well within the lease: an indefinitely stalled
	// claimant must not wake after a peer took its expired claim.
	claimCtx, cancelClaim := context.WithTimeout(base, compileHeartbeatInterval)
	defer cancelClaim()
	_, _, ok, err := i.pendingCompile(claimCtx, hint.RunId)
	if err != nil {
		i.log().Warn("work compile: could not read pending run", "run", hint.RunId, "error", err)
		return false
	}
	if !ok || !i.claimCompile(claimCtx, hint.RunId) {
		return false
	}
	// A delayed event or claimant may have read before another replica
	// finished. Recheck AFTER arbitration, including persisted ownership:
	// even after a lease expires, a long-running compiler is never restarted.
	req, rc, ok, err := i.pendingCompile(claimCtx, hint.RunId)
	if err != nil || !ok {
		return false
	}
	base, err = auth.ContextWithPersistedOwner(base, req.OwnerUserId)
	if err != nil {
		i.log().Warn("work compile: could not restore the owner's forwarded authority", "run", req.RunId, "error", err)
		return false
	}
	base = common.ContextWithRun(base, rc)
	base = memql.ContextWithBudgetScope(base, compileBudgetScopes(req)...)
	if err := i.store().updateRun(claimCtx, req.RunId, map[string]any{
		"nodeId": selfNodeId(), "heartbeatAt": rfc(i.clock()),
	}); err != nil {
		i.log().Warn("work compile: could not record planner ownership; refusing to compile", "run", req.RunId, "error", err)
		return false
	}
	i.log().Info("work: claimed a run for compilation", "component", "work.compile", "run", req.RunId, "goal", req.GoalId, "node", selfNodeId())
	go i.compileWithHeartbeat(base, c, req)
	return true
}

// pendingCompile reads under the event's owner, with no internal-origin or
// cluster-owner read bypass. A forged/mismatched owner therefore reads no run.
// heartbeatAt is the durable evidence that a planner already took this run;
// older versions wrote the intake BFF into nodeId before any compiler ran.
func (i *Integration) pendingCompile(ctx context.Context, runId string) (CompileRequest, common.RunContext, bool, error) {
	var req CompileRequest
	var rc common.RunContext
	run, err := i.store().runForOwner(ctx, runId)
	if err != nil || run == nil {
		return req, rc, false, err
	}
	if rowString(run, "status") != runStatusCompiling || rowString(run, "heartbeatAt") != "" || argBool(run, "cancelRequested") {
		return req, rc, false, nil
	}
	goalId := rowString(run, "goalId")
	if goalId == "" {
		return req, rc, false, nil
	}
	goal, err := i.store().goalForOwner(ctx, goalId)
	if err != nil || goal == nil || rowString(goal, "status") == "closed" {
		return req, rc, false, err
	}
	req = CompileRequest{
		RunId: runId, GoalId: goalId, OwnerUserId: rowString(run, "ownerUserId"),
		Statement: rowString(goal, "statement"), Input: rowMap(run, "input"), Ceilings: rowMap(goal, "ceilings"),
	}
	rc = common.RunContext{
		RunId: runId, GoalId: goalId, OwnerUserId: req.OwnerUserId,
		Mode: rowString(run, "mode"), ReplayPolicy: rowString(run, "replayPolicy"),
		SourceRunId: rowString(run, "forkedFromRunId"), ForkAtStepKey: rowString(run, "forkAtStepKey"),
	}
	if rc.Mode == "" {
		rc.Mode = common.RunModeLive
	}
	if rc.ReplayPolicy == "" {
		rc.ReplayPolicy = common.ReplayStrict
	}
	if rc.SourceRunId != "" {
		source, sourceErr := i.store().runForOwner(ctx, rc.SourceRunId)
		if sourceErr != nil || source == nil {
			return req, rc, false, sourceErr
		}
		rc.SourceGoalId = rowString(source, "goalId")
		rc.StepOrder = rowStringSlice(source, "stepOrder")
	}
	return req, rc, true, nil
}

// Use the existing claim table, but fail CLOSED on storage errors. The
// scheduler's ClusterExecutionGuard deliberately allows bounded unguarded
// execution during outages; compilation must never turn that into multiple
// authoring loops. The persisted heartbeat fences later lease takeovers.
func (i *Integration) claimCompile(ctx context.Context, runId string) bool {
	if i.bunDB == nil || i.bunDB() == nil {
		i.log().Warn("work compile: no PostgreSQL claim store; refusing to compile", "run", runId)
		return false
	}
	result, err := i.bunDB().ExecContext(ctx, `
		INSERT INTO automation_execution_claims (automation_name, dedup_key, claimed_by)
		VALUES (?, ?, ?)
		ON CONFLICT (automation_name, dedup_key)
		DO UPDATE SET claimed_by = EXCLUDED.claimed_by, claimed_at = now()
		WHERE automation_execution_claims.claimed_at < now() - make_interval(secs => ?)`,
		compileClaimName, runId, selfNodeId(), runClaimTTL.Seconds())
	if err != nil {
		i.log().Warn("work compile: PostgreSQL claim failed; refusing to compile", "run", runId, "error", err)
		return false
	}
	n, err := result.RowsAffected()
	return err == nil && n == 1
}

type compileHeartbeatKey struct{}

func (i *Integration) compileWithHeartbeat(ctx context.Context, compiler Compiler, req CompileRequest) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if observer, ok := i.engine.(interface {
		ObserveWorkCalls(context.Context, context.CancelCauseFunc) context.Context
	}); ok {
		// Install on the planner that received the persisted run. An observer
		// on the originating BFF cannot see compilation's model calls.
		ctx = observer.ObserveWorkCalls(ctx, cancel)
	}
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	done := make(chan struct{})
	// Cancel an in-flight database pulse before joining it. Closing only a
	// stop channel cannot interrupt a pulse already inside Execute.
	stopHeartbeat := func() { cancelHeartbeat(); <-done }
	ctx = context.WithValue(ctx, compileHeartbeatKey{}, stopHeartbeat)
	go func() {
		defer close(done)
		ticker := time.NewTicker(compileHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				pulseCtx, cancelPulse := context.WithTimeout(heartbeatCtx, 5*time.Second)
				run, err := i.store().runForOwner(pulseCtx, req.RunId)
				if err != nil {
					cancelPulse()
					i.log().Warn("work compile: heartbeat read failed", "run", req.RunId, "error", err)
					continue
				}
				if rowString(run, "status") != runStatusCompiling || argBool(run, "cancelRequested") {
					cancelPulse()
					cancel(context.Canceled)
					return
				}
				err = i.store().updateRun(pulseCtx, req.RunId, map[string]any{"heartbeatAt": rfc(i.clock())})
				cancelPulse()
				if err != nil {
					i.log().Warn("work compile: heartbeat write failed", "run", req.RunId, "error", err)
				}
			}
		}
	}()
	defer stopHeartbeat()
	compiler.Compile(ctx, req)
}

// Stop and join the heartbeat before an outcome write. Otherwise a heartbeat
// read-merge can race that write and replace running/failed with compiling.
func (i *Integration) stopCompileHeartbeat(ctx context.Context, runId string) error {
	stop, ok := ctx.Value(compileHeartbeatKey{}).(func())
	if !ok {
		return nil
	}
	stop()
	run, err := i.store().runForOwner(context.WithoutCancel(ctx), runId)
	if err != nil {
		return err
	}
	if rowString(run, "status") != runStatusCompiling {
		return fmt.Errorf("work: compile outcome refused because run %s is no longer compiling", runId)
	}
	if argBool(run, "cancelRequested") {
		if err := i.store().updateRun(context.WithoutCancel(ctx), runId, map[string]any{"status": runStatusCancelled, "finishedAt": rfc(i.clock())}); err != nil {
			return err
		}
		return fmt.Errorf("work: compile outcome refused because run %s was cancelled", runId)
	}
	return nil
}
