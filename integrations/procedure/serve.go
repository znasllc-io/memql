package procedure

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// serve.go -- a goal served by a learned procedure (epic memql#5408, plan
// Task 5 step 5).
//
// Compile's exact tier serves a trusted or canary procedure through ONE
// embedded template, replayLearnedProcedure, whose one statement is the
// procedureReplay builtin: the run it executes in IS the goal's run, the
// construct rides in the run's variables, and this handler is where the
// procedure actually serves. It is the last place three things are checked,
// and each fails closed:
//
//	the run      the call must come from inside a work run -- the goal's --
//	             and the construct must be that run's OWNER's: a person
//	             naming somebody else's procedure is refused, never served
//	the rung     re-decided NOW (component/work.DecideServe): a procedure
//	             demoted or re-lifted since compile chose it is handed back to
//	             the app rather than replayed
//	the answer   served by the procedure, or handed to the app with the
//	             partial trace; a goal neither could serve fails, naming why
//	             -- never a silent success
//
// procedureStep, the statement every rendered step of a learned procedure's
// SOURCE is, refuses unconditionally: the source is the artifact a person
// reads and approves, and a procedure is served through the ladder, never
// activated as an automation.

// stepRefusal is procedureStep's one answer.
const stepRefusal = "procedureStep runs only inside replayLearnedProcedure"

func (i *Integration) handleProcedureStep(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return nil, fmt.Errorf("procedure.step: %s -- a learned procedure is served through the certification ladder, never run as an automation", stepRefusal)
}

// handleProcedureReplay serves the current work run from a learned procedure.
func (i *Integration) handleProcedureReplay(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	constructId := strings.TrimSpace(argString(args, "constructId"))
	if constructId == "" {
		return nil, fmt.Errorf("procedure.replay: constructId is required")
	}
	rc, inRun := common.RunFromContext(ctx)
	if !inRun {
		return nil, fmt.Errorf("procedure.replay: a learned procedure serves a goal's work run, and this call is not inside one -- it is replayLearnedProcedure's statement")
	}
	owner := strings.TrimSpace(rc.OwnerUserId)
	ac, _ := auth.AccessFromContext(ctx)
	if ac != nil && !ac.Synthetic {
		if owner == "" {
			owner = strings.TrimSpace(ac.UserId)
		} else if !sameUser(ac.UserId, owner) {
			return nil, fmt.Errorf("procedure.replay: the run belongs to another person, and a goal is served only as its owner")
		}
	}
	if owner == "" {
		return nil, fmt.Errorf("procedure.replay: the run names no owner, so there is nobody whose procedure could serve it")
	}
	actorCtx := ownerActor(ctx, owner)
	run, err := i.runForOwner(actorCtx, rc.RunId)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("procedure.replay: run %s is not readable as its owner", rc.RunId)
	}

	// THE CONSTRUCT MUST BE THE OWNER'S. Read under the owner, somebody
	// else's construct reads as nothing, and that is refused here -- the
	// replay would read nothing either, and a goal served from a procedure
	// the person never learned is the failure this gate exists for.
	row, err := i.constructForOwner(ctx, owner, constructId)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("procedure.replay: construct %s is not one of the run owner's learned procedures", constructId)
	}

	goalId := firstNonEmpty(str(run, "goalId"), rc.GoalId)
	statement, input := "", map[string]any(nil)
	if goalId != "" {
		rows, gerr := i.store.query(actorCtx, "query "+call("workGoalForOwner", map[string]any{"goalId": goalId}))
		if gerr == nil && len(rows) > 0 {
			statement = strings.TrimSpace(str(rows[0], "statement"))
			input = obj(rows[0], "input")
		}
	}
	if input == nil {
		// The goal's input is what the procedure's parameters were learned
		// against; a run whose goal cannot be read binds from its variables,
		// less the one the compile route added.
		input = map[string]any{}
		for k, v := range obj(run, "variables") {
			if k != "procedureConstructId" {
				input[k] = v
			}
		}
	}

	rung := storedRung(row)
	mode := ReplayTrusted
	if v := work.DecideServe(work.ReplayContext{Mode: "live", ConstructRung: rung}); v.Standby {
		mode = ReplayCanary
	}
	out, err := i.Replay(ctx, ReplayRequest{
		OwnerUserId: owner, ConstructId: constructId, Mode: mode,
		GoalRunId: rc.RunId, StepKey: rc.StepKey, GoalId: goalId, Statement: statement, Input: input,
	})
	if err != nil {
		return nil, err
	}
	servedBy := ""
	switch {
	case out.Served:
		servedBy = "procedure"
	case out.FellBack:
		servedBy = "app"
	default:
		// Neither the procedure nor the app served the goal. Failing the
		// statement fails the goal's run, naming why: a goal reported done
		// that nobody did is the one answer this must never give.
		return nil, fmt.Errorf("procedure.replay: %s: %s", firstNonEmpty(out.Code, codeFallbackUnavailable), firstNonEmpty(out.Diagnosis, "the goal was not served"))
	}
	return i.reply(map[string]any{
		"servedBy":     servedBy,
		"constructId":  constructId,
		"rung":         string(out.Rung),
		"diverged":     out.Diverged,
		"startRefused": out.StartRefused,
		"repairRunId":  out.Fallback.ChildRunId,
		"replayRunId":  out.ReplayRunId,
		"reason":       out.Diagnosis,
	}), nil
}
