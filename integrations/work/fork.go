package work

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
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
// BUILT, as of memql#4999. That seam is component/memql/model_journal.go: it
// computes the request hash, asks DecideServe once, and either serves the
// journaled answer or calls through and writes the row via
// integrations/work.ModelJournal. The context stamped in deriveRun below is
// what carries this run's mode and lineage to it.
//
// This paragraph used to record the RESIDUAL -- "a run opened in replay mode
// is a row that says `replay` and nothing reads it" -- and it is kept in this
// shape rather than deleted because the next reader's question is the same
// one: where does the decision get made. It is made in exactly one place, and
// a pass-through adapter here would still be wrong.

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

	// VARIABLE OVERRIDES ARE CARRIED NOW (memql#5000). This refused them,
	// on the grounds that "createWorkRun does not accept `variables`, so an
	// override would be dropped silently" -- which stopped being true in
	// 3a189cbe2, when the mutation gained the argument. The Go seed simply
	// never passed it, so the refusal outlived its reason and turned a
	// closed gap into a permanent restriction. That is the shape worth
	// naming: a guard citing a limitation nobody re-checks.
	overrides := argMap(args, "variables")

	source, err := i.readOwnRun(ctx, sourceRunId)
	if err != nil {
		return nil, err
	}
	newRunId, err := i.deriveRun(ctx, source, derivation{
		Mode:            modeFork,
		ForkedFromRunId: sourceRunId,
		ForkAtStepKey:   atStepKey,
		TriggeredBy:     "fork:" + sourceRunId,
		Variables:       overrides,
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

	// BOTH POLICIES ARE RECORDABLE NOW (memql#5000). This refused
	// `permissive` because "createWorkRun does not accept `replayPolicy`" --
	// true when it was written, and not since 3a189cbe2. The refusal was
	// right to exist: serving a permissive request strictly would raise a
	// divergence the caller did not ask for and could not explain. It is the
	// REASON that expired, not the caution.
	//
	// strict stays the default, because a replay that quietly calls a
	// provider on a miss reports a reproduction that did not happen.
	policy := argString(args, "policy")
	switch policy {
	case "":
		policy = "strict"
	case "strict", "permissive":
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
		ReplayPolicy:    policy,
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
	// ReplayPolicy is the new run's, empty for the concept default.
	ReplayPolicy string
	// Variables OVERRIDE the source's, per key, rather than replacing the
	// set. A fork that changed one input and silently dropped the rest would
	// not be a fork of anything.
	Variables map[string]any
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
		ReplayPolicy:        d.ReplayPolicy,
		Variables:           mergedVariables(source, d.Variables),
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

	// The derived run gets its OWN budget scope, and dispatchCompile is what
	// stamps it -- from compileBudgetScopes(req), off the run and goal ids in
	// the request built below. This used to re-derive the same two scopes
	// here and assign the result to `_`, which read as though it did
	// something and did not: sharing the source's budget would make a replay
	// spend against a ceiling already accounted for, and the line that
	// prevented it was dead.
	//
	// THE RUN CONTEXT IS WHAT THIS PATH DOES NEED TO STAMP (memql#4999), and
	// it is what turns this row from a record of intent into a replay. The
	// engine's model seam reads it, asks work.DecideServe, and serves the
	// source run's journaled answers instead of calling a provider.
	// context.WithoutCancel inside dispatchCompile preserves values, so it
	// survives onto the detached goroutine.
	ctx = common.ContextWithRun(ctx, common.RunContext{
		RunId:  runId,
		GoalId: goalId,
		Mode:   d.Mode,
		// strict is the only policy a derived run can carry today --
		// createWorkRun does not accept replayPolicy, and handleReplayRun
		// refuses a permissive request rather than recording it as strict.
		ReplayPolicy: common.ReplayStrict,
		SourceRunId:  d.ForkedFromRunId,
		// The source's goal, so the cross-goal rule has something to compare.
		// deriveRun copies goalId off the source row, so these agree by
		// construction -- and asserting it here is what would catch the day
		// they stop agreeing.
		SourceGoalId:  goalId,
		ForkAtStepKey: d.ForkAtStepKey,
		StepOrder:     rowStringSlice(source, "stepOrder"),
		OwnerUserId:   owner,
	})

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

// rowStringSlice reads a []string field off a decoded row.
//
// Payload arrays decode as []any, so the []string arm is the one that never
// fires against a real row and the []any arm is the one that does. Both are
// here because a test that builds a row in Go writes the typed form, and a
// helper that works only in tests is worse than none.
func rowStringSlice(row map[string]any, key string) []string {
	switch v := row[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// mergedVariables layers a fork's overrides over the source run's variables.
//
// PER KEY, not wholesale. A fork exists to change one thing and hold the rest
// fixed, so replacing the whole set would make every override silently drop
// every value the caller did not restate -- which is the failure the old
// refusal was protecting against, kept now that the override is real.
//
// nil overrides return the source's own map, so an ordinary fork carries the
// same variables and a replay is byte-identical.
func mergedVariables(source map[string]any, overrides map[string]any) map[string]any {
	base := rowMap(source, "variables")
	if len(overrides) == 0 {
		return base
	}
	out := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
