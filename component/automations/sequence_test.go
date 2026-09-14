package automations

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// sequence_test.go -- a statement body run through the real executor (epic
// memql#5370, task memql#5372). Each body is compiled from source by the
// loader the tree uses, then run by an Executor whose step registry is a
// probe: every construct call is recorded with the argument values it
// received, answers what the test configured, and fails when told to.

// stmtProbe is the probe registry.
type stmtProbe struct {
	mu      sync.Mutex
	calls   []stmtCall
	answers map[string]any // callee -> result (an *memql.ExecuteResult, a map, a scalar)
	fails   map[string]int // callee -> how many attempts fail before one succeeds (-1: always)
	tries   map[string]int // callee -> attempts so far
}

type stmtCall struct {
	callee string
	args   map[string]any
}

func newStmtProbe() *stmtProbe {
	return &stmtProbe{answers: map[string]any{}, fails: map[string]int{}, tries: map[string]int{}}
}

func (p *stmtProbe) Execute(ctx context.Context, step *Step, stepCtx *StepContext) (*StepResult, error) {
	now := time.Now()
	res := &StepResult{StepId: step.ID, StartedAt: now, CompletedAt: now}
	var callee string
	var raw map[string]any
	switch {
	case step.Function != nil:
		callee, raw = step.Function.Name, step.Function.Args
	case step.Event != nil:
		topic, err := stepCtx.Evaluator.EvalV1(ctx, step.Exprs.Topic)
		if err != nil {
			return res, err
		}
		callee, raw = "publish:"+V1Text(topic), step.Event.Payload
	case step.Action != nil:
		callee, raw = "action:"+step.Action.Ref, step.Action.Args
	default:
		return res, fmt.Errorf("the probe does not run a %s step", step.Type)
	}
	args, err := stepCtx.Evaluator.ResolveV1Map(ctx, raw)
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res, err
	}
	p.mu.Lock()
	p.calls = append(p.calls, stmtCall{callee: callee, args: args})
	p.tries[callee]++
	try, failN := p.tries[callee], p.fails[callee]
	answer := p.answers[callee]
	p.mu.Unlock()
	if failN < 0 || try <= failN {
		res.Status, res.Error = "failed", callee+" failed"
		return res, fmt.Errorf("%s failed (attempt %d)", callee, try)
	}
	res.Status, res.Result = "success", answer
	return res, nil
}

func (p *stmtProbe) callees() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		out = append(out, c.callee)
	}
	return out
}

func (p *stmtProbe) argsOf(t *testing.T, callee string, nth int) map[string]any {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c.callee == callee {
			if n == nth {
				return c.args
			}
			n++
		}
	}
	t.Fatalf("%s was called %d time(s); no call #%d", callee, n, nth+1)
	return nil
}

// statementAutomation compiles src with the tree's loader and requires a
// statement body.
func statementAutomation(t *testing.T, src string) *Automation {
	t.Helper()
	a, err := NewLoader(LoaderOptions{}).CompileSource(src, "test.memql")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !a.IsStatementBody() {
		t.Fatalf("%s did not load as a statement body (body %q)", a.Name, a.Body)
	}
	return a
}

func runStatements(t *testing.T, src string, probe *stmtProbe, payload map[string]any) (*AutomationExecution, error) {
	t.Helper()
	a := statementAutomation(t, src)
	ev := events.NewEvent("probe.fired", events.KindMessage, payload)
	return NewExecutor(ExecutorOptions{StepRegistry: probe}).ExecuteWithEvent(context.Background(), a, "test", &ev)
}

func rowsResult(rows ...map[string]any) *memql.ExecuteResult {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return memql.NewResultWithOutput(out)
}

func TestSequenceRunsInSourceOrderAndBindsNames(t *testing.T) {
	probe := newStmtProbe()
	probe.answers["one"] = memql.NewResultWithOutput("first")
	probe.answers["two"] = memql.NewResultWithOutput(map[string]any{"n": int64(2)})
	exec, err := runStatements(t, `@trigger(event="probe.fired")
automation ordered {
  a := builtin one()
  b := builtin two(v: a)
  builtin three(v: b.n, w: a + "!")
}`, probe, nil)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	if got := strings.Join(probe.callees(), ","); got != "one,two,three" {
		t.Fatalf("calls = %s, want the source order", got)
	}
	if v := probe.argsOf(t, "two", 0)["v"]; v != "first" {
		t.Errorf("two(v: a) got %v, want the value one returned", v)
	}
	if got := probe.argsOf(t, "three", 0); got["v"] != int64(2) || got["w"] != "first!" {
		t.Errorf("three got %v, want v: 2 and w: \"first!\"", got)
	}
}

