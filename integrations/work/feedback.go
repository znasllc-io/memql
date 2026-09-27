package work

// feedback.go -- recordFeedback: a person's verdict on one of their runs, or
// on one version of one of its steps (epic memql#5414, #5416; design D21 and
// D22).
//
// A VERDICT IS AN OBSERVATION, AND IT NEVER OVERWRITES. Every call writes a
// NEW v1:work:observation of kind `feedback` under a fresh id, so a person who
// changes their mind leaves both verdicts in the record and the newest is
// their current judgment -- the reading epic D's candidate gate and the
// procedure corpus both give (component/work/verdict.go,
// integrations/procedure/corpus.go readFeedback). The row is written in
// exactly the shape they read: data.verdict, data.target.stepKey (a string;
// absent means the RUN) and data.target.version (a number), plus the axes, the
// reason and the goal's signature this epic adds.
//
// A DISLIKE MUST SAY WHAT WAS WRONG (D21). component/work.ValidateFeedback
// refuses one naming none of the framework's three axes, because a dislike
// that says only "wrong" is something nothing downstream can act on: not a
// re-run's guidance, not the goal's description guidance.
//
// THE VALIDATOR'S VERDICT IS KEPT BESIDE THE PERSON'S, NEVER RESOLVED (D22).
// When the answer validator judged the same version the other way, the row
// records the disagreement and names the validator's observation; that
// disagreement is the signal the validator's prompt needs work.

import (
	"context"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
)

func (i *Integration) handleRecordFeedback(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if _, err := requirePrincipal(ctx); err != nil {
		return nil, err
	}
	runId := argString(args, "runId")
	if runId == "" {
		return nil, fmt.Errorf("work: recordFeedback needs a runId")
	}
	verdict := work.ParseVerdict(argString(args, "verdict"))
	axes := work.Axes{
		Product:     argBool(args, "product"),
		Process:     argBool(args, "process"),
		Performance: argBool(args, "performance"),
	}
	reason := argString(args, "reason")
	if err := work.ValidateFeedback(verdict, axes, reason); err != nil {
		return nil, fmt.Errorf("work: %w", err)
	}

	run, err := i.readActRun(ctx, runId)
	if err != nil {
		return nil, err
	}

	stepKey := argString(args, "stepKey")
	version := 0
	if stepKey != "" {
		if version, err = i.feedbackTarget(ctx, run, stepKey, argInt(args, "version", 0)); err != nil {
			return nil, err
		}
	}

	scoped := ownerActor(ctx, run.owner)
	st := i.store()
	observations, err := st.query(scoped, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": run.id}))
	if err != nil {
		return nil, err
	}

	target := map[string]any{}
	if stepKey != "" {
		target = map[string]any{"stepKey": stepKey, "version": version}
	}
	data := map[string]any{
		"verdict":       string(verdict),
		"axes":          axes.Object(),
		"reason":        reason,
		"target":        target,
		"goalSignature": rowString(run.row, "goalSignature"),
	}
	disagrees := false
	if stepKey != "" {
		if decision := newestValidatorDecision(observations, stepKey, version); decision != nil {
			validator := validatorVerdictOf(decision)
			disagrees = work.Disagrees(verdict, validator)
			data["validatorDisagrees"] = disagrees
			data["validatorObservationId"] = rowString(decision, "id")
		}
	}

	observationId := newRowId(observationConcept)
	write := map[string]any{
		"observationId": observationId,
		"runId":         run.id,
		"kind":          "feedback",
		"content":       work.FeedbackContent(verdict, stepKey, version, axes, reason),
		"data":          data,
	}
	if stepKey != "" {
		write["stepKey"] = stepKey
	}
	if err := st.writeInternal(scoped, "mutation "+call("createWorkObservation", write)); err != nil {
		return nil, err
	}

	return i.resultNode(map[string]any{
		"observationId":      observationId,
		"verdict":            string(verdict),
		"validatorDisagrees": disagrees,
	}), nil
}

// feedbackTarget resolves the step version a verdict names: the version given,
// or the step's current one, and in either case one the run RECORDS -- a
// verdict on a version that never ran would hold a procedure at candidate on
// evidence that does not exist.
//
// The key need not be in the run's step order: a recording run's actions are
// steps with no order, and a verdict on one of them is exactly what the
// candidate gate reads. It must not be nested, because a nested statement's
// version belongs to the step that holds it.
func (i *Integration) feedbackTarget(ctx context.Context, run actRun, stepKey string, version int) (int, error) {
	if strings.Contains(stepKey, "/") {
		return 0, refuse(codeFeedbackTargetNotFound, "%s is inside another step; judge the step that holds it", stepKey)
	}
	versions, err := i.readVersions(ctx, run.id, run.order)
	if err != nil {
		return 0, err
	}
	head := headOf(run, versions)
	if version <= 0 {
		version = versions.newest(stepKey)
		if e, named := head[stepKey]; named {
			if e.RunId != "" && bareRunId(e.RunId) != bareRunId(run.id) {
				return 0, refuse(codeFeedbackTargetNotFound, "step %s of run %s is served from run %s; judge it there", stepKey, run.id, e.RunId)
			}
			version = e.Version
		}
	}
	if versions.row(stepKey, version) == nil {
		if versions.newest(stepKey) == 0 {
			return 0, refuse(codeFeedbackTargetNotFound, "run %s records no step %s", run.id, stepKey)
		}
		return 0, refuse(codeFeedbackTargetNotFound, "run %s records no version %d of %s", run.id, version, stepKey)
	}
	return version, nil
}

// newestValidatorDecision is the answer validator's newest decision on one
// step version: a `decision` observation carrying data.validator.
func newestValidatorDecision(observations []map[string]any, stepKey string, version int) map[string]any {
	var newest map[string]any
	var newestAt time.Time
	for _, o := range observations {
		if rowString(o, "kind") != "decision" {
			continue
		}
		data := rowMap(o, "data")
		if rowMap(data, "validator") == nil {
			continue
		}
		target := rowMap(data, "target")
		if rowString(target, "stepKey") != stepKey || rowInt(target, "version") != version {
			continue
		}
		at, _ := rowTime(o, "createdAt")
		if newest == nil || !at.Before(newestAt) {
			newest, newestAt = o, at
		}
	}
	return newest
}

// validatorVerdictOf reads a validator decision back into component/work's
// shape.
func validatorVerdictOf(decision map[string]any) work.ValidatorVerdict {
	v := rowMap(rowMap(decision, "data"), "validator")
	return work.ValidatorVerdict{Axes: work.ParseAxes(v["axes"]), Reason: rowString(v, "reason")}
}
