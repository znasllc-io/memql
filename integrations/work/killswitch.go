package work

// killswitch.go -- the in-flight half of the computer-use kill switch,
// rebuilt on the work spine (memql#5066).
//
// ===========================================================================
// WHAT WAS THERE, AND A CORRECTION TO THE RECORD
// ===========================================================================
// `killSwitchSuspendsRunningPlans` moved a user's running computer-use Plans
// to awaitingFeedback when `preferences.computerUseEnabled` flipped false. It
// went with the Plan concepts in memql#5053, on three findings -- and the
// FIRST OF THEM WAS WRONG, which is worth writing down because it is why the
// deletion looked free.
//
// It said the automation "never fired": its query's doc comment recorded that
// `args.event.payload.id ?? ""` reached argument validation as an unevaluated
// CoalesceExpr (memql#2870). That WAS true, and it stopped being true on
// 2026-07-27 in 8ef364cd7 ("evaluate expressions passed as nested-call
// arguments"), six weeks before the deletion. The doc comment survived its own
// fix, and memql#5066 inherited it as fact. component/memql's
// nested_call_arg_eval_test.go is the evidence either way: the sibling cases
// it drives through the real resolver -- magicLinkExpirySweep's bare `now`,
// deployGateGreen's root coalesce -- pass on main today.
//
// So this restores a control that was live, rather than building one that
// never was. The other two findings stand and shape what is below.
//
// ===========================================================================
// THE SELECTION, WHICH IS THE WHOLE DESIGN
// ===========================================================================
// The old predicate was `computerUseScope != null` -- "the plans actually
// using the worker". `v1:work:run` carries no computer-use marker, and adding
// one would put a field on the run concept to carry a scope it does not
// otherwise need. The alternative offered was cancelling EVERY in-flight run
// of that user, which is far broader than the control ever was.
//
// Neither is necessary. `v1:worker:invocation.runId` already names the run a
// call was made inside -- re-pointed from planId to run id by memql#5053 --
// so "runs actually using the worker" is a fact already recorded, per call,
// by the dispatcher. It is also STRICTER than the old predicate: a plan with a
// computerUseScope was one AUTHORIZED to use a machine, while an invocation
// row is one that DID.
//
// ===========================================================================
// WHY IT STARTS FROM THE RUNS
// ===========================================================================
// The set is { run : owner is U, status is running, some invocation names it }.
// It is read runs-first because that is the bounded side: a person has a
// handful of runs in flight and may have tens of thousands of invocation rows
// behind them. Starting from the invocations would page through call history
// to rediscover a set the run table already holds.
//
// The run table holds that set cheaply only through the recovery projection.
// The read was the DSL query workRunningRunsForOwner, which the engine
// answered from the owner's whole run history, and for production's cluster
// owner -- the owner of every pipeline run -- that ran past the statement
// limit (2026-10-29). It now reads work_run_heads (runningRunsForOwner in
// recovery_heads.go, which says why it does not refresh), and it REFUSES while
// that projection cannot answer, so a failure is reported rather than read as
// "nothing to stop". Either way the pre-dispatch gate is refusing the owner's
// next machine call.
//
// ===========================================================================
// AUTHORITY: INTERNAL ORIGIN, WHICH IS THE GATE THIS EXACT SURFACE TAUGHT
// ===========================================================================
// The argument names WHOSE runs to stop, so it IS the authorization decision
// and an ungated builtin taking it is a primitive for cancelling somebody
// else's work. memql#2888 hardened precisely this shape, and its rule is
// quoted in component/automations/executor.go above ExecuteWithClientEvent --
// naming this automation's predecessor as the worked example:
//
//	run_automation("killSwitchSuspendsRunningPlans", {node:{id: <any user>}})
//	reached runningPlansForUser (@serverOnly) with an attacker-chosen userId
//	and then transitioned every plan it returned [...] a cross-user write, not
//	just a read leak.
//
// The fix was that internal origin requires BOTH a trusted SOURCE (an
// automation from the registered tree, not a caller-submitted bundle) AND a
// trigger payload the caller did not supply. That is exactly the pair this
// handler needs, so it gates on origin and nothing else.
//
// NOT a cluster-owner floor, which was the first shape tried here and is the
// wrong one twice over: it does not stop the attack above (the payload, not
// the role, is what is chosen), and clearing it would put the automation on
// component/auth/maintenance_actor.go's list -- whose own gate test states the
// standard an entry must meet, "its reads span owners BY NATURE [...] not
// merely finding an owner-scoped read inconvenient. For that, borrow ONE
// owner's authority". This acts on one owner. So it borrows.
//
// Everything after the gate runs under the AFFECTED OWNER's borrowed
// authority: v1:work:run's write guard ignores the clusterOwner arm of its
// composite tier, so a write as anyone but the owner is refused. That is
// handleCancelGoal's rule, and this obeys it for the same reason.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// killSwitchActor is what lands in `cancelledBy`. It is a sentinel rather than
// a person: nobody clicked stop on these runs individually, and attributing
// them to the owner would misreport what happened -- they flipped a switch,
// which is a different act from cancelling six specific runs.
const killSwitchActor = "system:kill-switch:computerUse"

