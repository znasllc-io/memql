package automations

// loop_runtime.go -- the loop protection at run time (epic memql#5380; D-A and
// D-C of the loop protection plan).
//
// Every run carries a cause (component/events/cause.go): the run whose work
// published the event that fired it, the chain of runs back to the root cause,
// and how deep that chain is. The executor reads the parent cause when a fire
// arrives and stamps the run's own -- one deeper -- on the context its steps
// run under. Every publisher that has the context stamps it on what it
// publishes, so the fire a run's write causes is one deeper again.
//
// A fire whose chain would pass the cap (MEMQL_AUTOMATION_MAX_CHAIN_DEPTH,
// default 16) is REFUSED, and so is a fire of an @loop automation whose chain
// already holds as many runs of it as its maxDepth allows. A refusal is not a
// skip: it is a run failure with errorCode loop_depth_exceeded, recorded as
// one v1:work:run row that names the chain (journal_loop.go), logged at WARN
// with the chain, and counted on memql_automation_loops_stopped_total.
//
// THREE PLACEMENTS ARE LOAD-BEARING.
//
//   - The decision is taken at the top of the run and ACTED ON after the dedup
//     gates and the cluster guard's claim, before the journal opens: one event
//     that reaches two replicas is refused once and recorded once, and a
//     refused run opens no row beyond its refusal.
//   - A refused fire runs under its PARENT's cause, not its own. It never ran,
//     so nothing published on its behalf -- its automation.started, the events
//     its refusal row causes -- may carry a chain past the cap. That is what
//     keeps Cause.Chain's promise that no event names more runs than the cap.
//   - The refusal never reaches closeRun, which classifies a failure before it
//     records one and can park it on a retry. A loop is terminal by
//     construction: another attempt runs the same chain into the same bound.
//
// WHAT COUNTS AS A RUN. An event-triggered run is one, and so is a
// sub-automation call: its parent is the calling run, read off the context,
// because the synthetic invocation event the scheduler builds carries no cause
// of its own. A logic call is a statement of its run and does not count. A
// resumed run is the same run continuing: it keeps the parent its first attempt
// recorded (openRun's triggerEvent.cause) and runs at the same depth.
//
// WHERE THE CHAIN BREAKS (D-B), and breaks rather than falsely stops: a Go
// bus.Subscribe handler that writes, the durable run-delivery substrate, an
// async sub-automation and an external round trip all start a new root. D-F's
// per-(automation, row) budget is the backstop for a loop that escapes that
// way.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/component/work"
)

// LoopRefusal is a run the loop protection stopped before it started
// (D19, D-C): loop_depth_exceeded, carrying the chain.
type LoopRefusal struct {
	Reason     string       // metrics.LoopStopDepth | metrics.LoopStopLoopBound
	Automation string       // the automation that would have run
	Depth      int          // the depth the refused run would have had
	Cap        int          // the bound it exceeded
	Cause      events.Cause // the refused run's cause, chain included
}

// Error is the refusal in one line: the automation, the depth and the bound it
// passed, and the chain that led here, ending with the rule id the corpus
// reads -- "loop_depth_exceeded: <automation> would run at depth 17, past the
// cap of 16; chain: a (run-1) -> b (run-2) -> ... [loop_depth_exceeded]". It
// is the errorMessage the refused run's row carries.
func (r *LoopRefusal) Error() string {
	if r == nil {
		return work.TerminalLoopDepthExceeded
	}
	bound := fmt.Sprintf("past the cap of %d", r.Cap)
	if r.Reason == metrics.LoopStopLoopBound {
		bound = fmt.Sprintf("past its @loop maxDepth of %d runs in one chain", r.Cap)
	}
	return fmt.Sprintf("%s: %s would run at depth %d, %s; chain: %s [%s]",
		work.TerminalLoopDepthExceeded, r.Automation, r.Depth, bound, chainText(r.PriorRuns()), work.TerminalLoopDepthExceeded)
}

