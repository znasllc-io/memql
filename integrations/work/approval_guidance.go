package work

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

const maxApprovalGuidanceCharacters = 4000

// Guidance is an alternative to approval. Validate the exact parked run before
// recording the answer, so a stale/non-executable request is not consumed while
// claiming that its harness will reconsider it. The ordinary approval lock and
// fresh reads are already held by the caller.
func (i *Integration) approvalGuidance(ctx context.Context, approval map[string]any, decision string, answer map[string]any) (string, error) {
	raw, supplied := answer["feedback"]
	if !supplied {
		return "", nil
	}
	text, valid := raw.(string)
	text = strings.TrimSpace(text)
	if decision != "answered" || !valid || text == "" || utf8.RuneCountInString(text) > maxApprovalGuidanceCharacters || len(answer) != 1 {
		return "", fmt.Errorf("work: guidance needs an answered decision and 1 to 4000 characters of feedback, without an approval or permission choice")
	}
	if err := i.validateApprovalGuidance(ctx, approval); err != nil {
		return "", err
	}
	return text, nil
}

// ApprovalGuidanceAvailable projects the same eligibility used by the decision
// path. Opening a discussion neither consumes the request nor resumes work.
func (i *Integration) ApprovalGuidanceAvailable(ctx context.Context, approval map[string]any) bool {
	return i.validateApprovalGuidance(memql.ContextWithFreshRead(ctx), approval) == nil
}

// RecoverableApprovalGuidance lets a reopened discussion offer continuation of
// an already-recorded answer. It never turns an edited draft into a new grant.
func (i *Integration) RecoverableApprovalGuidance(ctx context.Context, approval map[string]any) string {
	answer := rowMap(approval, "answer")
	text := rowString(answer, "feedback")
	if rowString(approval, "decision") != "answered" || len(answer) != 1 || strings.TrimSpace(text) == "" || utf8.RuneCountInString(text) > maxApprovalGuidanceCharacters ||
		memql.BareShortId(rowString(approval, "decidedBy")) != memql.BareShortId(callerUserId(ctx)) || i.validateGuidanceRun(memql.ContextWithFreshRead(ctx), approval) != nil {
		return ""
	}
	return text
}

func (i *Integration) validateApprovalGuidance(ctx context.Context, approval map[string]any) error {
	if rowString(approval, "decision") != "" {
		return fmt.Errorf("work: guidance needs this person's pending request")
	}
	return i.validateGuidanceRun(ctx, approval)
}

func (i *Integration) validateGuidanceRun(ctx context.Context, approval map[string]any) error {
	principal, err := requirePrincipal(ctx)
	if err != nil {
		return err
	}
	if run, running := common.RunFromContext(ctx); principal.Synthetic || principal.Unranked || (running && run.RunId != "") {
		return fmt.Errorf("work: guidance requires a person's interactive decision")
	}
	if memql.BareShortId(rowString(approval, "ownerUserId")) != memql.BareShortId(principal.UserId) {
		return fmt.Errorf("work: guidance needs this person's pending request")
	}
	if !workstate.ApprovalAcceptsGuidance(rowString(approval, "kind"), rowMap(approval, "subject"), approval["options"]) {
		return fmt.Errorf("work: this request has its own review process and cannot resume from guidance")
	}
	run, err := i.readActRun(ctx, rowString(approval, "runId"))
	if err != nil {
		return err
	}
	if err := run.requireExecutable(); err != nil {
		return err
	}
	if err := i.requireApprovalStep(ctx, &run, topLevelStepKey(rowString(approval, "stepKey"))); err != nil {
		return err
	}
	if rowString(run.row, "status") != runStatusWaiting || run.row["cancelRequested"] == true || rowString(rowMap(run.row, "waitingOn"), "kind") != "approval" ||
		memql.BareShortId(rowString(rowMap(run.row, "waitingOn"), "subject")) != memql.BareShortId(rowString(approval, "id")) {
		return fmt.Errorf("work: this run is no longer waiting on the request being discussed")
	}
	return nil
}

// A human question can park inside an admitted step before its receipt advances
// the run's checkpoint. Recover only the contiguous, owner-admitted journal tail;
// the approval's step key alone is never evidence that a step belongs to a run.
func (i *Integration) requireApprovalStep(ctx context.Context, run *actRun, key string) error {
	if err := run.requireTopLevel(key); err == nil {
		return nil
	}
	if _, err := i.readActVersions(memql.ContextWithFreshRead(ctx), run); err != nil {
		return err
	}
	return run.requireTopLevel(key)
}

func guidanceRetry(approval map[string]any, text, decidedBy string) *failureRetry {
	return &failureRetry{stepKey: rowString(approval, "stepKey"), decidedBy: decidedBy, symptom: workstate.SymptomPlan,
		reason: "The person supplied guidance instead of approving the blocked action. Reconsider the unfinished work using this guidance; no permission or budget increase was granted.\n" + text + "\nOriginal request: " + rowString(approval, "question")}
}

// Recover only an exact human guidance receipt after its decision committed but
// resuming the run failed. The caller holds the same approval decision gate.
// No second answer is written and a changed answer never borrows this receipt.
func (i *Integration) recoverGuidanceDecision(ctx context.Context, approvalID, decision string, answer map[string]any) (map[string]any, error) {
	text, ok := answer["feedback"].(string)
	text = strings.TrimSpace(text)
	if decision != "answered" || !ok || text == "" || len(answer) != 1 || utf8.RuneCountInString(text) > maxApprovalGuidanceCharacters {
		return nil, nil
	}
	rows, err := i.store().query(ctx, "query "+call("workApprovalForOwner", map[string]any{"approvalId": approvalID}))
	if err != nil || len(rows) != 1 {
		return nil, err
	}
	approval := rows[0]
	if rowString(approval, "decision") != "answered" || rowString(rowMap(approval, "answer"), "feedback") != text || len(rowMap(approval, "answer")) != 1 ||
		memql.BareShortId(rowString(approval, "ownerUserId")) != memql.BareShortId(callerUserId(ctx)) ||
		memql.BareShortId(rowString(approval, "decidedBy")) != memql.BareShortId(callerUserId(ctx)) {
		return nil, nil
	}
	result := map[string]any{"approvalId": approvalID, "runId": rowString(approval, "runId"), "decision": "answered", "runResumed": false}
	run, err := i.readActRun(ctx, rowString(approval, "runId"))
	if err != nil {
		result["resumeError"] = err.Error()
		return result, nil
	}
	if memql.BareShortId(rowString(run.row, "humanResumeId")) == memql.BareShortId(approvalID) {
		result["runResumed"] = true
		return result, nil
	}
	if err := i.validateGuidanceRun(ctx, approval); err != nil {
		result["resumeError"] = err.Error()
		return result, nil
	}
	resumed, err := i.resumeParkedRun(ownerActor(ctx, rowString(approval, "ownerUserId")), rowString(approval, "runId"), approvalID, "answered", guidanceRetry(approval, text, callerUserId(ctx)), i.clock().UTC())
	result["runResumed"] = resumed
	if err != nil {
		result["resumeError"] = err.Error()
	}
	return result, nil
}
