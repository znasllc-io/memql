package procedure

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
)

// shadow.go -- a succeeded recording, compared with every learned procedure
// in shadow for the same goal (epic memql#5408, D15; plan Task 5 step 3).
//
// In shadow the APP serves every goal. When its session's recording succeeds,
// the procedure is replayed beside it -- in the workbench sandbox, or DRY for a
// machine-local procedure -- on the bindings the app's own actions give its
// parameters, and each step is compared with what the app did. m consecutive
// matches across k distinct bindings of every free parameter propose the one
// promotion; one mismatch starts the streak again.
//
// THE RECORDING IS READ THE WAY THE CORPUS READS IT -- the same loader, so its
// arguments are relativized against its own workspace (relativize.go) by the
// same function the lift used, and its unconsumed pure reads are dropped by
// the same canonicalization. Then its actions are written in the procedure's
// alphabet (component/procedure.AssignSymbol, under the lift's own distance
// budget) and the procedure's steps are looked for, in order, with the gap the
// miner allowed between them; the run of actions found is bound to the
// template (BindInstance). A recording that is not an instance -- no such run
// of actions, or one whose values differ where the procedure holds one -- is
// a MISMATCH: the app did this goal some other way.
//
// WHAT IS NOT COMPARED, AND IS NOT A MISMATCH EITHER: a run that holds no
// actions at this level (the goal's own run, whose steps are statements), a
// recording a person disliked, and one whose calls cannot be read back whole.
// None of them says anything about whether the procedure does what the app
// does.
//
// CALLED FROM THE LEARN HANDLER, AFTER THE LIFT, ONLY WHEN THE LIFT LEFT THE
// VERSION UNCHANGED (LearnResult.Lift == LiftUnchanged): a recording that
// changed the procedure is part of the new version's corpus, not evidence
// about it. The exported LearnFromRun does NOT compare -- its callers (the
// proving driver, tests) call ShadowCompare themselves, and a lift that also
// compared would count their recording twice.

// ShadowCompare compares a succeeded recording against every learned
// procedure in shadow for its goal, and answers one outcome per procedure
// compared. The caller is gated as LearnFromRun's is: the run's OWNER, whose
// owned read is the only way the recording is read -- there is no cluster-wide
// variant, and the completion trigger borrows the owner before it compares.
func (i *Integration) ShadowCompare(ctx context.Context, recordingRunId string) ([]ReplayOutcome, error) {
	runId := strings.TrimSpace(recordingRunId)
	if runId == "" {
		return nil, fmt.Errorf("procedure.shadowCompare: recordingRunId is required")
	}
	run, err := i.readRunForCaller(ctx, runId)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("procedure.shadowCompare: run %s is not readable as the caller", runId)
	}
	owner := strings.TrimSpace(str(run, "ownerUserId"))
	if ac, ok := auth.AccessFromContext(ctx); !ok || ac == nil || strings.TrimSpace(ac.UserId) == "" || !sameUser(ac.UserId, owner) {
		// Belt and braces over the owned read: a cluster owner CAN read
		// another person's run, and comparing it would move that person's
		// ladder on somebody else's say-so.
		return nil, fmt.Errorf("procedure.shadowCompare: run %s belongs to another person; "+
			"a recording is compared with its owner's own procedures", runId)
	}
	return i.shadowCompareRun(ctx, run)
}

