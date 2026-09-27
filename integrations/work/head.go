package work

// head.go -- moveRunHead: make an earlier version of a step current again
// (epic memql#5414, #5415; design D18).
//
// Going back is not an undo. component/work.MoveHead decides the new head --
// the chosen version WITH the upstream it was computed from, every later step
// whose recorded version was computed from that same upstream restored, and
// only the first step with no such version (and everything after it) left to
// run again -- and this act makes the stores agree with it.
//
// THE HEAD IS RE-ASSERTED, NOT ONLY POINTED AT. Every collapsed read -- the
// timeline, the procedure corpus, resume's journal loader -- answers with a
// step's NEWEST row-version, and after a re-run that is the later version. So
// for every step whose current version changed, the chosen version's row is
// written again, verbatim, as a new row-version (reassertWorkStepVersion):
// the store stays append-only, the later versions stay readable through the
// version history, and every reader that never heard of heads keeps answering
// with the head. The rows go first and the run last, because the run's write
// is what can start a re-run, and the executor it starts loads the collapsed
// rows.

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
)

func (i *Integration) handleMoveRunHead(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	runId := argString(args, "runId")
	stepKey := argString(args, "stepKey")
	version := argInt(args, "version", 0)
	if runId == "" || stepKey == "" {
		return nil, fmt.Errorf("work: moveRunHead needs a runId and a stepKey")
	}
	if version <= 0 {
		return nil, refuse(codeVersionNotFound, "moveRunHead needs the version to make current, a positive number")
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
	old := headOf(run, versions)
	next, stale, err := work.MoveHead(run.order, old, versions.byKey, stepKey, version)
	if err != nil {
		return nil, headRefusal(err)
	}

	scoped := ownerActor(ctx, run.owner)
	st := i.store()
	for _, key := range run.order {
		e, ok := next[key]
		if !ok || e == old[key] {
			continue
		}
		if e.RunId != "" && bareRunId(e.RunId) != bareRunId(run.id) {
			// The version lives in the run this one forked; its row is that
			// run's, and a branch leaves its source untouched (D19).
			continue
		}
		row := versions.row(key, e.Version)
		if row == nil {
			return nil, refuse(codeVersionNotFound, "run %s records no version %d of %s to make current", run.id, e.Version, key)
		}
		if err := st.writeInternal(scoped, "mutation "+call("reassertWorkStepVersion", reassertion(row, e.Version))); err != nil {
			return nil, fmt.Errorf("work: re-assert version %d of %s: %w", e.Version, key, err)
		}
	}

	fields := map[string]any{
		"head":       next.Object(),
		"staleSteps": staleList(stale),
	}
	if len(stale) > 0 {
		// Only the steps with no version computed from the new upstream run
		// again, from the first of them, with nobody's changes: the upstream
		// is what changed.
		now := i.clock().UTC()
		for k, v := range reopenFields(now) {
			fields[k] = v
		}
		fields["rerun"] = rerunRequest(rerunReasonHeadMove, stale[0], work.Override{}, nil, "", trim(ac.UserId), now)
	}
	if err := st.updateRun(scoped, run.id, fields); err != nil {
		return nil, err
	}

	return i.resultNode(map[string]any{
		"runId":      run.id,
		"stepKey":    stepKey,
		"version":    version,
		"staleSteps": staleList(stale),
	}), nil
}

// reassertion is the argument set that writes one version's row again. EVERY
// per-version field is named, empty values included: the update is a
// read-merge, and a field left out would keep the LATER version's value under
// the earlier version's number -- a v1 row carrying v2's result, or v2's
// override, is a record of a run that never happened.
func reassertion(row map[string]any, version int) map[string]any {
	attempt := rowInt(row, "attempt")
	if attempt <= 0 {
		attempt = version
	}
	return map[string]any{
		"stepId":            rowString(row, "id"),
		"status":            rowString(row, "status"),
		"result":            objectOrEmpty(row, "result"),
		"resultFingerprint": rowString(row, "resultFingerprint"),
		"binding":           objectOrEmpty(row, "binding"),
		"postcondition":     objectOrEmpty(row, "postcondition"),
		"symptom":           rowString(row, "symptom"),
		"attempt":           attempt,
		"version":           version,
		"basis":             objectOrEmpty(row, "basis"),
		"override":          objectOrEmpty(row, "override"),
		"authoredBy":        rowString(row, "authoredBy"),
		"childRunId":        rowString(row, "childRunId"),
		"idempotencyKey":    rowString(row, "idempotencyKey"),
		"startedAt":         rowString(row, "startedAt"),
		"finishedAt":        rowString(row, "finishedAt"),
		"durationMs":        rowInt(row, "durationMs"),
		"tokens":            rowInt(row, "tokens"),
		"cost":              rowFloat(row, "cost"),
		"errorCode":         rowString(row, "errorCode"),
		"errorMessage":      rowString(row, "errorMessage"),
	}
}

// objectOrEmpty is a stored object field, or {} -- an explicit empty object,
// which call() renders, rather than an absent argument, which it drops.
func objectOrEmpty(row map[string]any, key string) map[string]any {
	if m := rowMap(row, key); m != nil {
		return m
	}
	return map[string]any{}
}

// staleList renders the stale steps, [] when there are none: `[]` is how a
// writer clears run.staleSteps, and an absent argument would keep the old list.
func staleList(stale []string) []string {
	if stale == nil {
		return []string{}
	}
	return stale
}
