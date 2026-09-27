package work

// branch.go -- branchRun: branch a finished run at a step into a NEW fork run
// (epic memql#5414, #5415; design D19).
//
// The branch is a run in the existing `fork` mode whose steps before the branch
// point are served BY REFERENCE from the source -- its head points each of
// them at the source's current version (component/work.ForkAt) and they never
// run again -- while the branch step runs live with the person's changes,
// followed by everything after it. The source run is untouched: nothing here
// writes to it.
//
// ONE WRITE, AND IT IS THE CREATE. The new run is opened already carrying its
// head and its request. Opened bare and completed by a second write, its own
// `running` event could reach an agent between the two, and that agent would
// execute a fork with no request on it -- every step, from the first.
//
// It is NOT forkRun's deriveRun, although it inherits the same things, because
// deriveRun opens its run in one write that cannot carry a head or a request.
// The inheritance is kept field for field (the template identity, the input,
// the variables, the goal, the execution authority), and the owner is the
// SOURCE's, for deriveRun's reason: a run belongs to the person whose work it
// continues, not to whoever pressed the button.

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
)

func (i *Integration) handleBranchRun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	sourceRunId := argString(args, "runId")
	stepKey := argString(args, "stepKey")
	if sourceRunId == "" || stepKey == "" {
		return nil, fmt.Errorf("work: branchRun needs a runId and a stepKey")
	}
	requestedBy := trim(ac.UserId)
	override, err := overrideFromArgs(args, requestedBy)
	if err != nil {
		return nil, err
	}

	source, err := i.readActRun(ctx, sourceRunId)
	if err != nil {
		return nil, err
	}
	if err := source.requireFinished(); err != nil {
		return nil, err
	}
	if err := source.requireExecutable(); err != nil {
		return nil, err
	}
	if err := source.requireTopLevel(stepKey); err != nil {
		return nil, err
	}

	versions, err := i.readVersions(ctx, source.id, source.order)
	if err != nil {
		return nil, err
	}
	sourceHead := headOf(source, versions)
	forkHead, err := work.ForkAt(source.order, sourceHead, source.id, stepKey)
	if err != nil {
		return nil, headRefusal(err)
	}
	// The fork's own step rows are new rows of a new run, so every step it
	// executes -- the branch step and each after it -- runs as that row's
	// first version: the plan over a run that has recorded nothing.
	plan, err := work.PlanRerun(source.order, nil, stepKey)
	if err != nil {
		return nil, headRefusal(err)
	}

	if e := sourceHead[stepKey]; e.RunId == "" {
		guidance, err := i.dislikeGuidance(ctx, source.owner, source.id, stepKey, e.Version)
		if err != nil {
			return nil, err
		}
		override = withGuidance(override, guidance)
	}

	// A branch of a session step is a NEW session against the workspace as it
	// was before that step (D19). A partial snapshot is refused here, naming
	// the file, before any run exists.
	snapshot, err := i.sessionSnapshot(ctx, source, versions, sourceHead, stepKey)
	if err != nil {
		return nil, err
	}

	runId := newRowId(runConcept)
	now := i.clock().UTC()
	seed := runSeed{
		ExecutionAuthority:  rowMap(source.row, "executionAuthority"),
		RunId:               runId,
		GoalId:              rowString(source.row, "goalId"),
		GoalSignature:       rowString(source.row, "goalSignature"),
		AutomationName:      rowString(source.row, "automationName"),
		TemplateFingerprint: rowString(source.row, "templateFingerprint"),
		TemplateConstructId: rowString(source.row, "templateConstructId"),
		TemplateVersion:     rowString(source.row, "templateVersion"),
		Input:               rowMap(source.row, "input"),
		InputFingerprint:    rowString(source.row, "inputFingerprint"),
		TriggeredBy:         "branch:" + bareRunId(source.id),
		Mode:                modeFork,
		Variables:           mergedVariables(source.row, nil),
		ForkedFromRunId:     source.id,
		ForkAtStepKey:       stepKey,
		Status:              runStatusRunning,
		StartedAt:           now,
		OwnerUserId:         source.owner,
		Head:                forkHead.Object(),
		// The fork's sessions run in a directory named for the fork, never in
		// the source's: the source is untouched, its workspace included.
		Rerun: rerunRequest(rerunReasonBranch, stepKey, override, plan.Versions, snapshot, bareRunId(runId), requestedBy, now),
	}
	if err := i.store().createRunRow(ownerActor(ctx, source.owner), seed); err != nil {
		return nil, err
	}

	reply := map[string]any{
		"runId":           runId,
		"forkedFromRunId": source.id,
		"forkAtStepKey":   stepKey,
	}
	if snapshot != nil {
		reply["unrecordedCommands"] = snapshot.UnrecordedCommands
	}
	return i.resultNode(reply), nil
}
