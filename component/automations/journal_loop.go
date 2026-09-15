package automations

// journal_loop.go -- the row a run the loop protection refused leaves behind
// (epic memql#5380, D-C of the loop protection plan). See loop_runtime.go for
// when a run is refused.

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/work"
)

// refuseLoop records a run the loop protection refused before it started: ONE
// v1:work:run row, opened and closed at failed in the same breath, with the
// chain on it.
//
// IT DOES NOT GO THROUGH closeRun, and that is why it is a writer of its own.
// closeRun classifies a failure before it records one -- the symptom table,
// perhaps a model call -- and can land the run at `waiting` on a retry. A loop
// is terminal by construction: a retry runs the same chain into the same
// bound. So the refusal writes its terminal state itself, and the classifier
// never sees it.
//
// The row's triggerEvent carries the PARENT cause (openRun), which a resume
// or a recovery reads back; outcome.loop carries the chain that led here.
func (j *workJournal) refuseLoop(ctx context.Context, automation *Automation, exec *AutomationExecution, ev *events.Event, parent events.Cause, r *LoopRefusal) {
	if j == nil || automation == nil || exec == nil || r == nil {
		return
	}
	j.openRun(ctx, automation, exec, ev, parent)
	j.closeLoopRefusal(ctx, exec, r)
}

// closeLoopRefusal writes the refused run's terminal version. It is the whole
// of the record for an ADOPTED run, whose row its creator already wrote: a
// second createWorkRun would replace that row's goal, variables and template
// with the little an execution knows.
func (j *workJournal) closeLoopRefusal(ctx context.Context, exec *AutomationExecution, r *LoopRefusal) {
	if j == nil || exec == nil || r == nil {
		return
	}
	finished := exec.CompletedAt
	if finished.IsZero() {
		finished = time.Now()
	}
	j.call(ctx, "updateWorkRun", map[string]any{
		"runId":        exec.ID,
		"status":       "failed",
		"finishedAt":   rfc3339(finished),
		"errorCode":    work.TerminalLoopDepthExceeded,
		"errorMessage": r.Error(),
		"outcome": map[string]any{
			"executorStatus": "failed",
			"loop":           loopRecord(r),
		},
	})
}

// loopRecord is outcome.loop: which bound stopped the run, how deep it would
// have run, and the chain of runs that led to it, oldest first -- without the
// refused run, which never ran and is the row's own automationName. The OS's
// loop-stops read projects exactly these fields.
func loopRecord(r *LoopRefusal) map[string]any {
	return map[string]any{
		"reason":        r.Reason,
		"depth":         r.Depth,
		"cap":           r.Cap,
		"correlationId": r.Cause.CorrelationId,
		"chain":         r.PriorRuns(),
	}
}
