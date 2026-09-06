package automations

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

var errBoom = errors.New("boom")

// recordingJournalExecutor captures every call the journal renders so the
// exact MemQL text is asserted without an engine or a database.
type recordingJournalExecutor struct {
	calls []string
	ctxs  []context.Context
}

func (r *recordingJournalExecutor) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	r.calls = append(r.calls, query)
	r.ctxs = append(r.ctxs, ctx)
	return &memql.ExecuteResult{}, nil
}

// argsOf parses a rendered `name(k: v, ...)` call back into its name and
// arguments.
//
// It does that by quoting the top-level keys and reading the result as a JSON
// object, which works because journalArgs JSON-encodes every VALUE. Written
// this way rather than against `name({...})` deliberately: the object-literal
// wrapper is rejected by the parser (#2335), so a test that accepted it would
// be asserting a call the engine refuses -- which is exactly what the first
// version of this file did, with every test green and every journal write
// silently failing.
func argsOf(t *testing.T, call string) (string, map[string]any) {
	t.Helper()
	open := strings.Index(call, "(")
	if open < 0 || !strings.HasSuffix(call, ")") {
		t.Fatalf("call %q is not name(k: v, ...)", call)
	}
	name, body := call[:open], call[open+1:len(call)-1]
	args := map[string]any{}
	if strings.TrimSpace(body) == "" {
		return name, args
	}

	// Walk the body quoting bare top-level keys: `runId: "x", seq: 0` becomes
	// `"runId": "x", "seq": 0`. Depth and in-string tracking keep nested
	// object values (`call: {"construct": "query"}`) untouched.
	var b strings.Builder
	depth, inString, escaped, atKey := 0, false, false, true
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
		case c == ',' && depth == 0:
			atKey = true
		case depth == 0 && atKey && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'):
			j := i
			for j < len(body) && (body[j] != ':') {
				j++
			}
			key := strings.TrimSpace(body[i:j])
			b.WriteString(`"` + key + `"`)
			i = j - 1
			atKey = false
			continue
		}
		b.WriteByte(c)
	}
	if err := json.Unmarshal([]byte("{"+b.String()+"}"), &args); err != nil {
		t.Fatalf("call %q does not re-read as named args (%q): %v", call, "{"+b.String()+"}", err)
	}
	return name, args
}

func TestWorkStepId_SanitisesTheKey(t *testing.T) {
	if got := workStepId("run-1", "layer0.sales"); got != "run-1-layer0-sales" {
		t.Fatalf("workStepId = %q, want run-1-layer0-sales", got)
	}
	if got := workStepId("run-1", "ok_key"); got != "run-1-ok_key" {
		t.Fatalf("workStepId = %q, want run-1-ok_key", got)
	}
}

func TestStepKindFor_DerivesDeterministicExceptFunction(t *testing.T) {
	for _, tc := range []struct {
		typ  StepType
		want string
	}{
		{StepTypeQuery, "deterministic"},
		{StepTypeMutation, "deterministic"},
		{StepTypeAction, "deterministic"},
		{StepTypeParallel, "deterministic"},
		{StepTypeAutomation, "deterministic"},
		{StepTypeFunction, ""},
	} {
		if got := stepKindFor(&Step{Type: tc.typ}); got != tc.want {
			t.Errorf("stepKindFor(%s) = %q, want %q", tc.typ, got, tc.want)
		}
	}
}

func TestJournalSkipsAutomation_ReactingToItsOwnRows(t *testing.T) {
	loop := &Automation{Name: "onStep", Trigger: &TriggerConfig{Event: "graph.node.created.*.v1:work:step"}}
	if !journalSkipsAutomation(loop) {
		t.Fatal("an automation triggered by a work row must not journal itself: that is a feedback loop")
	}
	bare := &Automation{Name: "onRun", Trigger: &TriggerConfig{Event: "graph.node.updated.v1:work:run"}}
	if !journalSkipsAutomation(bare) {
		t.Fatal("the partition segment is optional; the bare form must be caught too")
	}
	plain := &Automation{Name: "sweep", Trigger: &TriggerConfig{Event: "graph.node.created.*.v1:library:file"}}
	if journalSkipsAutomation(plain) {
		t.Fatal("an ordinary automation is journaled")
	}
	if journalSkipsAutomation(&Automation{Name: "cron", Schedule: "0 * * * * *"}) {
		t.Fatal("a scheduled automation is journaled")
	}
}

