//go:build agent

package app

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	work "github.com/znasllc-io/memql/integrations/work"
)

// PrepareDispatch waits for an answered invocation to finish unwinding before
// spending its once-only claim. An answer can precede the stopped step receipt.
// A timeout consumes nothing: the durable humanResumeId plus stopped receipt
// lets the ordinary recovery sweep claim that same answer, even while the
// original execution's leased claim still exists.
func (d *workRunDispatcher) PrepareDispatch(ctx context.Context, req work.DispatchRequest) (work.DispatchRequest, bool) {
	if !req.Recovery && req.HumanResumeId == "" {
		return req, true
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		journal, err := automations.LoadRunJournal(memql.ContextWithFreshRead(waitCtx), d.app.engine, req.RunId)
		if err != nil {
			return req, false
		}
		if req.Recovery {
			if journal.GoalId == "" || journal.Status != "running" || journal.HumanResumeId == "" || !humanResumeReceiptReady(journal) {
				return req, true
			}
			req.HumanResumeId = journal.HumanResumeId
			req.RerunRequestId = ""
			if journal.Rerun != nil {
				req.RerunRequestId = journal.Rerun.RequestId
			}
			req.OwnerUserId, req.GoalId, req.AutomationName = journal.OwnerUserId, journal.GoalId, journal.AutomationName
			req.HumanResumeRecovery = !journal.HeartbeatAt.IsZero() && time.Since(journal.HeartbeatAt) >= work.DefaultAbandonedAfterSeconds*time.Second
			req.Recovery = req.HumanResumeRecovery
		}
		if journal.CancelRequested || journal.Status != "running" || req.HumanResumeId != journal.HumanResumeId || journal.GoalId == "" || !workRerunServable(req, journal) {
			return req, false
		}
		if !journal.HasRunningStep && humanResumeReceiptReady(journal) {
			return req, true
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return req, false
		case <-timer.C:
		}
	}
}

func humanResumeReceiptReady(journal *automations.RunJournal) bool {
	if journal.StepStates[journal.FailedStep].Status == "waiting" {
		return true
	}
	// Failure feedback leaves a failed receipt, not a suspended invocation.
	// Retry authorizes the target's next version. Once that version has started
	// or failed, this answer must not admit it again as another fresh attempt.
	rerun := journal.Rerun
	if rerun == nil || rerun.Reason != automations.RerunReasonRerun || rerun.RequestId == "" {
		return false
	}
	step := journal.StepStates[rerun.StepKey]
	return step.Status == "failed" && rerun.Versions[rerun.StepKey] > max(step.Attempt, step.Version)
}
