package automations

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/compiler"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// logic_statements_test.go -- a statement-body logic on the sequence runner,
// and where its journal lands (epic memql#5370, task memql#5372). The step
// registry is sequence_test.go's probe; the journal writes into a recorder
// that stands in for the engine and, like the engine, reports every write it
// is handed to the write observer -- so a journal write that counted as the
// logic's own would show here as a run a read-only logic opened.

type journalRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (j *journalRecorder) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	j.mu.Lock()
	j.calls = append(j.calls, query)
	j.mu.Unlock()
	common.NotifyWrite(ctx, "v1:work:journal", "row")
	return memql.NewResultWithOutput(nil), nil
}

func (j *journalRecorder) count(prefix string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for _, c := range j.calls {
		if strings.HasPrefix(c, prefix+"(") {
			n++
		}
	}
	return n
}

func (j *journalRecorder) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.calls...)
}

// keys lists the `key:` of every createWorkStep, in the order written.
func (j *journalRecorder) keys() []string {
	var out []string
	for _, c := range j.all() {
		if !strings.HasPrefix(c, "createWorkStep(") {
			continue
		}
		i := strings.Index(c, `key: "`)
		if i < 0 {
			continue
		}
		rest := c[i+len(`key: "`):]
		out = append(out, rest[:strings.IndexByte(rest, '"')])
	}
	return out
}

// compiledLogic parses and compiles one statement-form logic the way the
// function loader does.
func compiledLogic(t *testing.T, src string) (string, []map[string]any) {
	t.Helper()
	norm, err := languageParser.NormaliseAll(src)
	if err != nil {
		t.Fatal(err)
	}
	file, err := languageParser.ParseFile(norm)
	if err != nil {
		t.Fatal(err)
	}
	fn := file.Definitions[0].(*languageParser.FunctionDef)
	var args []string
	if fn.ArgsSchema != nil {
		for _, f := range fn.ArgsSchema.Fields {
			args = append(args, f.Name)
		}
	}
	body := fn.Body.(*languageParser.AutomationDef).Body
	steps, problems := compiler.CompileBody("logic", fn.Name, args, body)
	if len(problems) > 0 {
		t.Fatalf("compile: %v", problems)
	}
	return fn.Name, steps
}

// logicProbe is the probe, plus what the engine does for two calls: `write`
// reports a graph write, and a call naming a logic in `logics` runs it
// through the runner with the step's context, as a logic statement does.
type logicProbe struct {
	*stmtProbe
	runner *LogicRunner
	logics map[string][]map[string]any
}

func (p logicProbe) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	if step.Function != nil {
		if body, ok := p.logics[step.Function.Name]; ok {
			now := time.Now()
			out, err := p.runner.RunLogicBody(ctx, step.Function.Name, body, nil)
			res := &StepResult{StepId: step.ID, StartedAt: now, CompletedAt: time.Now(), Status: "success", Result: out}
			if err != nil {
				res.Status, res.Error = "failed", err.Error()
			}
			return res, err
		}
		if step.Function.Name == "write" {
			common.NotifyWrite(ctx, "v1:x:thing", "t1")
		}
	}
	return p.stmtProbe.Execute(ctx, step, stepCtx)
}

// newLogicRig is a runner over the probe with its journal in a recorder.
func newLogicRig(logics map[string][]map[string]any) (*LogicRunner, *stmtProbe, *journalRecorder) {
	probe := newStmtProbe()
	rec := &journalRecorder{}
	lp := logicProbe{stmtProbe: probe, logics: logics}
	r := NewLogicRunner(nil, lp, nil)
	r.journalExec = rec
	lp.runner = r
	r.stepRegistry = lp
	return r, probe, rec
}