// shadowCompareRun is ShadowCompare over a run already read.
func (i *Integration) shadowCompareRun(ctx context.Context, run map[string]any) ([]ReplayOutcome, error) {
	runId := str(run, "id")
	owner := strings.TrimSpace(str(run, "ownerUserId"))
	sig := strings.TrimSpace(str(run, "goalSignature"))
	switch {
	case owner == "", sig == "", isReplayRun(run), str(run, "status") != "succeeded":
		// No owner to compare as, no goal to compare for, a replay (which
		// never shadows itself), or a recording that did not succeed --
		// which is no evidence of what the goal takes.
		return nil, nil
	}
	actorCtx := ownerActor(ctx, owner)
	rows, err := i.store.query(actorCtx, "query "+call("procedureConstructsForGoalSignature", map[string]any{"goalSignature": sig}))
	if err != nil {
		return nil, err
	}
	var shadows []map[string]any
	for _, row := range rows {
		if storedRung(row) == work.RungShadow && str(row, "goalSignature") == sig {
			shadows = append(shadows, row)
		}
	}
	if len(shadows) == 0 {
		return nil, nil
	}
	rec, ok, err := i.loadRecording(actorCtx, run, LevelAction)
	if err != nil {
		return nil, err
	}
	if !ok {
		i.log().Debug("procedure: a succeeded run is not a recording to compare", "runId", runId)
		return nil, nil
	}
	actions := proc.Canonicalize(rec.Steps)
	proc.SortActions(actions)
	inputs := make(map[string]map[string]any, len(rec.Steps))
	for _, s := range rec.Steps {
		inputs[s.Key] = s.Input
	}

	var outs []ReplayOutcome
	for _, row := range shadows {
		constructId := str(row, "id")
		p, err := DecodeProcedure(row["procedure"])
		if err != nil || p.Level != int(LevelAction) {
			i.log().Debug("procedure: a shadow procedure is not one an action recording is compared with",
				"constructId", constructId, "error", err)
			continue
		}
		req := ReplayRequest{Mode: ReplayShadow, OwnerUserId: owner, ConstructId: constructId, GoalRunId: runId}
		inst, bindings, unfit, step := i.instanceOf(p, actions)
		if unfit != "" {
			req.Unfit, req.UnfitStep = unfit, step
		} else {
			req.Bindings = bindings
			for _, a := range inst {
				req.AppActions = append(req.AppActions, rec.Evidence[a.Key].Observation)
				req.AppArgs = append(req.AppArgs, inputs[a.Key])
			}
		}
		out, err := i.Replay(ctx, req)
		if err != nil {
			i.log().Warn("procedure: a shadow comparison could not run", "constructId", constructId, "runId", runId, "error", err)
			continue
		}
		outs = append(outs, out)
	}
	return outs, nil
}

// instanceOf finds the run of a recording's actions that is an instance of the
// procedure: its steps' symbols in order, no more than the miner's gap apart,
// binding to the template. It answers the actions, every hole's value, and --
// when there is none -- why, with the first step the recording lacked.
func (i *Integration) instanceOf(p Procedure, actions []proc.Action) ([]proc.Action, map[string]string, string, int) {
	t := p.Template()
	if len(t.Steps) == 0 {
		return nil, nil, "the procedure has no steps", 0
	}
	syms := make([]string, len(actions))
	for n, a := range actions {
		syms[n], _ = proc.AssignSymbol(p.Symbols, a, i.params.SymbolBudget)
	}
	want := make([]string, len(p.Steps))
	for n, s := range p.Steps {
		want[n] = s.Symbol
	}
	gap := i.params.Gap
	if gap < 0 {
		gap = 0
	}
	best, bound := 0, false
	for start := range actions {
		if syms[start] != want[0] {
			continue
		}
		picked := []int{start}
		for j := 1; j < len(want); j++ {
			prev, found := picked[len(picked)-1], -1
			for q := prev + 1; q < len(actions) && q <= prev+1+gap; q++ {
				if syms[q] == want[j] {
					found = q
					break
				}
			}
			if found < 0 {
				break
			}
			picked = append(picked, found)
		}
		if len(picked) > best {
			best = len(picked)
		}
		if len(picked) != len(want) {
			continue
		}
		inst := make([]proc.Action, len(picked))
		for n, at := range picked {
			inst[n] = actions[at]
		}
		if b, ok := proc.BindInstance(t, inst); ok {
			return inst, b, "", 0
		}
		bound = true
	}
	if bound {
		return nil, nil, "The recording's actions follow the procedure's steps, and a value the procedure holds constant differs, so the app did this goal some other way.", 0
	}
	if best >= len(want) {
		best = len(want) - 1
	}
	return nil, nil, fmt.Sprintf("The recording has no action matching step %d (%s) of the procedure where the procedure needs it, so the app did this goal some other way.",
		best, oneLine(t.Steps[best].Tool)), best
}
