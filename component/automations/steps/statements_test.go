package steps

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// statements_test.go -- a statement body's `for` and `parallel` through the
// real step registry (epic memql#5370, task memql#5372). Construct calls go
// to a probe registered for the function step type; the loop, the parallel
// and its block branches are the production executors.

type callProbe struct {
	mu      sync.Mutex
	calls   []string
	args    []map[string]any
	answers map[string]any
	fail    map[string]bool
	delay   map[string]time.Duration
}

func (p *callProbe) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	now := time.Now()
	res := &automations.StepResult{StepId: step.ID, StartedAt: now, CompletedAt: now}
	args, err := stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args)
	if err != nil {
		return res, err
	}
	name := step.Function.Name
	if d := p.delay[name]; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
	p.mu.Lock()
	p.calls = append(p.calls, name)
	p.args = append(p.args, args)
	answer, fails := p.answers[name], p.fail[name]
	p.mu.Unlock()
	if fails {
		res.Status = "failed"
		return res, fmt.Errorf("%s failed", name)
	}
	res.Status, res.Result = "success", answer
	return res, nil
}

func (p *callProbe) called() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.calls, ",")
}

func (p *callProbe) argsOf(name string) []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []map[string]any
	for i, c := range p.calls {
		if c == name {
			out = append(out, p.args[i])
		}
	}
	return out
}

func runStatementBody(t *testing.T, src string, probe *callProbe) (*automations.AutomationExecution, error) {
	t.Helper()
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(src, "test.memql")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !a.IsStatementBody() {
		t.Fatalf("%s is not a statement body", a.Name)
	}
	reg := NewRegistry()
	reg.Register(automations.StepTypeFunction, probe)
	ev := events.NewEvent("probe.fired", events.KindMessage, nil)
	return automations.NewExecutor(automations.ExecutorOptions{StepRegistry: reg}).ExecuteWithEvent(context.Background(), a, "test", &ev)
}

func rowResult(rows ...map[string]any) *memql.ExecuteResult {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return memql.NewResultWithOutput(out)
}

func row(id string, payload map[string]any) map[string]any {
	return map[string]any{"id": id, "concept": "v1:x:item", "payload": payload}
}

func TestStatementForBindsItsVariablePerIteration(t *testing.T) {
	probe := &callProbe{answers: map[string]any{
		"items": rowResult(row("i1", map[string]any{"n": int64(1), "keep": true}), row("i2", map[string]any{"n": int64(2), "keep": false}), row("i3", map[string]any{"n": int64(3), "keep": true})),
	}}
	exec, err := runStatementBody(t, `@trigger(event="probe.fired")
automation loops {
  rows := query items()
  for it in rows if it.keep {
    doubled := it.n * 2
    builtin touch(id: it.id, doubled: doubled)
  }
  builtin done(count: rows.count())
}`, probe)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	if got := probe.called(); got != "items,touch,touch,done" {
		t.Fatalf("calls = %s: the filter keeps i1 and i3, then the loop ends", got)
	}
	touches := probe.argsOf("touch")
	if touches[0]["id"] != "i1" || touches[0]["doubled"] != int64(2) || touches[1]["id"] != "i3" || touches[1]["doubled"] != int64(6) {
		t.Fatalf("touch got %v", touches)
	}
}

func TestStatementReturnInsideAForEndsTheBody(t *testing.T) {
	probe := &callProbe{answers: map[string]any{
		"items": rowResult(row("i1", map[string]any{"hit": false}), row("i2", map[string]any{"hit": true}), row("i3", map[string]any{"hit": true})),
	}}
	exec, err := runStatementBody(t, `@trigger(event="probe.fired")
automation finds {
  rows := query items()
  for it in rows {
    builtin look(id: it.id)
    if it.hit {
      return it.id
    }
  }
  builtin notFound()
}`, probe)
	if err != nil {
		t.Fatal(err)
	}
	if got := probe.called(); got != "items,look,look" {
		t.Fatalf("calls = %s: the return in the second iteration ends the loop and the body", got)
	}
	if !exec.Returned || exec.Output != "i2" {
		t.Fatalf("returned %v, output %v", exec.Returned, exec.Output)
	}
}

func TestStatementForOnErrorContinue(t *testing.T) {
	probe := &callProbe{
		answers: map[string]any{"items": rowResult(row("i1", map[string]any{}), row("i2", map[string]any{}))},
		fail:    map[string]bool{"touch": true},
	}
	exec, err := runStatementBody(t, `@trigger(event="probe.fired")
automation tolerant {
  rows := query items()
  for it in rows {
    builtin touch(id: it.id)
  } on error continue
  builtin after()
}`, probe)
	if err != nil || exec.Status != "completed" {
		t.Fatalf("status %s, err %v", exec.Status, err)
	}
	if got := probe.called(); got != "items,touch,touch,after" {
		t.Fatalf("calls = %s: each failed iteration is recorded and the loop goes on", got)
	}

	probe = &callProbe{
		answers: map[string]any{"items": rowResult(row("i1", map[string]any{}), row("i2", map[string]any{}))},
		fail:    map[string]bool{"touch": true},
	}
	exec, err = runStatementBody(t, `@trigger(event="probe.fired")
automation strict {
  rows := query items()
  for it in rows {
    builtin touch(id: it.id)
  }
  builtin after()
}`, probe)
	if err == nil || exec.Status != "failed" {
		t.Fatalf("status %s, err %v: without on error continue the first failed iteration fails the run", exec.Status, err)
	}
	if got := probe.called(); got != "items,touch" {
		t.Fatalf("calls = %s", got)
	}
}

func TestStatementParallelBranchesRunTheirOwnLists(t *testing.T) {
	probe := &callProbe{answers: map[string]any{"one": memql.NewResultWithOutput("a"), "two": memql.NewResultWithOutput("b")}}
	exec, err := runStatementBody(t, `@trigger(event="probe.fired")
automation fans {
  parallel {
    branch left {
      x := builtin one()
      builtin leftDone(x: x)
    }
    branch right {
      x := builtin two()
      builtin rightDone(x: x)
    }
  }
  builtin after()
}`, probe)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	if l := probe.argsOf("leftDone"); len(l) != 1 || l[0]["x"] != "a" {
		t.Errorf("leftDone got %v: each branch reads its own x", l)
	}
	if r := probe.argsOf("rightDone"); len(r) != 1 || r[0]["x"] != "b" {
		t.Errorf("rightDone got %v", r)
	}
	if !strings.HasSuffix(probe.called(), ",after") {
		t.Errorf("calls = %s: after runs once both branches finished", probe.called())
	}
}

func TestStatementParallelWaitAny(t *testing.T) {
	probe := &callProbe{
		answers: map[string]any{"fast": memql.NewResultWithOutput("f"), "slow": memql.NewResultWithOutput("s")},
		delay:   map[string]time.Duration{"slow": 2 * time.Second},
	}
	started := time.Now()
	exec, err := runStatementBody(t, `@trigger(event="probe.fired")
automation races {
  parallel {
    branch quick {
      builtin fast()
    }
    branch sluggish {
      builtin slow()
    }
  } wait any
  builtin after()
}`, probe)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("wait any waited %v for the slow branch", time.Since(started))
	}
	if !strings.Contains(probe.called(), "after") {
		t.Fatalf("calls = %s", probe.called())
	}
}