func TestLogicBodyRunsOnTheSequence(t *testing.T) {
	r, probe, _ := newLogicRig(nil)
	probe.answers["price"] = memql.NewResultWithOutput(int64(40))
	name, body := compiledLogic(t, `logic quote {
  args {
    qty any
  }
  unit := builtin price()
  total := unit * (args.qty ?? 1)
  return total > 100 ? "large" : "small"
}`)
	out, err := r.RunLogicBody(context.Background(), name, body, map[string]any{"qty": int64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if out != "large" {
		t.Fatalf("out = %v, want large (40 * 3 > 100)", out)
	}
	if again, _ := r.RunLogicBody(context.Background(), name, body, map[string]any{"qty": int64(1)}); again != "small" {
		t.Fatalf("second call = %v: the parsed body is cached, the names are not", again)
	}
}

func TestSingleStatementLogicRunsOnTheSequence(t *testing.T) {
	r, probe, _ := newLogicRig(nil)
	rows := []map[string]any{{"id": "v1:x:item:1", "payload": map[string]any{"n": int64(1)}}}
	probe.answers["items"] = rowsResult(rows...)
	name, body := compiledLogic(t, `logic listItems {
  return query items()
}`)
	out, err := r.RunLogicBody(context.Background(), name, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, []any{rows[0]}) {
		t.Fatalf("out = %#v, want the rows as the maps they came as", out)
	}
}

func TestLogicInsideARunJournalsAsItsSteps(t *testing.T) {
	r, _, rec := newLogicRig(nil)
	name, body := compiledLogic(t, `logic decide {
  a := builtin readA()
  builtin write()
  return a
}`)
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "run-1", StepKey: "decide", Mode: common.RunModeLive})
	if _, err := r.RunLogicBody(ctx, name, body, nil); err != nil {
		t.Fatal(err)
	}
	if n := rec.count("createWorkRun"); n != 0 {
		t.Fatalf("a logic inside a run opened %d run(s) of its own", n)
	}
	if n := rec.count("updateWorkRun"); n != 0 {
		t.Fatalf("a logic inside a run wrote the caller's run row %d time(s); the run row is the caller's", n)
	}
	if got, want := rec.keys(), []string{"decide/a", "decide/write", "decide/return"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("step rows %v, want %v: every statement, under the calling statement's key", got, want)
	}
	if n := rec.count("updateWorkStep"); n != 3 {
		t.Fatalf("%d receipts, want one per statement", n)
	}
	if joined := strings.Join(rec.all(), "\n"); strings.Count(joined, `runId: "run-1"`) != 3 {
		t.Errorf("the rows are not all on the caller's run:\n%s", joined)
	}
}

func TestLogicInsideAnUnjournaledRunJournalsNothing(t *testing.T) {
	// A run whose executor pairs no journal (its trigger reacts to work rows):
	// the logic's rows would re-fire it.
	r, _, rec := newLogicRig(nil)
	name, body := compiledLogic(t, `logic decide {
  builtin write()
  return 1
}`)
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "run-1", StepKey: "decide", Mode: common.RunModeLive})
	ctx = withRunJournal(ctx, "run-1", nil)
	if _, err := r.RunLogicBody(ctx, name, body, nil); err != nil {
		t.Fatal(err)
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Fatalf("a logic inside an unjournaled run wrote %d journal row(s): %v", len(calls), calls)
	}
}

func TestReadOnlyLogicLeavesNoRun(t *testing.T) {
	r, _, rec := newLogicRig(nil)
	name, body := compiledLogic(t, `logic readOnly {
  a := builtin readA()
  b := a ?? "none"
  return b
}`)
	if _, err := r.RunLogicBody(context.Background(), name, body, nil); err != nil {
		t.Fatal(err)
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Fatalf("a read-only logic wrote %d journal row(s): %v", len(calls), calls)
	}
}