func TestJournalContext_IsSyntheticInternalClusterActor(t *testing.T) {
	ctx := journalContext(context.Background())
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("no access context")
	}
	if !ac.Synthetic || !ac.Unranked || ac.Role != auth.RoleOwner {
		t.Fatalf("journal actor = %+v, want Synthetic, Unranked, RoleOwner", ac)
	}
	if !auth.OriginFromContext(ctx).IsInternal() {
		t.Fatal("journal writes must carry internal origin: the mutations are @serverOnly")
	}
	// The six server-only reads spell `actor.isClusterOwner==true`, and this
	// is the actor that has to satisfy it -- see dsl/work/queries.memql.
	if !ac.IsClusterOwner() {
		t.Fatal("the journal actor must satisfy actor.isClusterOwner==true, or every run-scoped read returns zero rows and resume re-runs completed steps")
	}
}

func TestJournal_RunAndStepLifecycle(t *testing.T) {
	rec := &recordingJournalExecutor{}
	j := newWorkJournal(rec, nil)
	j.nodeId = "node-a"
	auto := &Automation{Name: "demo", Steps: []*Step{{ID: "one", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}}}}
	exec := NewExecution("demo", "test")
	exec.ID = "run-1"
	exec.StartedAt = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	j.openRun(context.Background(), auto, exec, nil)
	j.stepRunning(context.Background(), exec, auto.Steps[0], 0, 1)
	res := &StepResult{StepId: "one", Status: "completed", Result: map[string]any{"rows": 3}, StartedAt: exec.StartedAt, CompletedAt: exec.StartedAt.Add(20 * time.Millisecond), Duration: 20 * time.Millisecond}
	j.stepFinished(context.Background(), exec, auto.Steps[0], res, "")
	exec.Complete()
	j.closeRun(context.Background(), exec, "")

	if len(rec.calls) != 5 {
		t.Fatalf("calls = %d (%v), want 5: createWorkRun, createWorkStep, updateWorkStep, updateWorkRun (heartbeat), updateWorkRun (close)", len(rec.calls), rec.calls)
	}
	name, args := argsOf(t, rec.calls[0])
	if name != "createWorkRun" || args["runId"] != "run-1" || args["automationName"] != "demo" || args["status"] != "running" || args["nodeId"] != "node-a" {
		t.Errorf("open: %s %v", name, args)
	}
	if args["templateFingerprint"] == "" || args["templateFingerprint"] == nil {
		t.Error("open must record the automation definition fingerprint, or resume cannot refuse a changed automation")
	}
	name, args = argsOf(t, rec.calls[1])
	if name != "createWorkStep" || args["stepId"] != "run-1-one" || args["key"] != "one" || args["status"] != "running" || args["kind"] != "deterministic" || args["stepType"] != "query" {
		t.Errorf("running: %s %v", name, args)
	}
	if _, present := args["input"]; present {
		t.Error("resolved step arguments must not be journaled in A1 (they may carry resolved secrets)")
	}
	name, args = argsOf(t, rec.calls[2])
	if name != "updateWorkStep" || args["stepId"] != "run-1-one" || args["status"] != "done" || args["durationMs"] != float64(20) {
		t.Errorf("finished: %s %v", name, args)
	}
	if args["result"] == nil {
		t.Error("a done step carries its trimmed result for resume to rehydrate")
	}
	name, args = argsOf(t, rec.calls[3])
	if name != "updateWorkRun" || args["runId"] != "run-1" || args["heartbeatAt"] == nil {
		t.Errorf("heartbeat: %s %v", name, args)
	}
	name, args = argsOf(t, rec.calls[4])
	if name != "updateWorkRun" || args["status"] != "succeeded" || args["finishedAt"] == nil {
		t.Errorf("close: %s %v", name, args)
	}
	for i, c := range rec.ctxs {
		if ac, _ := auth.AccessFromContext(c); ac == nil || !ac.Synthetic {
			t.Errorf("call %d was not made under the synthetic journal actor", i)
		}
	}
}

