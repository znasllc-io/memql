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
// spending its once-only claim. An answer can precede the waiting step receipt.
// A timeout consumes nothing: the durable humanResumeId plus waiting receipt
// lets the ordinary recovery sweep claim that same answer, even while the
// original execution's leased claim still exists.
func (d *workRunDispatcher) PrepareDispatch(ctx context.Context, req work.DispatchRequest) (work.DispatchRequest, bool) {
	if req.RerunRequestId != "" || (!req.Recovery && req.HumanResumeId == "") {
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
			if journal.GoalId == "" || journal.Status != "running" || journal.Rerun != nil || journal.HumanResumeId == "" || journal.StepStates[journal.FailedStep].Status != "waiting" {
				return req, true
			}
			req.HumanResumeId = journal.HumanResumeId
			req.OwnerUserId, req.GoalId, req.AutomationName = journal.OwnerUserId, journal.GoalId, journal.AutomationName
			req.HumanResumeRecovery = !journal.HeartbeatAt.IsZero() && time.Since(journal.HeartbeatAt) >= work.DefaultAbandonedAfterSeconds*time.Second
			req.Recovery = req.HumanResumeRecovery
		}
		if journal.CancelRequested || journal.Status != "running" || req.HumanResumeId != journal.HumanResumeId || journal.GoalId == "" || journal.Rerun != nil {
			return req, false
		}
		if !journal.HasRunningStep && journal.StepStates[journal.FailedStep].Status == "waiting" {
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
