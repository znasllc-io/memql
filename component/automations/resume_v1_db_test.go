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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
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

// TestResumeV1_DB_AProjectedTriggerResumesWithWhatItReads: an event firing
// records on its run only the payload keys the run can read
// (journal_trigger.go), and a resume from that stored row binds the same args
// and continues the same cause as the attempt that failed. The trigger here carries 30 KB the run never reads, as
// a staged inbound delivery's headers and body did on production.
func TestResumeV1_DB_AProjectedTriggerResumesWithWhatItReads(t *testing.T) {
	engine := sharedJournalEngine(t)
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	a := statementAutomation(t, fmt.Sprintf(`@trigger(event="probe.fired")
automation projectedTrigger%d {
  args {
    id any
    title any
    label any
  }
  first := builtin one(id: args.id)
  builtin record(title: args.title, label: args.label)
}`, stamp))
	a.JournalTriggerReads = computeJournalTriggerReads(a, registrySource{reg: work.Registry{
		"one": {ConstructKind: work.ConstructBuiltin}, "record": {ConstructKind: work.ConstructBuiltin},
	}})
	if want := []string{"id", "label", "nodeId", "title"}; !reflect.DeepEqual(a.JournalTriggerReads, want) {
		t.Fatalf("the run can read %v of its trigger, want %v", a.JournalTriggerReads, want)
	}
	probe := newStmtProbe()
	probe.answers["one"] = memql.NewResultWithOutput("first")
	probe.fails["record"] = 1
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: resumeDBProbe{stmtProbe: probe, engine: engine}})
	defer e.Close()
	// Hex compresses about as poorly as a webhook's JSON body, so the stored
	// sizes below are near what such a trigger costs.
	var unread strings.Builder
	for i := 0; unread.Len() < 30<<10; i++ {
		sum := sha256.Sum256([]byte(fmt.Sprint(stamp, i)))
		unread.WriteString(hex.EncodeToString(sum[:]))
	}
	ev := events.NewEvent("probe.fired", events.KindMessage, map[string]any{
		"id": fmt.Sprintf("row-%d", stamp), "title": "Title", "label": "Label", "bodyText": unread.String(), "headersJson": unread.String()[:8<<10],
	})
	first, _ := e.ExecuteWithEvent(ctx, a, TriggeredByEventPrefix+ev.Topic, &ev)
	if first == nil || first.Status != "failed" {
		t.Fatalf("the first run did not fail: %+v", first)
	}

	// The run row as Postgres holds it.
	rows, err := engine.Execute(journalContext(ctx), "query workRunById(runId: "+jsonString(first.ID)+")")
	if err != nil || len(memql.MaterializeRows(rows)) != 1 {
		t.Fatalf("workRunById: %v", err)
	}
	trigger, _ := memql.MaterializeRows(rows)[0]["triggerEvent"].(map[string]any)
	payload, _ := trigger["payload"].(map[string]any)
	if len(payload) != 3 || payload["label"] != "Label" || payload["bodyText"] != nil {
		t.Fatalf("the stored trigger payload is %d keys: %v", len(payload), payload)
	}
	cause, _ := trigger["cause"].(map[string]any)
	if cause["correlationId"] != rootCorrelation(&ev) {
		t.Fatalf("the stored cause %v does not carry the root's correlation %s", cause, rootCorrelation(&ev))
	}
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	defer db.Close()
	stored := func(runID string) (versions, bytes int) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(pg_column_size(payload)),0) FROM "MemoryNodes" WHERE id = $1`, "v1:work:run:"+memql.BareShortId(runID)).Scan(&versions, &bytes); err != nil {
			t.Fatal(err)
		}
		return versions, bytes
	}
	versions, bytes := stored(first.ID)
	// The same firing recorded whole, as every run was before 2026-11-08.
	whole := statementAutomation(t, strings.Replace(fmt.Sprintf(`@trigger(event="probe.fired")
automation projectedTrigger%d {
  args {
    id any
    title any
    label any
  }
  first := builtin one(id: args.id)
  builtin record(title: args.title, label: args.label)
}`, stamp), "projectedTrigger", "wholeTrigger", 1))
	unprojected, _ := e.ExecuteWithEvent(ctx, whole, TriggeredByEventPrefix+ev.Topic, &ev)
	wholeVersions, wholeBytes := stored(unprojected.ID)
	t.Logf("the failed run's row stored: %d versions in %d payload bytes; recorded whole, %d versions in %d bytes", versions, bytes, wholeVersions, wholeBytes)
	if versions != wholeVersions || bytes*5 > wholeBytes {
		t.Fatalf("the projected run stored %d bytes in %d versions, the whole one %d in %d", bytes, versions, wholeBytes, wholeVersions)
	}

	journal, err := LoadRunJournal(ctx, engine, first.ID)
	if err != nil {
		t.Fatalf("LoadRunJournal: %v", err)
	}
	_, firstCause := runCause(ctx, a, first.ID, &ev)
	if got := journalRunCause(ctx, a, first.ID, journal.TriggerEvent); !reflect.DeepEqual(got, firstCause) {
		t.Fatalf("a resume continues cause %+v; the first attempt ran under %+v", got, firstCause)
	}
	resumed, err := e.ResumeFrom(ctx, journal, a, &ResumeOptions{AllowSideEffects: true})
	if err != nil || resumed.Status != "completed" {
		t.Fatalf("resume: %v %+v", err, resumed)
	}
	if failed, again := jsonShape(t, probe.argsOf(t, "record", 0)), jsonShape(t, probe.argsOf(t, "record", 1)); !reflect.DeepEqual(failed, again) {
		t.Fatalf("the resumed statement read %v; the failed attempt read %v", again, failed)
	}
}