func TestJournal_FailedStepAndFailedRun(t *testing.T) {
	rec := &recordingJournalExecutor{}
	j := newWorkJournal(rec, nil)
	auto := &Automation{Name: "demo", Steps: []*Step{{ID: "one", Type: StepTypeMutation, Mutation: &MutationStepConfig{Concept: "v1:x:y"}}}}
	exec := NewExecution("demo", "test")
	exec.ID = "run-2"
	j.stepRunning(context.Background(), exec, auto.Steps[0], 0, 1)
	res := &StepResult{StepId: "one", Status: "failed", Error: "boom", StartedAt: time.Now(), CompletedAt: time.Now()}
	j.stepFinished(context.Background(), exec, auto.Steps[0], res, "")
	exec.Fail(errBoom)
	j.closeRun(context.Background(), exec, "")
	_, args := argsOf(t, rec.calls[1])
	if args["status"] != "failed" || args["errorMessage"] != "boom" {
		t.Errorf("failed step: %v", args)
	}
	if _, present := args["result"]; present {
		t.Error("a failed step carries no result")
	}
	_, args = argsOf(t, rec.calls[3])
	if args["status"] != "failed" || args["errorMessage"] != "boom" {
		t.Errorf("failed run: %v", args)
	}
}

func TestJournal_NilIsANoOp(t *testing.T) {
	var j *workJournal
	j.openRun(context.Background(), &Automation{Name: "x"}, NewExecution("x", "t"), nil)
	j.closeRun(context.Background(), NewExecution("x", "t"), "")
	if newWorkJournal(nil, nil) != nil {
		t.Fatal("no executor means no journal")
	}
}

// journalProbeRegistry runs every step as a success and records nothing
// else; it exists so the executor's own loop drives the journal.
type journalProbeRegistry struct{}

func (journalProbeRegistry) Execute(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{"ok": true}, StartedAt: now, CompletedAt: now}, nil
}

func TestExecutor_JournalsEveryStepBoundary(t *testing.T) {
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}})
	e.journal = newWorkJournal(rec, nil)
	auto := &Automation{Name: "demo", Steps: []*Step{
		{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}},
		{ID: "b", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}, Condition: "false"},
	}}
	exec, err := e.Execute(context.Background(), auto, "test")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exec.Status != "completed" {
		t.Fatalf("status = %q", exec.Status)
	}
	var names []string
	for _, c := range rec.calls {
		n, _ := argsOf(t, c)
		names = append(names, n)
	}
	want := []string{"createWorkRun", "createWorkStep", "updateWorkStep", "updateWorkRun", "createWorkStep", "updateWorkRun"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("journal calls = %v, want %v (open; a running; a done + heartbeat; b skipped; close)", names, want)
	}
	_, skipped := argsOf(t, rec.calls[4])
	if skipped["key"] != "b" || skipped["status"] != "skipped" {
		t.Errorf("skipped step: %v", skipped)
	}
}

func TestExecutor_SandboxRunWritesNoJournal(t *testing.T) {
	e := NewExecutor(ExecutorOptions{StepRegistry: journalProbeRegistry{}, SandboxRun: true})
	if e.journal != nil {
		t.Fatal("a sandboxed dry-run must not journal: nothing resumes a preview and the write would escape the sandbox")
	}
}

