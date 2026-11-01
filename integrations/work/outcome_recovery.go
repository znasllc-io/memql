package work

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

// An acknowledged reporting Retry can be stranded before dispatch. Recover
// only its unchanged, unstarted failed receipt after watchdog abandonment.
// The person must retry the same saved answer; no cron resurrects old work.
// This restores execution intent, never permissions, usage or goal acceptance.
func (i *Integration) recoverOutcomeDecision(ctx context.Context, approvalID, decision string, answer map[string]any) (map[string]any, error) {
	if decision != "answered" || len(answer) != 1 || rowString(answer, "value") != workstate.FailureAnswerRetry {
		return nil, nil
	}
	rows, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
	if err != nil || len(rows) != 1 {
		return nil, err
	}
	a := rows[0]
	subject := rowMap(a, "subject")
	message := rowString(subject, "errorMessage")
	if rowString(a, "kind") != workstate.ApprovalKindFeedback || rowString(a, "decision") != "answered" ||
		!workstate.IsFailureQuestion(a["options"]) || !workstate.IsOutcomeRecoveryFailure(message) ||
		workstate.ArtifactHash(rowMap(a, "answer")) != workstate.ArtifactHash(answer) ||
		memql.BareShortId(rowString(a, "ownerUserId")) != memql.BareShortId(callerUserId(ctx)) ||
		memql.BareShortId(rowString(a, "decidedBy")) != memql.BareShortId(callerUserId(ctx)) {
		return nil, nil
	}
	if workstate.ArtifactHash(subject) != rowString(a, "artifactHash") {
		return nil, fmt.Errorf("work: the saved reporting decision changed")
	}
	run, err := i.readActRun(ctx, rowString(a, "runId"))
	if err != nil {
		return nil, err
	}
	release, err := i.decisionGate(ctx, "goal-lifecycle-"+memql.BareShortId(rowString(run.row, "goalId")), "workload:"+memql.BareShortId(run.id))
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	run, err = i.readActRun(ctx, run.id)
	if err != nil {
		return nil, err
	}
	if run.row["cancelRequested"] == true || memql.BareShortId(rowString(run.row, "humanResumeId")) != memql.BareShortId(approvalID) {
		return nil, fmt.Errorf("work: the saved reporting execution changed")
	}
	result := map[string]any{"approvalId": approvalID, "runId": run.id, "decision": "answered", "runResumed": true}
	if rowString(run.row, "status") == runStatusRunning {
		return result, nil
	}
	if rowString(run.row, "status") != runStatusAbandoned || rowString(run.row, "errorCode") != "run_abandoned" {
		return nil, fmt.Errorf("work: this reporting decision no longer names abandoned dispatch")
	}
	goal, err := i.store().goalForOwner(ctx, rowString(run.row, "goalId"))
	if err != nil {
		return nil, err
	}
	if goal == nil || rowString(goal, "status") != "open" || len(rowMap(goal, "acceptance")) != 0 {
		return nil, fmt.Errorf("work: this goal is closed or already accepted")
	}
	key := rowString(a, "stepKey")
	steps, err := i.store().query(ctx, "query "+call("workStepForOwnerRun", map[string]any{"runId": run.id, "stepKey": key}))
	if err != nil {
		return nil, err
	}
	decidedAt, decided := rowTime(a, "decidedAt")
	if len(steps) != 1 || !decided || rowString(steps[0], "status") != "failed" || rowString(steps[0], "errorMessage") != message {
		return nil, fmt.Errorf("work: the saved reporting failure changed")
	}
	stepAt, valid := rowTime(steps[0], "createdAt")
	if !valid || !stepAt.Before(decidedAt) {
		return nil, fmt.Errorf("work: this answer already started another execution")
	}
	if err := run.requireExecutable(); err != nil {
		return nil, err
	}
	if err := i.requireApprovalStep(ctx, &run, key); err != nil {
		return nil, err
	}
	now := i.clock().UTC()
	// Keep request, override, head, saved answer and spend byte-for-byte. The
	// agent must still validate the retained result before inference can start.
	if err := i.store().updateRun(ownerActor(ctx, run.owner), run.id, map[string]any{
		"status": runStatusRunning, "errorCode": "", "errorMessage": message,
		"heartbeatAt": rfc(now), "finishedAt": "", "waitingOn": map[string]any{},
		"versionTime": rfc(workRowVersionAfter(run.row["createdAt"], now)),
	}); err != nil {
		return nil, err
	}
	return result, nil
}
