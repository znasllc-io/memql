package automations

// journal_loop_db_test.go -- the loop protection's rows against a real
// Postgres (epic memql#5380).
//
// The DB-free tests assert the calls the refusal renders. A recording fake
// accepts any call, and a journal write that the engine refuses is SILENT --
// call() logs a Warn and the run carries on -- so only a real engine proves
// the refused run's row lands with its code, its message and its chain, and
// that the parent cause openRun records comes back through a read as the cause
// a resume runs under.
//
// Postgres-gated: skips cleanly when no DB is reachable, FAILS under
// MEMQL_REQUIRE_DB=1.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/component/work"
)

// readRunRow reads one v1:work:run row the way resume does: through the
// @serverOnly workRunById, under the journal's own actor.
func readRunRow(t *testing.T, engine *memql.MemQLEngine, runId string) map[string]any {
	t.Helper()
	call, err := journalArgs("workRunById", map[string]any{"runId": runId})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Execute(journalContext(context.Background()), "query "+call)
	if err != nil {
		t.Fatalf("read run %s: %v", runId, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) != 1 {
		t.Fatalf("read run %s: %d rows, want 1", runId, len(rows))
	}
	return rows[0]
}

// numberOf is a stored number, however the read decoded it.
func numberOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return -1
}

func TestLoopRefusal_DB_TheRefusedRunIsOneFailedRowNamingTheChain(t *testing.T) {
	engine := openTestEngine(t)
	t.Setenv(maxChainDepthEnv, "16")
	sharedAutomationBudget.reset()
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: &causeProbeRegistry{}})
	defer e.Close()
	auto := causeProbeAutomation(fmt.Sprintf("loopRefusalProbe%d", time.Now().UnixNano()))
	auto.Trigger = &TriggerConfig{Event: loopProbeTopic}
	parent := chainOf(fmt.Sprintf("evt-dbroot%d", time.Now().UnixNano()), repeatName("upstream", 16)...)
	ev := &events.Event{Topic: loopProbeTopic, Kind: events.KindNodeUpdated, Payload: map[string]any{"id": "ticket-1"}, Cause: parent}
	before := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth)

	exec, err := e.ExecuteWithEvent(context.Background(), auto, "event:"+loopProbeTopic, ev)
	var r *LoopRefusal
	if !errors.As(err, &r) {
		t.Fatalf("got %v, want the refusal", err)
	}

	row := readRunRow(t, engine, exec.ID)
	if row["status"] != "failed" || row["errorCode"] != work.TerminalLoopDepthExceeded || row["automationName"] != auto.Name {
		t.Fatalf("the refused run's row = status %v, errorCode %v, automation %v", row["status"], row["errorCode"], row["automationName"])
	}
	// The message survives the write exactly: the renderer escapes the
	// chain's arrows for JSON, and the row must hold the arrows, not the escapes.
	if row["errorMessage"] != r.Error() {
		t.Errorf("errorMessage = %q, want %q", row["errorMessage"], r.Error())
	}
	if row["finishedAt"] == nil {
		t.Error("the refused run has no finishedAt; every terminal-run reader would treat it as in flight")
	}
	loop := mapOf(mapOf(row["outcome"])["loop"])
	if loop == nil || loop["reason"] != metrics.LoopStopDepth || numberOf(loop["depth"]) != 17 || numberOf(loop["cap"]) != 16 || loop["correlationId"] != parent.CorrelationId {
		t.Fatalf("outcome.loop = %v", row["outcome"])
	}
	if chain, _ := loop["chain"].([]any); len(chain) != 16 || mapOf(chain[15])["runId"] != "run-16" {
		t.Fatalf("outcome.loop.chain = %v, want the 16 upstream runs", loop["chain"])
	}

	// The parent cause came back through a read as the cause that went in:
	// this is what a resume or a recovery rebuilds the run's place from.
	journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
	if err != nil {
		t.Fatalf("LoadRunJournal: %v", err)
	}
	if got := events.CauseFromMap(mapOf(journal.TriggerEvent["cause"])); !reflect.DeepEqual(got, parent) {
		t.Fatalf("triggerEvent.cause read back as %+v, want %+v", got, parent)
	}
	if got := metrics.AutomationLoopsStoppedValue(auto.Name, metrics.LoopStopDepth) - before; got != 1 {
		t.Errorf("the stop was counted %v times, want 1", got)
	}
}

// causeFailOnceRegistry fails step "b" the first time and records the cause
// every step ran under.
type causeFailOnceRegistry struct {
	failed bool
	causes map[string][]events.Cause
}

func (r *causeFailOnceRegistry) Execute(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	c, _ := events.CauseFromContext(ctx)
	r.causes[step.ID] = append(r.causes[step.ID], c)
	now := time.Now()
	if step.ID == "b" && !r.failed {
		r.failed = true
		return &StepResult{StepId: step.ID, Status: "failed", Error: "first time fails", StartedAt: now, CompletedAt: now}, errors.New("first time fails")
	}
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": step.ID}, StartedAt: now, CompletedAt: now}, nil
}

// TestLoopDepth_DB_AResumedRunKeepsItsDepth: a run in a chain fails, is read
// back from its own rows and resumed, and the resumed step runs under exactly
// the cause the first attempt's steps did. A resume that restarted the chain
// would let a loop that fails and is resumed run past the cap forever.
func TestLoopDepth_DB_AResumedRunKeepsItsDepth(t *testing.T) {
	engine := openTestEngine(t)
	sharedAutomationBudget.reset()
	reg := &causeFailOnceRegistry{causes: map[string][]events.Cause{}}
	e := NewExecutor(ExecutorOptions{Engine: engine, StepRegistry: reg})
	defer e.Close()
	auto := &Automation{Name: fmt.Sprintf("loopResumeProbe%d", time.Now().UnixNano()), Trigger: &TriggerConfig{Event: loopProbeTopic}, Steps: []*Step{
		{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}},
		{ID: "b", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}, OnError: ErrorStrategyStop},
	}}
	parent := chainOf(fmt.Sprintf("evt-resume%d", time.Now().UnixNano()), "x", "y", "z")
	ev := &events.Event{Topic: loopProbeTopic, Kind: events.KindNodeUpdated, Payload: map[string]any{"id": "ticket-1"}, Cause: parent}

	exec, _ := e.ExecuteWithEvent(context.Background(), auto, "event:"+loopProbeTopic, ev)
	if exec.Status != "failed" {
		t.Fatalf("first attempt = %q, want failed at b", exec.Status)
	}
	first := reg.causes["b"][0]
	if want := parent.Next(auto.Name, exec.ID, parent.CorrelationId); !reflect.DeepEqual(first, want) {
		t.Fatalf("the first attempt ran under %+v, want %+v", first, want)
	}

	journal, err := LoadRunJournal(context.Background(), engine, exec.ID)
	if err != nil {
		t.Fatalf("LoadRunJournal: %v", err)
	}
	if _, err := e.ResumeFrom(context.Background(), journal, auto, &ResumeOptions{}); err != nil {
		t.Fatalf("ResumeFrom: %v", err)
	}
	if len(reg.causes["b"]) != 2 {
		t.Fatalf("b ran %d times, want twice (the failure, then the resume)", len(reg.causes["b"]))
	}
	if resumed := reg.causes["b"][1]; !reflect.DeepEqual(resumed, first) {
		t.Fatalf("the resumed step ran under %+v, the first attempt under %+v: the resume left the run's chain", resumed, first)
	}
}