// TestExecutor_JournalCarriesTheChainHeadOnFailure pins the one place the
// journal is NOT a straight read of AutomationExecution. exec.ChainHead is
// assigned only on the success path, so a failed run's close would carry an
// empty chain head if closeRun read the struct -- which is exactly what the
// checkpoint it replaces did NOT do (saveCheckpointOnFailure was handed the
// loop's local `chainHead`). The journal takes it as an argument for that
// reason, and this test is what stops it being "simplified" back.
func TestExecutor_JournalCarriesTheChainHeadOnFailure(t *testing.T) {
	rec := &recordingJournalExecutor{}
	e := NewExecutor(ExecutorOptions{StepRegistry: failingRegistry{}, ChainTrackingEnabled: true})
	e.journal = newWorkJournal(rec, nil)
	auto := &Automation{Name: "demo", Steps: []*Step{
		{ID: "a", Type: StepTypeQuery, Query: &QueryStepConfig{Query: "q"}, OnError: ErrorStrategyStop},
	}}
	exec, _ := e.Execute(context.Background(), auto, "test")
	if exec.Status != "failed" {
		t.Fatalf("status = %q, want failed", exec.Status)
	}
	last := rec.calls[len(rec.calls)-1]
	name, args := argsOf(t, last)
	if name != "updateWorkRun" || args["status"] != "failed" {
		t.Fatalf("close: %s %v", name, args)
	}
	if args["chainHead"] == nil || args["chainHead"] == "" {
		t.Error("a failed run's close carries no chain head; resume cannot verify the prefix it is resuming onto")
	}
}

// failingRegistry fails every step, with a result attached, which is the
// shape the executor's stop-on-error path takes.
type failingRegistry struct{}

func (failingRegistry) Execute(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "failed", Error: "boom", StartedAt: now, CompletedAt: now}, errBoom
}

// TestJournalArgs_RendersNamedArgsAndDropsNils pins the two defects the
// db-gated lane caught, both of which every DB-free test in this package was
// green through -- because a recording fake accepts whatever it is handed and
// call() logs a Warn rather than failing the run.
//
//  1. THE OBJECT-LITERAL WRAPPER IS REJECTED. `name({...})` was removed in
//     #2335 and the parser refuses it outright, so the first version of this
//     journal never wrote a single row.
//  2. A NIL VALUE MUST BE DROPPED, not rendered. exec.Input is nil whenever
//     an automation declares no `input:` block, and `input: null` fails the
//     concept's `object` type -- so every step row landed and no run row ever
//     did, which reads as "resume is broken" rather than "the writer is".
func TestJournalArgs_RendersNamedArgsAndDropsNils(t *testing.T) {
	got, err := journalArgs("createWorkRun", map[string]any{
		"runId":                 "r1",
		"input":                 nil,
		"callerSuppliedPayload": false,
		"seq":                   0,
	})
	if err != nil {
		t.Fatalf("journalArgs: %v", err)
	}
	want := `createWorkRun(callerSuppliedPayload: false, runId: "r1", seq: 0)`
	if got != want {
		t.Fatalf("journalArgs =\n  %s\nwant\n  %s", got, want)
	}
	if strings.Contains(got, "({") {
		t.Error("the object-literal wrapper is rejected by the parser (#2335); every write would fail silently")
	}
	if strings.Contains(got, "null") {
		t.Error("a nil argument must be DROPPED: `input: null` fails the concept's object type and refuses the whole row")
	}
	// false and 0 are values, not absences, and dropping them would change
	// what the row says.
	if !strings.Contains(got, "callerSuppliedPayload: false") {
		t.Error("callerSuppliedPayload=false was dropped; it is the memql#2888 flag and its absence reads as unset")
	}
	if !strings.Contains(got, "seq: 0") {
		t.Error("seq=0 was dropped; step 0 is the first step, not a missing one")
	}

	if got, err := journalArgs("x", map[string]any{"a": nil}); err != nil || got != "x()" {
		t.Fatalf("all-nil args = %q, %v; want x()", got, err)
	}
}
