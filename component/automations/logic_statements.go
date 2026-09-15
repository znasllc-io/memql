package automations

// logic_statements.go -- a logic with a statement body runs on the sequence
// runner an automation's statements run on (epic memql#5370, task
// memql#5372; D14 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// ONE EXECUTION MODEL. The body was compiled at load (the function loader
// keeps it on Function.LogicBody); a call parses the step list once per
// loaded function, binds the caller's args, and runs it through runSequence,
// one statement or many. There is no local short-circuit: an expression
// statement is EvalV1, a construct call is a step, a return is a return.
//
// JOURNAL PARITY. A logic's statements are journaled as an automation's are,
// and where they land depends on how it was called:
//
//   - INSIDE A RUN (a statement of an automation, or of a logic already
//     journaled): as steps of THAT run, keyed `<calling statement's key>/<id>`,
//     through the journal the run writes through (withRunJournal), writing
//     step rows only -- the run row, its heartbeat and its step order are the
//     caller's. A run that is not journaled journals none of its logic.
//     Resume never resumes into a nested key; it re-runs the calling
//     statement, as it always has.
//   - DIRECTLY (a client's call): as a run of its own, `logic:<name>`, but only
//     if it WRITES. Whether a body writes is not known until it does (a
//     builtin, a nested logic, a branch not yet taken), so its journal holds
//     every write -- the run row, each statement's rows -- until the engine
//     reports the logic's first graph write (common.NotifyWrite), then writes
//     them in order and every later one as it comes. A read-only call leaves
//     no row. A logic a statement of it calls is inside its run, so it
//     journals through the same held journal and a write it makes opens the
//     caller's run: one run, however deep the calls. The journal's own writes
//     are masked from the observer (journalContext).

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// statementLogicCache holds each logic's parsed step list, keyed by name and
// checked against the body it was parsed from (a reload replaces the body).
type statementLogicCache struct {
	mu      sync.Mutex
	entries map[string]statementLogicEntry
}

type statementLogicEntry struct {
	body       uintptr
	automation *Automation
}

var statementLogics = &statementLogicCache{entries: map[string]statementLogicEntry{}}

// statementLogic returns the executable form of a logic's compiled body.
func (r *LogicRunner) statementLogic(fnName string, body []map[string]any) (*Automation, error) {
	key := reflect.ValueOf(body).Pointer()
	statementLogics.mu.Lock()
	if e, ok := statementLogics.entries[fnName]; ok && e.body == key {
		statementLogics.mu.Unlock()
		return e.automation, nil
	}
	statementLogics.mu.Unlock()

	data, err := json.Marshal(map[string]any{
		"name":  "logic:" + fnName,
		"steps": body,
	})
	if err != nil {
		return nil, fmt.Errorf("logic %q: encode its body: %w", fnName, err)
	}
	a, err := r.loader.parseJSON(data, "logic:"+fnName)
	if err != nil {
		return nil, fmt.Errorf("logic %q: %w", fnName, err)
	}
	statementLogics.mu.Lock()
	statementLogics.entries[fnName] = statementLogicEntry{body: key, automation: a}
	statementLogics.mu.Unlock()
	return a, nil
}

// RunLogicBody implements memql.LogicRunner (see the file comment).
func (r *LogicRunner) RunLogicBody(ctx context.Context, fnName string, body []map[string]any, args map[string]any) (any, error) {
	if memql.InBeforeWrite(ctx) {
		r = r.WithoutJournal()
	}
	if r.stepRegistry == nil {
		return nil, fmt.Errorf("logic runner has no step registry wired")
	}
	a, err := r.statementLogic(fnName, body)
	if err != nil {
		return nil, err
	}
	evaluator := r.newEvaluatorForLogic(ctx, args)
	evaluator.enterStatements()

	var bus *events.Bus
	if r.engine != nil {
		bus = r.engine.EventBus()
	}
	ex := &Executor{stepRegistry: r.stepRegistry, engine: r.engine, logger: r.logger}
	exec := NewExecution(a.Name, "call")
	// The statements run at the CALLER's origin. executeStep stamps each one
	// from SourceTrusted (the #2800 rule), and a logic has no source trust of
	// its own to lend: a trusted automation's call reaches it at internal
	// origin, so its @serverOnly reads stay reachable, and a client's call
	// stays at client origin, so calling a logic launders nothing.
	exec.SourceTrusted = auth.OriginFromContext(ctx).IsInternal()
	stepCtx := &StepContext{Logger: r.logger, Engine: r.engine, Evaluator: evaluator, EventBus: bus, Execution: exec}
	run := &sequenceRun{stepCtx: stepCtx}
	steps := a.Steps
	// The logic's own list: its steps' keys are their ids (or the caller's
	// key and their ids), whatever list the call was made from.
	ctx = withListKey(ctx, "")

	var held *workJournal
	if caller, inRun := common.RunFromContext(ctx); inRun && strings.TrimSpace(caller.RunId) != "" {
		journal, paired := runJournalFrom(ctx)
		if !paired {
			// A run stamped by something other than an executor (a goal's
			// compile): its rows go through the engine's journal.
			journal = r.logicJournal()
		}
		if r.noJournal {
			journal = nil
		}
		exec.ID = caller.RunId
		steps = keyedUnder(steps, caller.StepKey)
		run.journal, run.rowsOnly = journal, true
	} else if held = r.logicJournal(); held != nil {
		exec.Input, exec.CallerSuppliedPayload = args, true
		openCtx := ctx
		held.hold = &heldWrites{open: func() {
			held.write(openCtx, "createWorkRun", held.openRunArgs(a, exec, nil, events.Cause{}))
		}}
		run.journal, run.orders = held, true
		ctx = common.ContextWithWriteObserver(ctx, func(string, string) { held.release() })
		ctx = withRunJournal(ctx, exec.ID, held)
	}

	out, err := ex.runSequence(ctx, steps, run)
	if held != nil {
		if err != nil {
			exec.Fail(err)
		} else {
			exec.Returned, exec.Output = out.Returned, out.Value
			exec.Complete()
		}
		// Held, and so written only if the run opened.
		held.closeRunRecord(ctx, exec, "")
	}
	if err != nil {
		return nil, err
	}
	if memql.IsAbsent(out.Value) {
		return nil, nil
	}
	return out.Value, nil
}

// WithoutJournal returns a runner whose logic journals nothing, however it is
// called: the dry-run sandbox's (component/automations/steps), whose preview
// must leave no run behind, whatever the logic writes.
func (r *LogicRunner) WithoutJournal() *LogicRunner {
	c := *r
	c.noJournal = true
	return &c
}

// logicJournal is a journal for a logic's statements: through a test's
// recorder (journalExec) or the engine. Nil without either, and for a runner
// WithoutJournal -- every journal method is a no-op on nil.
func (r *LogicRunner) logicJournal() *workJournal {
	if r.noJournal {
		return nil
	}
	if r.journalExec != nil {
		return newWorkJournal(r.journalExec, r.logger)
	}
	if r.engine != nil {
		return newWorkJournal(r.engine, r.logger)
	}
	return nil
}

// keyedUnder returns the steps with their ids under the calling statement's
// key, `<key>/<id>`: each one's key in the caller's run, and the key a logic
// it calls in turn nests under. The originals are shared and left untouched.
func keyedUnder(steps []*Step, key string) []*Step {
	if strings.TrimSpace(key) == "" {
		return steps
	}
	out := make([]*Step, len(steps))
	for i, s := range steps {
		if s == nil {
			continue
		}
		c := *s
		c.ID = key + "/" + s.ID
		out[i] = &c
	}
	return out
}