func TestDirectWritingLogicLeavesARun(t *testing.T) {
	r, probe, rec := newLogicRig(nil)
	probe.answers["readB"] = memql.NewResultWithOutput("b")
	name, body := compiledLogic(t, `logic writes {
  a := builtin readA()
  builtin write()
  b := builtin readB()
  return b
}`)
	out, err := r.RunLogicBody(context.Background(), name, body, nil)
	if err != nil || out != "b" {
		t.Fatalf("out %v, err %v", out, err)
	}
	calls := rec.all()
	if n := rec.count("createWorkRun"); n != 1 {
		t.Fatalf("createWorkRun x%d, want exactly one -- the journal's own writes must not open another:\n%s", n, strings.Join(calls, "\n"))
	}
	if !strings.HasPrefix(calls[0], "createWorkRun(") || !strings.Contains(calls[0], `automationName: "logic:writes"`) {
		t.Fatalf("the first journal write is %q, want the logic's run", calls[0])
	}
	if got, want := rec.keys(), []string{"a", "write", "b", "return"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("step rows %v, want %v: the statements before the first write too", got, want)
	}
	last := calls[len(calls)-1]
	if !strings.HasPrefix(last, "updateWorkRun(") || !strings.Contains(last, `status: "succeeded"`) || !strings.Contains(last, `"returned":"b"`) {
		t.Fatalf("the last journal write is %q, want the run closed as succeeded with what it returned", last)
	}
	if strings.Contains(strings.Join(calls, "\n"), "waiting") {
		t.Fatal("a logic's run went through the failure path")
	}
}

func TestDirectFailingLogicClosesItsRunFailed(t *testing.T) {
	// The probe's error names its callee, so `eof` fails with a message the
	// failure path's rules call transient: an automation's run would park on a
	// retry. A logic's caller already has the error, and nothing resumes a
	// logic's run, so it closes failed.
	r, probe, rec := newLogicRig(nil)
	probe.fails["eof"] = -1
	name, body := compiledLogic(t, `logic fails {
  builtin write()
  builtin eof()
  return 1
}`)
	if _, err := r.RunLogicBody(context.Background(), name, body, nil); err == nil {
		t.Fatal("the failure did not reach the caller")
	}
	calls := rec.all()
	for _, c := range calls {
		if strings.Contains(c, `"waiting"`) || strings.Contains(c, "WorkObservation") {
			t.Fatalf("a logic's run went through the failure path: %s", c)
		}
	}
	last := calls[len(calls)-1]
	if !strings.HasPrefix(last, "updateWorkRun(") || !strings.Contains(last, `status: "failed"`) {
		t.Fatalf("the last journal write is %q, want the run closed failed", last)
	}
}

func TestANestedLogicsWriteOpensItsCallersRun(t *testing.T) {
	// A directly called logic whose second statement calls a logic that
	// writes. The inner logic is inside the outer's run -- unopened when it
	// starts -- so its rows are held with the outer's and its write opens the
	// one run.
	_, inner := compiledLogic(t, `logic inner {
  x := builtin readX()
  builtin write()
  return x
}`)
	r, _, rec := newLogicRig(map[string][]map[string]any{"inner": inner})
	name, outer := compiledLogic(t, `logic outer {
  a := builtin readA()
  r := logic inner()
  return r
}`)
	if _, err := r.RunLogicBody(context.Background(), name, outer, nil); err != nil {
		t.Fatal(err)
	}
	if n := rec.count("createWorkRun"); n != 1 {
		t.Fatalf("createWorkRun x%d, want the outer logic's one run:\n%s", n, strings.Join(rec.all(), "\n"))
	}
	if !strings.Contains(rec.all()[0], `automationName: "logic:outer"`) {
		t.Fatalf("the run is %q, want the outer logic's", rec.all()[0])
	}
	want := []string{"a", "r", "r/x", "r/write", "r/return", "return"}
	if got := rec.keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("step rows %v, want %v", got, want)
	}
}

func TestAnAutomationsLogicJournalsInItsRun(t *testing.T) {
	// An automation whose statement calls a writing logic: the logic's
	// statements are rows of the automation's run, keyed under the calling
	// statement, and there is no second run.
	_, decide := compiledLogic(t, `logic decide {
  a := builtin readA()
  builtin write()
  return a
}`)
	r, _, rec := newLogicRig(map[string][]map[string]any{"decide": decide})
	a := statementAutomation(t, `@trigger(event="probe.fired")
automation applies {
  verdict := logic decide()
  builtin after(v: verdict)
}`)
	e := NewExecutor(ExecutorOptions{StepRegistry: r.stepRegistry})
	e.journal = newWorkJournal(rec, nil)
	ev := events.NewEvent("probe.fired", events.KindMessage, nil)
	exec, err := e.ExecuteWithEvent(context.Background(), a, "test", &ev)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	if n := rec.count("createWorkRun"); n != 1 {
		t.Fatalf("createWorkRun x%d, want the automation's one run:\n%s", n, strings.Join(rec.all(), "\n"))
	}
	want := []string{"verdict", "verdict/a", "verdict/write", "verdict/return", "after"}
	if got := rec.keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("step rows %v, want %v", got, want)
	}
	if strings.Count(strings.Join(rec.all(), "\n"), "runId: \""+exec.ID+"\"") < len(want) {
		t.Fatalf("the logic's rows are not on the automation's run %s:\n%s", exec.ID, strings.Join(rec.all(), "\n"))
	}
}