// PriorRuns is the chain that led to the refusal, oldest first: the refused
// run's own chain without the refused run, which never ran. It is the chain
// the refused run's row records and the one its message names.
func (r *LoopRefusal) PriorRuns() []events.Link {
	if r == nil {
		return nil
	}
	return append([]events.Link{}, priorRuns(r.Cause)...)
}

// priorRuns is a run's chain without the run itself: the parent's chain. A
// cause is built by Next, whose last link is the run the cause belongs to.
func priorRuns(run events.Cause) []events.Link {
	chain := run.Chain
	if n := len(chain); n > 0 && chain[n-1].RunId == run.CausationId {
		chain = chain[:n-1]
	}
	return chain
}

// chainText renders a chain the way a person reads it: a (run-1) -> b (run-2).
func chainText(chain []events.Link) string {
	if len(chain) == 0 {
		return "(no run recorded)"
	}
	parts := make([]string, len(chain))
	for i, l := range chain {
		parts[i] = l.Automation + " (" + l.RunId + ")"
	}
	return strings.Join(parts, " -> ")
}

// rootCorrelation is a root event's correlation id: deterministic, so every
// replica derives the same one from the same event (D-A).
//
// It is the event's own fingerprint -- its topic, its kind and its whole
// payload, never its Timestamp, for eventFingerprintData's wall-clock reason --
// so two writes of one row are two roots: a person toggling a status twice
// starts two chains, not one.
func rootCorrelation(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	fp, err := fingerprintEngine.FromMap(map[string]any{
		"topic":   ev.Topic,
		"kind":    ev.Kind.String(),
		"payload": ev.Payload,
	})
	if err != nil {
		// The payload does not encode as JSON (a channel, a func, a NaN). The
		// id must still be one every replica derives, so it is the event's
		// topic and kind -- all of the event that can be named -- rather than
		// anything drawn at random.
		fp = fingerprintEngine.MustFromMap(map[string]any{"topic": ev.Topic, "kind": ev.Kind.String()})
	}
	return "evt-" + string(fp)
}

// runCause is the cause a run carries, and its parent: the triggering event's
// cause, else the context's (a sub-automation), else none.
//
// The correlation is the parent's; a root has none, so the run takes the root
// event's own (rootCorrelation), or -- with no event either, a cron or manual
// run -- its own run id.
func runCause(ctx context.Context, automation *Automation, runId string, ev *events.Event) (parent, run events.Cause) {
	if ev != nil && !ev.Cause.IsZero() {
		parent = ev.Cause.Clone()
	} else if c, ok := events.CauseFromContext(ctx); ok {
		parent = c
	}
	correlation := parent.CorrelationId
	if correlation == "" {
		if ev != nil {
			correlation = rootCorrelation(ev)
		} else {
			correlation = runId
		}
	}
	name := ""
	if automation != nil {
		name = automation.Name
	}
	return parent, parent.Next(name, runId, correlation)
}

// loopBound refuses a run whose depth passes the cap, or whose @loop allows no
// further run of itself in this chain.
//
// The @loop bound counts the runs of this automation the PARENT's chain holds:
// maxDepth=N admits N runs of it in one chain, and refuses the next.
func loopBound(automation *Automation, run events.Cause, cap int) *LoopRefusal {
	if automation == nil {
		return nil
	}
	if run.Depth > cap {
		return &LoopRefusal{Reason: metrics.LoopStopDepth, Automation: automation.Name, Depth: run.Depth, Cap: cap, Cause: run.Clone()}
	}
	// Nil-checked: an automation built for the LogicRunner is never prepared
	// and never carries a @loop, and a maxDepth below 1 is refused at load --
	// the cap above still bounds whatever reaches here.
	if loop := automation.Loop; loop != nil && loop.MaxDepth > 0 {
		held := 0
		for _, l := range priorRuns(run) {
			if l.Automation == automation.Name {
				held++
			}
		}
		if held >= loop.MaxDepth {
			return &LoopRefusal{Reason: metrics.LoopStopLoopBound, Automation: automation.Name, Depth: run.Depth, Cap: loop.MaxDepth, Cause: run.Clone()}
		}
	}
	return nil
}

