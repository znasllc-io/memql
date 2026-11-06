package automations

// resume_v1_db_test.go -- a statement body failed, journaled to Postgres,
// read back and resumed (epic memql#5370, task memql#5372).
//
// resume_statements_test.go resumes over a journal built by hand. This one
// resumes over the journal the run actually wrote, read back through
// LoadRunJournal: the recorded value of each statement has to survive the
// engine's step row, the continued failure has to be told apart from the
// stop in whatever order the rows come back, and a logic's rows under the
// calling statement's key must not become the resume point.
//
// Postgres-gated: skips cleanly when no DB is reachable, FAILS under
// MEMQL_REQUIRE_DB=1.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// resumeDBProbe is the probe, with a logic call run through the engine the
// way a logic statement runs one.
type resumeDBProbe struct {
	*stmtProbe
	engine *memql.MemQLEngine
	logics map[string]bool
}

func (p resumeDBProbe) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	if step.Function != nil && p.logics[step.Function.Name] {
		now := time.Now()
		out, err := p.engine.Execute(ctx, step.Function.Name+"()")
		res := &StepResult{StepId: step.ID, StartedAt: now, CompletedAt: time.Now(), Status: "success", Result: out}
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
		}
		return res, err
	}
	return p.stmtProbe.Execute(ctx, step, stepCtx)
}

// runThenResume fires src once through an executor journaling to the engine
// and, if the run failed, resumes it from the journal it wrote.
func runThenResume(t *testing.T, engine *memql.MemQLEngine, probe resumeDBProbe, src string) (first, resumed *AutomationExecution) {
	t.Helper()
	a := statementAutomation(t, src)
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: probe})
	t.Cleanup(e.Close)
	ev := events.NewEvent("probe.fired", events.KindMessage, nil)
	first, _ = e.ExecuteWithEvent(context.Background(), a, "test", &ev)
	if first == nil || first.Status != "failed" {
		t.Fatalf("the first run did not fail: %+v", first)
	}
	journal, err := LoadRunJournal(context.Background(), engine, first.ID)
	if err != nil {
		t.Fatalf("LoadRunJournal: %v", err)
	}
	resumed, err = e.ResumeFrom(context.Background(), journal, a, &ResumeOptions{AllowSideEffects: true})
	if err != nil {
		t.Fatalf("ResumeFrom: %v", err)
	}
	return first, resumed
}

// jsonShape is v as it reads after a trip through JSON, the way a row stores
// it: the comparison of what an uninterrupted run and a resumed one send.
func jsonShape(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestResumeV1_DB_FinishesAsAnUninterruptedRunWould(t *testing.T) {
	engine := sharedJournalEngine(t)
	src := fmt.Sprintf(`@trigger(event="probe.fired")
automation fourthFails%d {
  a := builtin one()
  b := builtin two(x: a)
  c := b.n * 10
  builtin record(a: a, c: c, b: b)
  builtin record2(c: c)
}`, time.Now().UnixNano())
	answers := func(p *stmtProbe) {
		p.answers["one"] = memql.NewResultWithOutput("A")
		p.answers["two"] = memql.NewResultWithOutput(map[string]any{"n": int64(2)})
	}

	// The run nothing interrupts.
	whole := newStmtProbe()
	answers(whole)
	a := statementAutomation(t, src)
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: resumeDBProbe{stmtProbe: whole, engine: engine}})
	defer e.Close()
	ev := events.NewEvent("probe.fired", events.KindMessage, nil)
	if exec, err := e.ExecuteWithEvent(context.Background(), a, "test", &ev); err != nil {
		t.Fatalf("uninterrupted run: %v (%s)", err, exec.Error)
	}

	// The run that fails at its fourth statement, then resumes.
	broken := newStmtProbe()
	answers(broken)
	broken.fails["record"] = 1
	_, resumed := runThenResume(t, engine, resumeDBProbe{stmtProbe: broken, engine: engine}, src)
	if resumed.Status != "completed" {
		t.Fatalf("resumed run status %q (%s)", resumed.Status, resumed.Error)
	}
	if got, want := broken.callees(), []string{"one", "two", "record", "record", "record2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v: what finished does not run again", got, want)
	}
	for _, callee := range []string{"record", "record2"} {
		n := 0
		if callee == "record" {
			n = 1 // the resumed attempt
		}
		want, got := jsonShape(t, whole.argsOf(t, callee, 0)), jsonShape(t, broken.argsOf(t, callee, n))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s got %v after the resume, %v in the uninterrupted run", callee, got, want)
		}
	}
}

func TestResumeV1_DB_DoesNotRerunAContinuedFailure(t *testing.T) {
	engine := sharedJournalEngine(t)
	probe := newStmtProbe()
	probe.answers["one"] = memql.NewResultWithOutput("A")
	probe.fails["flaky"] = -1
	probe.fails["boom"] = 1
	_, resumed := runThenResume(t, engine, resumeDBProbe{stmtProbe: probe, engine: engine}, fmt.Sprintf(`@trigger(event="probe.fired")
automation continuesPast%d {
  builtin flaky() on error continue
  a := builtin one()
  builtin boom()
  builtin after(a: a)
}`, time.Now().UnixNano()))
	if resumed.Status != "completed" {
		t.Fatalf("resumed run status %q (%s)", resumed.Status, resumed.Error)
	}
	if got, want := probe.callees(), []string{"flaky", "one", "boom", "boom", "after"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v: flaky was continued past and stays so", got, want)
	}
	if a := probe.argsOf(t, "after", 0)["a"]; a != "A" {
		t.Fatalf("after(a: %v), want the value one recorded", a)
	}
}