func TestSequenceSkippedStatementBindsNothing(t *testing.T) {
	probe := newStmtProbe()
	probe.answers["one"] = memql.NewResultWithOutput("ran")
	_, err := runStatements(t, `@trigger(event="probe.fired")
automation skipped {
  args {
    go any
  }
  if args.go == true {
    x := builtin one()
  }
  builtin report(x: x ?? "absent")
}`, probe, map[string]any{"go": false})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(probe.callees(), ","); got != "report" {
		t.Fatalf("calls = %s, want only report: the if's branch did not run", got)
	}
	if v := probe.argsOf(t, "report", 0)["x"]; v != "absent" {
		t.Errorf("x read %v after a branch that did not run, want absent", v)
	}
}

func TestSequenceSiblingBranchesBindWhicheverRan(t *testing.T) {
	for _, c := range []struct {
		fast bool
		want string
	}{{true, "quick"}, {false, "full"}} {
		probe := newStmtProbe()
		probe.answers["quickQuote"] = memql.NewResultWithOutput("quick")
		probe.answers["fullQuote"] = memql.NewResultWithOutput("full")
		_, err := runStatements(t, `@trigger(event="probe.fired")
automation quote {
  args {
    fast any
  }
  if args.fast == true {
    q := builtin quickQuote()
  } else {
    q := builtin fullQuote()
  }
  builtin send(q: q)
}`, probe, map[string]any{"fast": c.fast})
		if err != nil {
			t.Fatal(err)
		}
		if v := probe.argsOf(t, "send", 0)["q"]; v != c.want {
			t.Errorf("fast=%v: q = %v, want %s", c.fast, v, c.want)
		}
	}
}

func TestSequenceRetryAndOnError(t *testing.T) {
	t.Run("retry(2) runs a failing call three times, then the run fails", func(t *testing.T) {
		probe := newStmtProbe()
		probe.fails["flaky"] = -1
		exec, err := runStatements(t, `@trigger(event="probe.fired")
automation retries {
  builtin flaky() retry(2)
  builtin after()
}`, probe, nil)
		if err == nil || exec.Status != "failed" {
			t.Fatalf("status %s, err %v: the run should fail", exec.Status, err)
		}
		if got := strings.Join(probe.callees(), ","); got != "flaky,flaky,flaky" {
			t.Fatalf("calls = %s, want three attempts and nothing after", got)
		}
	})
	t.Run("retry(2) succeeds on the third attempt", func(t *testing.T) {
		probe := newStmtProbe()
		probe.fails["flaky"] = 2
		probe.answers["flaky"] = memql.NewResultWithOutput("ok")
		if _, err := runStatements(t, `@trigger(event="probe.fired")
automation retries {
  r := builtin flaky() retry(2)
  builtin after(r: r)
}`, probe, nil); err != nil {
			t.Fatal(err)
		}
		if v := probe.argsOf(t, "after", 0)["r"]; v != "ok" {
			t.Errorf("r = %v, want the attempt that succeeded", v)
		}
	})
	t.Run("retry(1) on error continue goes on with the name absent", func(t *testing.T) {
		probe := newStmtProbe()
		probe.fails["flaky"] = -1
		exec, err := runStatements(t, `@trigger(event="probe.fired")
automation continues {
  r := builtin flaky() retry(1) on error continue
  builtin after(r: r ?? "absent")
}`, probe, nil)
		if err != nil || exec.Status != "completed" {
			t.Fatalf("status %s, err %v: on error continue goes on", exec.Status, err)
		}
		if got := strings.Join(probe.callees(), ","); got != "flaky,flaky,after" {
			t.Fatalf("calls = %s", got)
		}
		if v := probe.argsOf(t, "after", 0)["r"]; v != "absent" {
			t.Errorf("r = %v after a failed statement, want absent", v)
		}
	})
}

