package automations

// logic_journal_db_test.go -- a statement-body logic's journal against a real
// Postgres (epic memql#5370, task memql#5372; D14).
//
// logic_statements_test.go asserts the calls the journal renders, through a
// recorder that accepts anything. This asserts what the engine does with
// them: that a direct call's held rows parse, are accepted and read back as
// the run of `logic:<name>`; that a read-only call leaves nothing; and that a
// logic called by an automation's statement lands in that automation's run
// under nested keys. The logic is called the way a client calls it -- a
// registered function through engine.Execute -- and its write is a real
// engine write, so the run opens on the engine's own write report.
//
// Postgres-gated: skips cleanly when no DB is reachable, FAILS under
// MEMQL_REQUIRE_DB=1.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// dbLogicProbe answers construct calls. `write` makes a real graph write
// through the engine (a v1:work:run row under a throwaway id, written as the
// cluster -- but NOT through the journal, whose writes are masked); a call
// naming a registered logic runs it through engine.Execute with the step's
// context, as a logic statement does; everything else answers its name. It
// records the run each call was made in.
type dbLogicProbe struct {
	engine *memql.MemQLEngine
	logics map[string]bool

	mu   sync.Mutex
	runs []string
}

func (p *dbLogicProbe) Execute(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	now := time.Now()
	res := &StepResult{StepId: step.ID, StartedAt: now, Status: "success"}
	if run, ok := common.RunFromContext(ctx); ok {
		p.mu.Lock()
		p.runs = append(p.runs, run.RunId)
		p.mu.Unlock()
	}
	name := step.Function.Name
	switch {
	case p.logics[name]:
		out, err := p.engine.Execute(ctx, name+"()")
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
			return res, err
		}
		res.Result = out
	case name == "write":
		call, err := journalArgs("createWorkRun", map[string]any{
			"runId":               fmt.Sprintf("logicWriteProbe%d", time.Now().UnixNano()),
			"automationName":      "logicWriteProbe",
			"templateFingerprint": "logicWriteProbe",
			"status":              "running",
			"mode":                "live",
			"triggeredBy":         "test",
			"startedAt":           rfc3339(now),
		})
		if err == nil {
			_, err = p.engine.Execute(clusterWriteContext(ctx), call)
		}
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
			return res, err
		}
	default:
		res.Result = memql.NewResultWithOutput(name)
	}
	res.CompletedAt = time.Now()
	return res, nil
}

// lastRun is the run the probe's last call was made in.
func (p *dbLogicProbe) lastRun(t *testing.T) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.runs) == 0 {
		t.Fatal("no call was made inside a run")
	}
	return p.runs[len(p.runs)-1]
}

// clusterWriteContext is journalContext's actor without its mask: the probe's
// write is the logic's write, and it must reach the observer.
func clusterWriteContext(ctx context.Context) context.Context {
	const subject = "cluster:logic-journal-probe"
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: subject, Claims: map[string]any{"sub": subject, "role": "system"}})
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: subject, Role: auth.RoleOwner, Unranked: true, Synthetic: true})
	return auth.ContextWithInternalOrigin(ctx)
}

// registerLogic compiles src and registers it on the engine as the loader
// would, returning its name.
func registerLogic(t *testing.T, engine *memql.MemQLEngine, src string) string {
	t.Helper()
	name, body := compiledLogic(t, src)
	if err := engine.Functions().Upsert(&memql.Function{Name: name, FunctionKind: "logic", Enabled: true, LogicBody: body}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return name
}

func TestLogicJournal_DB_DirectWritingLogicLeavesARun(t *testing.T) {
	engine := openTestEngine(t)
	probe := &dbLogicProbe{engine: engine}
	engine.SetLogicRunner(NewLogicRunner(engine, probe, nil))
	name := registerLogic(t, engine, fmt.Sprintf(`logic logicJournalWrites%d {
  a := builtin readA()
  builtin write()
  b := builtin readB()
  return b
}`, time.Now().UnixNano()))

	if _, err := engine.Execute(clusterWriteContext(context.Background()), name+"()"); err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	journal, err := LoadRunJournal(context.Background(), engine, probe.lastRun(t))
	if err != nil {
		t.Fatalf("the logic's run: %v", err)
	}
	if journal.AutomationName != "logic:"+name {
		t.Fatalf("the run is %q's, want logic:%s", journal.AutomationName, name)
	}
	if journal.Status != "succeeded" {
		t.Fatalf("run status %q, want succeeded", journal.Status)
	}
	for _, key := range []string{"a", "write", "b", "return"} {
		if s := journal.Steps[key]; s == nil || s.Status != "success" {
			t.Errorf("step %q: %+v -- every statement, the ones before the write included", key, s)
		}
	}
}

func TestLogicJournal_DB_ReadOnlyLogicLeavesNoRun(t *testing.T) {
	engine := openTestEngine(t)
	probe := &dbLogicProbe{engine: engine}
	engine.SetLogicRunner(NewLogicRunner(engine, probe, nil))
	name := registerLogic(t, engine, fmt.Sprintf(`logic logicJournalReads%d {
  a := builtin readA()
  return a
}`, time.Now().UnixNano()))

	if _, err := engine.Execute(clusterWriteContext(context.Background()), name+"()"); err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	// The run the call would have been, had it written.
	if _, err := LoadRunJournal(context.Background(), engine, probe.lastRun(t)); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("a read-only logic's run: %v, want none", err)
	}
}

func TestLogicJournal_DB_LogicInsideARunJournalsAsItsSteps(t *testing.T) {
	engine := openTestEngine(t)
	stamp := time.Now().UnixNano()
	probe := &dbLogicProbe{engine: engine, logics: map[string]bool{}}
	engine.SetLogicRunner(NewLogicRunner(engine, probe, nil))
	decide := registerLogic(t, engine, fmt.Sprintf(`logic decide%d {
  a := builtin readA()
  builtin write()
  return a
}`, stamp))
	probe.logics[decide] = true

	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: probe})
	defer e.Close()
	a := statementAutomation(t, fmt.Sprintf(`@trigger(event="probe.fired")
automation applies%d {
  verdict := logic %s()
  builtin after()
}`, stamp, decide))
	ev := events.NewEvent("probe.fired", events.KindMessage, nil)
	exec, err := e.ExecuteWithEvent(context.Background(), a, "test", &ev)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
	if err != nil {
		t.Fatalf("the automation's run: %v", err)
	}
	for _, key := range []string{"verdict", "verdict/a", "verdict/write", "verdict/return", "after"} {
		if s := journal.Steps[key]; s == nil || s.Status != "success" {
			t.Errorf("step %q: %+v", key, s)
		}
	}
	if len(journal.StepOrder) != 2 {
		t.Errorf("step order %v: the run's own statements only", journal.StepOrder)
	}
	for _, run := range probe.runs {
		if run != exec.ID {
			t.Fatalf("a call was made in run %s, want every one in the automation's %s", run, exec.ID)
		}
	}
}