// TestResumeV1_DB_AMutationReceiptHoldsItsRowOnceAndNoSchema: a REAL mutation's
// result -- the row the engine wrote, with its concept's schema on the node --
// is journaled to Postgres as a statement's receipt, read back through
// LoadRunJournal and resumed from (journal_step_result.go). The receipt holds
// the row once, as its value, and no schema anywhere; the mutation does not
// run again; and the statement after it reads from the rebound row exactly
// what the failed attempt read from the live one.
func TestResumeV1_DB_AMutationReceiptHoldsItsRowOnceAndNoSchema(t *testing.T) {
	engine := sharedJournalEngine(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	written, err := engine.Execute(journalContext(ctx), fmt.Sprintf(
		`mutation createWorkRun(runId: %s, automationName: "schemaProbe", templateFingerprint: "fp", status: "succeeded", startedAt: %s)`,
		jsonString(fmt.Sprintf("schema-probe-%d", stamp)), jsonString(rfc3339(time.Now()))))
	if err != nil {
		t.Fatalf("the real mutation: %v", err)
	}
	nodes := written.Bundle.GetNodes()
	if len(nodes) != 1 || len(nodes[0].GetSchema().GetFields()) == 0 {
		t.Fatalf("the engine's mutation result carries no schema on its node, so this test would prove nothing: %v", written.Bundle)
	}
	untrimmed, _ := json.Marshal(UnwrapStepResult(written))

	probe := newStmtProbe()
	probe.answers["writeThing"] = written
	probe.fails["finish"] = 1
	first, resumed := runThenResume(t, engine, resumeDBProbe{stmtProbe: probe, engine: engine}, fmt.Sprintf(`@trigger(event="probe.fired")
automation writeThenFinish%d {
  written := mutation writeThing()
  builtin finish(id: written.id, status: written.status)
}`, stamp))
	if resumed.Status != "completed" {
		t.Fatalf("resumed run status %q (%s)", resumed.Status, resumed.Error)
	}
	if got, want := probe.callees(), []string{"writeThing", "finish", "finish"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v: the mutation that finished must not run again", got, want)
	}
	failed, again := jsonShape(t, probe.argsOf(t, "finish", 0)), jsonShape(t, probe.argsOf(t, "finish", 1))
	if !reflect.DeepEqual(failed, again) {
		t.Fatalf("the resumed statement read %v from the rebound row; the failed attempt read %v from the live one", again, failed)
	}
	if args := probe.argsOf(t, "finish", 1); args["id"] != nodes[0].GetId() || args["status"] != "succeeded" {
		t.Fatalf("finish(%v), want the id and status of the row the mutation wrote (%s)", args, nodes[0].GetId())
	}

	// The receipt as Postgres holds it.
	steps, err := engine.Execute(journalContext(ctx), "query workStepsForRun(runId: "+jsonString(first.ID)+")")
	if err != nil {
		t.Fatalf("workStepsForRun: %v", err)
	}
	var receipt map[string]any
	for _, row := range memql.MaterializeRows(steps) {
		if row["key"] == "written" && row["status"] == "done" {
			receipt, _ = row["result"].(map[string]any)
		}
	}
	if receipt == nil {
		t.Fatalf("no done receipt for the mutation statement in run %s", first.ID)
	}
	stored, _ := json.Marshal(receipt)
	if strings.Contains(string(stored), `"schema"`) {
		t.Fatalf("the stored receipt records a concept schema:\n  %s", stored)
	}
	if _, has := receipt["result"]; has {
		t.Fatalf("the stored receipt records the Bundle beside the value it duplicates:\n  %s", stored)
	}
	if value, _ := receipt["value"].(map[string]any); value["id"] != nodes[0].GetId() || receipt["valueRecorded"] != true {
		t.Fatalf("the stored receipt's value is not the row the mutation wrote:\n  %s", stored)
	}
	t.Logf("receipt %d bytes; the Bundle alone was %d bytes before this change", len(stored), len(untrimmed))
}

// A private engine, unlike its two neighbours: this one registers a logic
// and installs a logic runner, which changes the engine (memql#5668).
func TestResumeV1_DB_RerunsALogicStatementOnce(t *testing.T) {
	engine := openTestEngine(t)
	stamp := time.Now().UnixNano()
	decide := registerLogic(t, engine, fmt.Sprintf(`logic resumeDecide%d {
  x := builtin readX()
  builtin write()
  return x
}`, stamp))
	probe := newStmtProbe()
	probe.answers["readX"] = memql.NewResultWithOutput("X")
	probe.fails["write"] = 1
	p := resumeDBProbe{stmtProbe: probe, engine: engine, logics: map[string]bool{decide: true}}
	engine.SetLogicRunner(NewLogicRunner(engine, p, nil))
	_, resumed := runThenResume(t, engine, p, fmt.Sprintf(`@trigger(event="probe.fired")
automation callsALogic%d {
  v := logic %s()
  builtin after(v: v)
}`, stamp, decide))
	if resumed.Status != "completed" {
		t.Fatalf("resumed run status %q (%s)", resumed.Status, resumed.Error)
	}
	// The logic's failed `write` is a row under `v`; resume re-runs `v`, once,
	// and the logic runs again from its first statement.
	if got, want := probe.callees(), []string{"readX", "write", "readX", "write", "after"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	if v := probe.argsOf(t, "after", 0)["v"]; v != "X" {
		t.Fatalf("after(v: %v), want X", v)
	}
}
