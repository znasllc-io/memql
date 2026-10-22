package work

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// A successful execution is evidence for a person's review, not acceptance of
// their goal. This endpoint accepts the exact result version the person saw.
// The lifecycle lock is shared with reruns and cancellation: an old review must
// never close a newer attempt. Feedback is retained on the append-only goal row.
func (i *Integration) handleAcceptGoalResults(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if rc, running := common.RunFromContext(ctx); ac.Synthetic || ac.Unranked || (running && rc.IsRun()) {
		return nil, fmt.Errorf("work: accepting goal results requires the person's interactive decision")
	}
	goalID, runID, version := argString(args, "goalId"), argString(args, "runId"), argString(args, "runVersion")
	feedback := argString(args, "feedback")
	reviewedVersion, versionErr := time.Parse(time.RFC3339Nano, version)
	if versionErr != nil {
		return nil, fmt.Errorf("work: the result changed or its version is invalid; review the current result")
	}
	version = rfc(reviewedVersion)
	if goalID == "" || runID == "" || version == "" || utf8.RuneCountInString(feedback) > 4000 {
		return nil, fmt.Errorf("work: choose the goal and exact finished result, with at most 4000 characters of feedback")
	}
	release, err := i.decisionGate(ctx, "goal-lifecycle-"+memql.BareShortId(goalID))
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = memql.ContextWithFreshRead(ctx)
	goal, err := i.store().goalForOwner(ctx, goalID)
	if err != nil {
		return nil, err
	}
	if goal == nil || memql.BareShortId(rowString(goal, "ownerUserId")) != memql.BareShortId(ac.UserId) {
		return nil, fmt.Errorf("work: this goal is not owned by the person accepting it")
	}
	accepted := rowMap(goal, "acceptance")
	if rowString(goal, "status") == "closed" {
		if memql.BareShortId(rowString(accepted, "runId")) == memql.BareShortId(runID) && rowString(accepted, "runVersion") == version && rowString(accepted, "feedback") == feedback && rowString(accepted, "acceptedBy") == ac.UserId {
			return i.resultNode(map[string]any{"goalId": goalID, "goalClosed": true}), nil
		}
		return nil, fmt.Errorf("work: this goal is already closed by a different decision")
	}
	runs, err := i.store().runsForGoal(ctx, goalID)
	if err != nil {
		return nil, err
	}
	var reviewed map[string]any
	for _, run := range runs {
		if !isTerminalRunStatus(rowString(run, "status")) {
			return nil, fmt.Errorf("work: a run is still active; stop it or wait before accepting the results")
		}
		if memql.BareShortId(rowString(run, "id")) == memql.BareShortId(runID) {
			reviewed = run
		}
	}
	storedVersion, storedValid := rowTime(reviewed, "createdAt")
	if reviewed == nil || !storedValid || !storedVersion.Equal(reviewedVersion) {
		return nil, fmt.Errorf("work: the result changed; review the current result before accepting it")
	}
	reviewedAt, valid := rowTime(reviewed, "createdAt")
	if !valid {
		return nil, fmt.Errorf("work: the reviewed result has no valid version")
	}
	for _, run := range runs {
		if at, ok := rowTime(run, "createdAt"); !ok || at.After(reviewedAt) {
			return nil, fmt.Errorf("work: newer results are available; review them before accepting the goal")
		}
	}
	// The person can accept a useful partial result even when an execution
	// failed. Do not replace their judgment with the model's success flag.
	acceptance := map[string]any{"runId": runID, "runVersion": version, "acceptedBy": ac.UserId, "feedback": feedback, "acceptedAt": rfc(i.clock())}
	err = i.store().writeInternal(ownerActor(ctx, ac.UserId), "mutation "+call("updateWorkGoal", map[string]any{
		"goalId": goalID, "status": "closed", "closedAt": rfc(i.clock()), "closeReason": "Results accepted by the owner", "acceptance": acceptance,
		"versionTime": rfc(workRowVersionAfter(goal["createdAt"], i.clock())),
	}))
	if err != nil {
		return nil, err
	}
	return i.resultNode(map[string]any{"goalId": goalID, "goalClosed": true}), nil
}