func TestAnUnjournaledAutomationsLogicJournalsNothing(t *testing.T) {
	// An automation that reacts to work rows is not journaled -- its rows
	// would re-fire it -- and neither is a logic its statements call.
	_, decide := compiledLogic(t, `logic decide {
  builtin write()
  return 1
}`)
	r, _, rec := newLogicRig(map[string][]map[string]any{"decide": decide})
	a := statementAutomation(t, `@trigger(event="probe.fired")
automation reacts {
  verdict := logic decide()
  builtin after(v: verdict)
}`)
	a.Trigger.Event = "graph.node.created.v1:work:step"
	if !journalSkipsAutomation(a) {
		t.Fatal("the fixture must be an automation the journal skips")
	}
	e := NewExecutor(ExecutorOptions{StepRegistry: r.stepRegistry})
	e.journal = newWorkJournal(rec, nil)
	ev := events.NewEvent("graph.node.created.v1:work:step", events.KindMessage, nil)
	if exec, err := e.ExecuteWithEvent(context.Background(), a, "test", &ev); err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Fatalf("an unjournaled automation's logic wrote %d journal row(s): %v", len(calls), calls)
	}
}

func TestALogicRunWithoutJournalLeavesNoRow(t *testing.T) {
	// The dry-run sandbox's runner: a preview leaves no run, even where a
	// write gets past the sandbox -- directly called or inside a run.
	name, body := compiledLogic(t, `logic writes {
  builtin write()
  return 1
}`)
	for _, ctx := range []context.Context{
		context.Background(),
		common.ContextWithRun(context.Background(), common.RunContext{RunId: "run-1", StepKey: "s", Mode: common.RunModeLive}),
	} {
		r, _, rec := newLogicRig(nil)
		if _, err := r.WithoutJournal().RunLogicBody(ctx, name, body, nil); err != nil {
			t.Fatal(err)
		}
		if calls := rec.all(); len(calls) != 0 {
			t.Fatalf("a runner without a journal wrote %d row(s): %v", len(calls), calls)
		}
	}
}

func TestJournalWritesAreNotTheRunsWrites(t *testing.T) {
	// Were they, the first write of a held journal's flush would re-enter the
	// release that is flushing it.
	seen := 0
	ctx := common.ContextWithWriteObserver(context.Background(), func(string, string) { seen++ })
	j := newWorkJournal(&journalRecorder{}, nil)
	j.write(ctx, "updateWorkRun", map[string]any{"runId": "r1"})
	if seen != 0 {
		t.Fatal("a journal write reached the write observer")
	}
	owned := common.ContextWithRun(ctx, common.RunContext{RunId: "r1", OwnerUserId: "u1"})
	j.write(owned, "updateWorkRun", map[string]any{"runId": "r1"})
	if seen != 0 {
		t.Fatal("an owned run's journal write reached the write observer")
	}
}

func TestANestedReadOnlyLogicLeavesNoRun(t *testing.T) {
	_, inner := compiledLogic(t, `logic inner {
  return builtin readX()
}`)
	r, _, rec := newLogicRig(map[string][]map[string]any{"inner": inner})
	name, outer := compiledLogic(t, `logic outer {
  r := logic inner()
  return r
}`)
	if _, err := r.RunLogicBody(context.Background(), name, outer, nil); err != nil {
		t.Fatal(err)
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Fatalf("two read-only logics wrote %d journal row(s): %v", len(calls), calls)
	}
}
