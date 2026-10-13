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

func (i *Integration) validateApprovalGuidance(ctx context.Context, approval map[string]any) error {
	principal, err := requirePrincipal(ctx)
	if err != nil {
		return err
	}
	if run, running := common.RunFromContext(ctx); principal.Synthetic || principal.Unranked || (running && run.RunId != "") {
		return fmt.Errorf("work: guidance requires a person's interactive decision")
	}
	if rowString(approval, "decision") != "" || memql.BareShortId(rowString(approval, "ownerUserId")) != memql.BareShortId(principal.UserId) {
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
	if err := run.requireTopLevel(topLevelStepKey(rowString(approval, "stepKey"))); err != nil {
		return err
	}
	if rowString(run.row, "status") != runStatusWaiting || run.row["cancelRequested"] == true || rowString(rowMap(run.row, "waitingOn"), "kind") != "approval" ||
		memql.BareShortId(rowString(rowMap(run.row, "waitingOn"), "subject")) != memql.BareShortId(rowString(approval, "id")) {
		return fmt.Errorf("work: this run is no longer waiting on the request being discussed")
	}
	return nil
}
