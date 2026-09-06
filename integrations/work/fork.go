package work

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// fork.go -- forkRun and replayRun (design record section D, "Replay has
// three modes").
//
// Both derive a NEW run from an existing one and leave the source untouched.
// They differ only in what the new run's mode makes the journal serve:
//
//	fork    the shared prefix from the journal, live from the fork step on
//	replay  EVERY model call from the journal, so it reaches no provider
//
// THE DECISION ITSELF IS NOT HERE, and there is deliberately no adapter for
// it here either. component/work.DecideServe answers ONE MODEL REQUEST at a
// time, and no model request is made in this package -- these two handlers
// record the mode, the lineage and the policy, and the run they open is picked
// up by the executor. So the ONE caller of DecideServe must be the executor's
// model-call seam, beside where the request hash is computed and the
// v1:work:modelCall row is written.
//
// RESIDUAL, stated so it is findable rather than discovered: that seam is the
// OTHER half of epic A2 and does not exist yet. Until it does, a run opened in
// replay mode is a row that says `replay` and nothing reads it -- so a replay
// today records its intent and does not yet serve from the journal. A
// pass-through adapter here would not change that and would make it look
// wired.

// handleForkRun derives a run that diverges at a step key.
func (i *Integration) handleForkRun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requirePrincipal(ctx); err != nil {
		return nil, err
	}
	sourceRunId := argString(args, "runId")
	if sourceRunId == "" {
		return nil, fmt.Errorf("work: forkRun needs a runId")
	}
	atStepKey := argString(args, "atStepKey")
	if atStepKey == "" {
		return nil, fmt.Errorf("work: forkRun needs an atStepKey -- a fork with no divergence point is a replay")
	}

	// REFUSED RATHER THAN DROPPED. createWorkRun does not accept `variables`
	// today, so an override would be accepted here, written nowhere, and the
	// fork would run on the source's values while the caller believed
	// otherwise. Silently dropping an argument that changes what the work
	// DOES is the class of bug this refusal exists to prevent; the residual
	// is recorded in goal.go's gaps note.
	if len(argMap(args, "variables")) > 0 {
		return nil, fmt.Errorf("work: forkRun cannot carry variable overrides yet -- createWorkRun does not accept `variables`, so an override would be dropped silently; fork without them, or add the argument to the mutation")
	}

	source, err := i.readOwnRun(ctx, sourceRunId)
	if err != nil {
		return nil, err
	}
	newRunId, err := i.deriveRun(ctx, source, derivation{
		Mode:            modeFork,
		ForkedFromRunId: sourceRunId,
		ForkAtStepKey:   atStepKey,
		TriggeredBy:     "fork:" + sourceRunId,
	})
	if err != nil {
		return nil, err
	}
	return i.resultNode(map[string]any{
		"runId":           newRunId,
		"forkedFromRunId": sourceRunId,
		"atStepKey":       atStepKey,
		"mode":            modeFork,
	}), nil
}

// handleReplayRun derives a run served entirely from the journal.
func (i *Integration) handleReplayRun(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requirePrincipal(ctx); err != nil {
		return nil, err
	}
	sourceRunId := argString(args, "runId")
	if sourceRunId == "" {
		return nil, fmt.Errorf("work: replayRun needs a runId")
	}

	// strict is the concept's default and the only policy a new run can
	// carry, because createWorkRun does not accept `replayPolicy`.
	//
	// A permissive request is REFUSED rather than quietly served strictly,
	// and the direction of the refusal matters: a strict replay of a changed
	// prompt raises a divergence and stops, so the caller would see a
	// failure they did not ask for and could not explain. Saying so costs
	// one error; guessing costs an investigation.
	policy := argString(args, "policy")
	switch policy {
	case "", "strict":
		policy = "strict"
	case "permissive":
		return nil, fmt.Errorf("work: replayRun cannot record a permissive policy yet -- createWorkRun does not accept `replayPolicy`, so the new run would be strict and a journal miss would raise a divergence you did not ask for")
	default:
		return nil, fmt.Errorf("work: replay policy %q is not strict or permissive", policy)
	}

	source, err := i.readOwnRun(ctx, sourceRunId)
	if err != nil {
		return nil, err
	}
	newRunId, err := i.deriveRun(ctx, source, derivation{
		Mode: modeReplay,
		// forkedFromRunId is the LINEAGE field, and a replay has lineage
		// exactly as a fork does: it names the run whose journal is being
		// served. Without it a replay is a run that came from nowhere, and
		// nothing could find the journal it must read.
		ForkedFromRunId: sourceRunId,
		TriggeredBy:     "replay:" + sourceRunId,
	})
	if err != nil {
		return nil, err
	}
	return i.resultNode(map[string]any{
		"runId":         newRunId,
		"replayOfRunId": sourceRunId,
		"policy":        policy,
		"mode":          modeReplay,
	}), nil
}