// contextWithRunCause is the context a fire runs under: its own cause when it
// is admitted, so every write and publish of the run is one deeper; its
// parent's when the loop bound refused it, because a run that never ran
// extends no chain.
func contextWithRunCause(ctx context.Context, parent, run events.Cause, refusal *LoopRefusal) context.Context {
	if refusal != nil {
		return events.ContextWithCause(ctx, parent)
	}
	return events.ContextWithCause(ctx, run)
}

// stopLoop acts on a refusal, after the dedup gates: it logs the stop at WARN
// with the chain, counts it, records the refused run -- one row at failed, or
// none for an automation the journal skips, where the WARN is the record --
// and returns the refusal as the run's failure.
//
// adopted is an execution onto a run row that already exists (adopt.go). The
// refusal closes that row rather than opening it a second time, which would
// overwrite what its creator wrote.
func (e *Executor) stopLoop(ctx context.Context, automation *Automation, exec *AutomationExecution, ev *events.Event, parent events.Cause, r *LoopRefusal, adopted bool) (*AutomationExecution, error) {
	if e.logger != nil {
		e.logger.Warn("automation fire refused: its chain of runs passed the loop bound",
			"component", ComponentName,
			"automation", automation.Name,
			"runId", exec.ID,
			"reason", r.Reason,
			"depth", r.Depth,
			"cap", r.Cap,
			"correlationId", r.Cause.CorrelationId,
			"chain", chainText(r.PriorRuns()),
		)
	}
	metrics.AutomationLoopStopped(automation.Name, r.Reason)

	exec.Status = "failed"
	exec.Error = r.Error()
	exec.ErrorValue = r
	exec.CompletedAt = time.Now()
	exec.Duration = exec.CompletedAt.Sub(exec.StartedAt)

	if !journalSkipsAutomation(automation) {
		if adopted {
			e.journal.closeLoopRefusal(ctx, exec, r)
		} else {
			e.journal.refuseLoop(ctx, automation, exec, ev, parent, r)
		}
	}
	return exec, r
}

// journalTriggerEvent rebuilds the triggering event a run recorded on its row
// -- openRun's triggerEvent: topic, kind, payload and, for a run in a chain,
// the parent cause -- or nil when it recorded none (a cron or manual run).
// Resume and a recovery that adopts the row run from it, so the run keeps its
// place in its chain.
func journalTriggerEvent(m map[string]any) *events.Event {
	if m == nil {
		return nil
	}
	topic, _ := m["topic"].(string)
	kind, _ := m["kind"].(string)
	payload, _ := m["payload"].(map[string]any)
	cause, _ := m["cause"].(map[string]any)
	return &events.Event{Topic: topic, Kind: kindNamed(kind), Payload: payload, Cause: events.CauseFromMap(cause)}
}

// journalRunCause is the cause a journaled run carries when it runs again on
// its own run id: rebuilt from what its first attempt recorded, so it is the
// same parent, the same correlation and the same depth.
func journalRunCause(ctx context.Context, automation *Automation, runId string, triggerEvent map[string]any) events.Cause {
	_, run := runCause(ctx, automation, runId, journalTriggerEvent(triggerEvent))
	return run
}

// kindScanLimit bounds kindNamed's walk: well past the last events.Kind, whose
// String() names nothing beyond the declared set.
const kindScanLimit = 256

// kindNamed is the events.Kind whose String() the row recorded, so a rebuilt
// root event derives the correlation its first attempt did. A name no kind
// answers to is the unspecified kind.
func kindNamed(name string) events.Kind {
	for k := events.Kind(0); k < kindScanLimit; k++ {
		if k.String() == name {
			return k
		}
	}
	return events.KindUnspecified
}
