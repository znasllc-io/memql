package app

import (
	"github.com/znasllc-io/memql/component/automations"
	work "github.com/znasllc-io/memql/integrations/work"
	"time"
)

func workRunCanStart(req work.DispatchRequest, j *automations.RunJournal, now time.Time) bool {
	// A run its driver owns -- a Go-written journal, a procedure's replay --
	// is never this dispatcher's (work.IsDriverOwnedRun). Asked of the STORED
	// row and not only of the request: an id-only event carries no
	// triggeredBy, so this read is the one place left that can see it.
	if work.IsDriverOwnedRun(j.TriggeredBy) {
		return false
	}
	// A renewable heartbeat fences the current executor after the arbitration
	// lease expires. Failed steps have no live intent and remain resumable.
	if j.Status == "running" && j.HasRunningStep && !j.HeartbeatAt.IsZero() && now.Sub(j.HeartbeatAt) < work.DefaultAbandonedAfterSeconds*time.Second {
		return false
	}
	return req.CanDispatchStoredRun(j.GoalId, j.Status, j.WaitingOn, now)
}
