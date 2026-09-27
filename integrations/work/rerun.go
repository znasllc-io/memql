package work

// rerun.go -- rerunStep: run one step of a finished run again as a new
// version (epic memql#5414, #5415; design D18, D20 and D23's repair half).
//
// THE ACT WRITES A REQUEST AND RUNS NOTHING. It validates, decides what the
// re-run executes (component/work.PlanRerun: the step and every step after it,
// each as a new version, the prefix served from its current versions), and
// writes ONE run update: the run back to `running`, the request on run.rerun,
// and the steps that will get new versions on run.staleSteps. The agent that
// claims the run's `running` event serves the request off the row -- this
// replica holds nothing the executing one needs, and is not asked to.
//
// WHAT A PERSON CHANGED APPLIES TO ONE VERSION OF ONE STEP (D20). The override
// rides the request for the targeted step alone; the steps re-run after it
// run as the system's own work against the new upstream.

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
)

func (i *Integration) handleRerunStep(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	runId := argString(args, "runId")
	stepKey := argString(args, "stepKey")
	if runId == "" || stepKey == "" {
		return nil, fmt.Errorf("work: rerunStep needs a runId and a stepKey")
	}
	requestedBy := trim(ac.UserId)
	override, err := overrideFromArgs(args, requestedBy)
	if err != nil {
		return nil, err
	}

	run, err := i.readActRun(ctx, runId)
	if err != nil {
		return nil, err
	}
	if err := run.requireFinished(); err != nil {
		return nil, err
	}
	if err := run.requireTopLevel(stepKey); err != nil {
		return nil, err
	}

	versions, err := i.readVersions(ctx, run.id, run.order)
	if err != nil {
		return nil, err
	}
	plan, err := work.PlanRerun(run.order, versions.byKey, stepKey)
	if err != nil {
		return nil, headRefusal(err)
	}
	head := headOf(run, versions)

	// THE VERSION BEING REPLACED IS THE HEAD'S. A dislike on it rides the
	// re-run as guidance, so the next attempt is told what was wrong.
	if e := head[stepKey]; e.RunId == "" {
		guidance, err := i.dislikeGuidance(ctx, run.owner, run.id, stepKey, e.Version)
		if err != nil {
			return nil, err
		}
		override = withGuidance(override, guidance)
	}

	// A session step starts a NEW session against the workspace as it was
	// before the step, in a fresh directory of its own: the default one is
	// where the replaced version, and every step after it, did their work.
	snapshot, err := i.sessionSnapshot(ctx, run, versions, head, stepKey)
	if err != nil {
		return nil, err
	}
	workspace := ""
	if snapshot != nil {
		workspace = fmt.Sprintf("%s-v%d", bareRunId(run.id), plan.Versions[stepKey])
	}

	now := i.clock().UTC()
	fields := reopenFields(now)
	fields["rerun"] = rerunRequest(rerunReasonRerun, stepKey, override, snapshot, workspace, requestedBy, now)
	fields["staleSteps"] = plan.Stale
	if err := i.store().updateRun(ownerActor(ctx, run.owner), run.id, fields); err != nil {
		return nil, err
	}

	reply := map[string]any{
		"runId":      run.id,
		"stepKey":    stepKey,
		"version":    plan.Versions[stepKey],
		"staleSteps": plan.Stale,
	}
	if snapshot != nil {
		// REPORTED, never silently restored: an earlier command's effects are
		// not in the snapshot, and the person deciding whether to trust the
		// new version should hear that now rather than find it later.
		reply["unrecordedCommands"] = snapshot.UnrecordedCommands
	}
	return i.resultNode(reply), nil
}