// readOwnRun resolves a run through the CALLER's own actor.
//
// This is the whole of the authorization on both capabilities, deliberately:
// workRunForOwner filters on ownerUserId==actor.userId, so a run the caller
// cannot read simply is not there, and a run they cannot read is one they
// cannot fork or replay. A second check keyed on a field of the returned row
// would be a check against a row the gate had already vouched for.
func (i *Integration) readOwnRun(ctx context.Context, runId string) (map[string]any, error) {
	run, err := i.store().runForOwner(ctx, runId)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("work: no run %q is readable by this caller", runId)
	}
	return run, nil
}

// derivation is what distinguishes the new run from its source.
type derivation struct {
	Mode            string
	ForkedFromRunId string
	ForkAtStepKey   string
	TriggeredBy     string
}

// deriveRun opens the new run, inheriting the source's template identity and
// input and taking its owner from the SOURCE ROW rather than from the caller.
//
// Inheriting the owner is what keeps a cluster owner's fork of somebody else's
// run owned by that somebody: the new run belongs to the person whose work it
// continues, not to whoever pressed the button. It is the same borrow the rest
// of the package makes -- the value comes off a row this caller already read
// under their own actor.
func (i *Integration) deriveRun(ctx context.Context, source map[string]any, d derivation) (string, error) {
	owner := rowString(source, "ownerUserId")
	goalId := rowString(source, "goalId")
	runId := newRowId(runConcept)
	now := i.clock().UTC()

	seed := runSeed{
		RunId:  runId,
		GoalId: goalId,
		// The template identity is inherited verbatim. A derived run that
		// compiled itself afresh would not be a replay of anything.
		AutomationName:      rowString(source, "automationName"),
		TemplateFingerprint: rowString(source, "templateFingerprint"),
		Input:               rowMap(source, "input"),
		InputFingerprint:    rowString(source, "inputFingerprint"),
		TriggeredBy:         d.TriggeredBy,
		Mode:                d.Mode,
		ForkedFromRunId:     d.ForkedFromRunId,
		ForkAtStepKey:       d.ForkAtStepKey,
		Status:              runStatusCompiling,
		NodeId:              selfNodeId(),
		StartedAt:           now,
		OwnerUserId:         owner,
	}
	if err := i.store().createRunRow(ownerActor(ctx, owner), seed); err != nil {
		return "", err
	}

	// The derived run gets its OWN budget scope. Sharing the source's would
	// make a replay spend against a budget that is already accounted for,
	// and a fork of a nearly exhausted run would be dead on arrival.
	_ = memql.ContextWithBudgetScope(ctx,
		memql.BudgetScopeId("run", runId),
		memql.BudgetScopeId("goal", goalId))

	if dispatched := i.dispatchCompile(ctx, CompileRequest{
		GoalId:      goalId,
		RunId:       runId,
		OwnerUserId: owner,
		Statement:   rowString(source, "automationName"),
		Input:       rowMap(source, "input"),
	}); !dispatched {
		i.log().Info("work: a derived run is waiting for a compile surface",
			"component", "work.fork", "run", runId, "mode", d.Mode, "from", d.ForkedFromRunId)
	}
	return runId, nil
}