// killSwitchReason is what lands on the run's errorMessage, in the words a
// person reading their own run rail needs. It says what to do next, because a
// run that stopped with no route back is a support ticket.
const killSwitchReason = "Stopped because computer use was switched off for this account. " +
	"Turn it back on in your preferences, then start the work again."

// CancelComputerUseRuns asks every in-flight run of one owner that has
// actually dispatched to one of their machines to stop.
//
// It returns how many runs it looked at and how many it asked, because the
// difference is the whole claim: "checked 9, asked 2" is the selection being
// narrow, and "checked 9, asked 9" on a cluster doing other work would mean it
// had stopped being.
func (i *Integration) CancelComputerUseRuns(ctx context.Context, ownerUserId string) (checked, asked int, err error) {
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" {
		return 0, 0, fmt.Errorf("work: the computer-use kill switch needs the owner whose runs to stop")
	}
	st := i.store()
	// One borrowed-authority context, used for the read, the per-run check and
	// the write. The owner came from the user row the trigger fired on, or
	// from a cluster owner's own call; neither is a caller naming somebody
	// else's id on an open surface.
	as := ownerActor(ctx, owner)

	if i.runningRuns == nil {
		return 0, 0, fmt.Errorf("work: no running-runs read is wired; the kill switch cannot tell what to stop")
	}
	runs, err := i.runningRuns(as, owner)
	if err != nil {
		return 0, 0, err
	}
	for _, run := range runs {
		runId := rowString(run, "id")
		if runId == "" {
			continue
		}
		checked++
		used, checkErr := st.runUsedAWorker(as, runId)
		if checkErr != nil {
			// A run whose invocation history cannot be read is LEFT ALONE. The
			// opposite default -- "cannot tell, so stop it" -- would turn a
			// database hiccup into every in-flight run of that user being
			// cancelled, which is the broad behaviour this selection exists to
			// avoid. The pre-dispatch gate is still refusing their next call
			// either way, so the control is not open in the meantime.
			i.log().Warn("work: could not tell whether a run used a worker; leaving it running",
				"component", "work.killswitch", "owner", owner, "run", runId, "err", checkErr)
			continue
		}
		if !used {
			continue
		}
		if updErr := st.updateRun(as, runId, map[string]any{
			"cancelRequested": true,
			"cancelledBy":     killSwitchActor,
			"errorMessage":    killSwitchReason,
		}); updErr != nil {
			// One run that will not take the flag must not stop the rest, for
			// handleCancelGoal's reason: the others still deserve to be asked
			// now.
			i.log().Warn("work: could not ask a computer-use run to stop",
				"component", "work.killswitch", "owner", owner, "run", runId, "err", updErr)
			continue
		}
		asked++
	}
	if asked > 0 {
		i.log().Info("work: the computer-use kill switch asked runs to stop",
			"component", "work.killswitch", "owner", owner, "checked", checked, "asked", asked)
	}
	return checked, asked, nil
}

// handleCancelComputerUseRuns is the builtin's executor. The gate is FIRST,
// because a refusal after a read is a read that happened.
func (i *Integration) handleCancelComputerUseRuns(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("work: cancelComputerUseRuns is reachable only from the engine's own " +
			"kill-switch automation -- its ownerUserId argument is the authorization decision, so a call " +
			"whose payload a caller chose is refused (memql#2888)")
	}
	owner := argString(args, "ownerUserId")
	checked, asked, err := i.CancelComputerUseRuns(ctx, owner)
	if err != nil {
		return nil, err
	}
	return i.resultNode(map[string]any{
		"ownerUserId": owner,
		"runsChecked": checked,
		"runsAsked":   asked,
	}), nil
}
