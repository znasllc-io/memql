package automations

// cancel.go -- honouring `v1:work:run.cancelRequested` at a step boundary
// (memql#5066).
//
// ===========================================================================
// THE FIELD HAD NO READER
// ===========================================================================
// `cancelRequested` has been on the run concept since the work spine landed,
// and `integrations/work/goal.go` writes it -- so the graph, the builtin and
// every surface that offers "stop this run" were complete. Nothing read it.
// A person asking a run to stop got a flag set on a row and a run that kept
// going to completion, with no error anywhere: exactly the shape of the
// kill-switch automation memql#5066 is about, which was documented-and-inert
// for the same reason.
//
// It is worth being precise about what this does and does not stop. It ends
// the run at the NEXT STEP BOUNDARY. A step already in flight finishes and is
// journaled rather than being abandoned mid-effect, which is the right
// granularity and the one the design record names: a half-written side effect
// with no receipt is worse than one more completed step.
//
// ===========================================================================
// WHY IT IS POLLED ON A CLOCK AND NOT AT EVERY BOUNDARY
// ===========================================================================
// The obvious implementation reads the run row before each step. That is one
// extra query per step forever, on every automation in the cluster, to serve a
// flag that is almost never set -- and automation steps are frequently
// sub-millisecond, so the read would often cost more than the step.
//
// So it is asked at most once per CancelPollInterval, and ALWAYS at the first
// boundary. The first check matters on its own: a run whose cancel arrived
// while it was queued, or one adopted by resume after a cancel, must not run a
// single step. After that the guarantee is "within one interval plus the
// current step", which is what a person clicking stop can actually observe.
//
// The interval is deliberately not configurable per automation. A knob here
// would let one template opt out of a control that exists to bound damage.

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// CancelPollInterval bounds how often a run asks whether it has been
// cancelled. Two seconds is under human reaction time for a stop button and
// far above the rate at which steps complete, so it costs a handful of reads
// on any run long enough for anyone to want to stop it.
const CancelPollInterval = 2 * time.Second

// cancelPoller tracks when a run last asked. The zero value asks immediately,
// which is the first-boundary rule above.
type cancelPoller struct {
	last     time.Time
	interval time.Duration
}

func newCancelPoller(interval time.Duration) *cancelPoller {
	if interval <= 0 {
		interval = CancelPollInterval
	}
	return &cancelPoller{interval: interval}
}

// due reports whether the flag should be read now, and records the ask.
func (p *cancelPoller) due(now time.Time) bool {
	if p == nil {
		return false
	}
	if !p.last.IsZero() && now.Sub(p.last) < p.interval {
		return false
	}
	p.last = now
	return true
}

// cancelRequested reads the run's flag.
//
// IT FAILS OPEN, and that is the deliberate direction. A read error means the
// executor could not reach the journal, which is a database problem; treating
// it as "cancelled" would stop every running automation in the cluster the
// moment a query hiccupped. The opposite failure -- a cancel that lands one
// interval late because a read failed -- costs one more step.
//
// A run that is not journaled (a sandboxed dry-run, or an automation that
// reacts to work rows) has no row to read and no cancel to honour, which is
// why this hangs off the journal rather than off the executor: a nil journal
// answers false with no query.
// It answers WHO asked as well, off the same row rather than with a second
// read: "cancelled" with no author reads as a fault rather than as somebody's
// decision, which is the distinction v1:platform:packageDeployment.status
// spells out at length and this row inherits.
func (j *workJournal) cancelRequested(ctx context.Context, runId string) (asked bool, by string) {
	if j == nil || runId == "" {
		return false, ""
	}
	call, err := journalArgs("workRunById", map[string]any{"runId": runId})
	if err != nil {
		j.warn("workRunById", err)
		return false, ""
	}
	res, err := j.exec.Execute(journalContext(ctx), "query "+call)
	if err != nil {
		j.warn("workRunById", err)
		return false, ""
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return false, ""
	}
	asked, _ = rows[0]["cancelRequested"].(bool)
	by, _ = rows[0]["cancelledBy"].(string)
	return asked, by
}

// cancelStop closes the run as a person's decision rather than as a fault.
func (j *workJournal) cancelStop(ctx context.Context, exec *AutomationExecution, chainHead, by string) {
	if j == nil || exec == nil {
		return
	}
	exec.Cancel()
	if exec.Error == "" {
		exec.Error = "cancelled: the run was asked to stop"
	}
	args := map[string]any{
		"runId":      exec.ID,
		"status":     "cancelled",
		"finishedAt": rfc3339(exec.CompletedAt),
		"chainHead":  chainHead,
		"stepOrder":  exec.StepOrder,
		"outcome": map[string]any{
			"executorStatus": exec.Status,
			// The reason is on the row rather than only in a log line: the run
			// rail is where a person looks to find out why their work stopped,
			// and "cancelled" with no author reads as a fault.
			"cancelledAtStepBoundary": true,
		},
		"errorMessage": exec.Error,
	}
	if by != "" {
		args["cancelledBy"] = by
	}
	closeHeadArgs(exec, args)
	j.call(ctx, "updateWorkRun", args)
}