func TestSequenceReturn(t *testing.T) {
	t.Run("return ends the run with its value; nothing after it runs", func(t *testing.T) {
		probe := newStmtProbe()
		exec, err := runStatements(t, `@trigger(event="probe.fired")
automation early {
  args {
    stop any
  }
  if args.stop == true {
    return "stopped"
  }
  builtin after()
}`, probe, map[string]any{"stop": true})
		if err != nil {
			t.Fatal(err)
		}
		if len(probe.callees()) != 0 {
			t.Fatalf("calls = %v after the return", probe.callees())
		}
		if !exec.Returned || exec.Output != "stopped" {
			t.Fatalf("returned %v, output %v", exec.Returned, exec.Output)
		}
	})
	t.Run("return of a call ends the run with the call's value", func(t *testing.T) {
		probe := newStmtProbe()
		probe.answers["decide"] = memql.NewResultWithOutput(map[string]any{"status": "queued"})
		exec, err := runStatements(t, `@trigger(event="probe.fired")
automation decides {
  return builtin decide()
}`, probe, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !exec.Returned || !reflect.DeepEqual(exec.Output, map[string]any{"status": "queued"}) {
			t.Fatalf("returned %v, output %#v", exec.Returned, exec.Output)
		}
	})
	t.Run("a body that runs off its end returns nothing", func(t *testing.T) {
		exec, err := runStatements(t, `@trigger(event="probe.fired")
automation plain {
  builtin one()
}`, newStmtProbe(), nil)
		if err != nil || exec.Returned {
			t.Fatalf("returned %v, err %v", exec.Returned, err)
		}
	})
}

func TestSequenceExpressionStatements(t *testing.T) {
	probe := newStmtProbe()
	_, err := runStatements(t, `@trigger(event="probe.fired")
automation computes {
  args {
    role any
  }
  role := args.role ?? ""
  status := role == "owner" ? "queued" : "needs_validation"
  builtin record(status: status, twice: [status, status].count())
}`, probe, map[string]any{"role": "owner"})
	if err != nil {
		t.Fatal(err)
	}
	got := probe.argsOf(t, "record", 0)
	if got["status"] != "queued" || got["twice"] != int64(2) {
		t.Fatalf("record got %v", got)
	}
}

func TestSequenceRowsReadFieldsDirectlyAndLeaveAsTheirMaps(t *testing.T) {
	probe := newStmtProbe()
	alice := map[string]any{"id": "v1:x:user:a", "concept": "v1:x:user", "payload": map[string]any{"email": "a@example.test", "active": true}}
	bob := map[string]any{"id": "v1:x:user:b", "concept": "v1:x:user", "payload": map[string]any{"email": "b@example.test", "active": false}}
	probe.answers["activeUsers"] = rowsResult(alice, bob)
	probe.answers["touch"] = rowsResult(alice)
	_, err := runStatements(t, `@trigger(event="probe.fired")
automation rowsRead {
  rows := query activeUsers()
  first := rows.first().email
  active := rows.where(r => r.active).count()
  written := mutation touch(id: rows.first().id)
  builtin report(first: first, active: active, rows: rows, one: rows.first(), writtenId: written.id)
}`, probe, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := probe.argsOf(t, "report", 0)
	if got["first"] != "a@example.test" || got["active"] != int64(1) || got["writtenId"] != "v1:x:user:a" {
		t.Fatalf("report got %v", got)
	}
	if !reflect.DeepEqual(got["rows"], []any{alice, bob}) {
		t.Errorf("a list of rows passed to a call arrived as %#v, want the row maps it was read as", got["rows"])
	}
	if !reflect.DeepEqual(got["one"], alice) {
		t.Errorf("a row passed to a call arrived as %#v, want its map", got["one"])
	}
}

func TestSequencePublishAndActionValues(t *testing.T) {
	probe := newStmtProbe()
	probe.answers["action:deploy"] = map[string]any{"ok": true, "changed": true, "result": map[string]any{"version": "1.2.3"}}
	_, err := runStatements(t, `@trigger(event="probe.fired")
automation acts {
  d := action deploy(v: 1)
  publish "deployed" { version: d.version, at: "now" }
}`, probe, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := probe.argsOf(t, "publish:deployed", 0)
	if got["version"] != "1.2.3" || got["at"] != "now" {
		t.Fatalf("the payload was %v: an action's value is its capability's result, not the envelope", got)
	}
}
