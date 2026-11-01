//go:build agent

package app

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	work "github.com/znasllc-io/memql/integrations/work"
)

// PrepareDispatch waits for an answered invocation to finish unwinding before
// spending its once-only claim. An answer can precede the stopped step receipt.
// A timeout consumes nothing. Once the original invocation settles, report
// re-offers the durable answer through wakeSettledHumanWait; this cannot depend
// on the cron leader having an executing dispatcher. Agent recovery may still
// claim that same answer if the settling executor itself dies.
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

// wakeSettledHumanWait closes the answer-before-settlement race without a
// longer polling window or a second execution claim. The decision event may
// have exhausted PrepareDispatch's bounded wait while an App was still
// stopping. A non-executing cron leader cannot redispatch that run, so waiting
// for its sweep can incorrectly abandon a healthy, already-approved task.
//
// report is reached only after the executor has saved its stopped receipt.
// Re-read under the original actor, matching this exact answer and execution
// generation, then re-offer the ordinary event. Duplicate/late offers still
// compete for the same answer claim and recheck current authority, cancellation,
// running intent and checkpoint bindings before any model or effect executes.
// No decision or permission is written here. A still-unanswered wait does
// nothing; its later decision event already sees the settled receipt.
func (d *workRunDispatcher) wakeSettledHumanWait(ctx context.Context, req work.DispatchRequest, approvalID string) {
	if approvalID == "" {
		return
	}
	// HumanWait can cancel the completed invocation's context. The detached
	// read preserves its actor; HandleRunEvent restores persisted owner authority
	// again for dispatch. This never revives that old model/tool invocation.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	ctx = memql.ContextWithFreshRead(readCtx)
	journal, err := automations.LoadRunJournal(ctx, d.app.engine, req.RunId)
	if err != nil {
		d.app.Logger.Warn("work run dispatch: could not inspect a settled human wait", "component", "work.dispatch", "run", req.RunId, "error", err)
		return
	}
	if journal.Status != "running" || journal.CancelRequested || journal.GoalId == "" ||
		journal.HasRunningStep || !humanResumeReceiptReady(journal) ||
		memql.BareShortId(journal.HumanResumeId) != memql.BareShortId(approvalID) {
		return
	}
	// The newly recorded answer belongs to this stopped generation, not to
	// the older HumanResumeId that may have started it. Never adopt a newer
	// rerun merely because its step key happens to be the same.
	req.HumanResumeId = journal.HumanResumeId
	req.Recovery, req.HumanResumeRecovery = false, false
	if !workRerunServable(req, journal) {
		return
	}
	integration := d.app.lookupWorkIntegration()
	if integration == nil {
		return
	}
	d.app.Logger.Info("work run dispatch: re-offering a settled human answer", "component", "work.dispatch", "run", req.RunId, "approval", approvalID)
	integration.HandleRunEvent(events.Event{Payload: map[string]any{
		"id": journal.RunId,
		"payload": map[string]any{
			"status": journal.Status, "automationName": journal.AutomationName,
			"goalId": journal.GoalId, "ownerUserId": journal.OwnerUserId,
			"triggeredBy": journal.TriggeredBy, "humanResumeId": journal.HumanResumeId,
			"rerun": map[string]any{"requestId": req.RerunRequestId},
		},
	}})
}

func humanResumeReceiptReady(journal *automations.RunJournal) bool {
	if journal.OutcomeRecovery && journal.HumanResumeId != "" && journal.StepStates[journal.FailedStep].Status == "failed" {
		// Reporting recovery reuses the completed execution and its consumed
		// formatting allowance. Requiring a higher version would strand this
		// saved answer before the agent can validate the exact retained stage.
		return true
	}
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
