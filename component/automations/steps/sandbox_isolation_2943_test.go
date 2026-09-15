package steps

// sandbox_isolation_2943_test.go -- memql#2943.
//
// dryrun.go promised, in writing, that under the isolated tier a would-be
// write "never reaches engine.Execute, so zero rows land in the live graph".
// That was false: sandboxStepRegistry.Execute intercepted the mutation calls,
// and its `default:` arm forwarded EVERYTHING ELSE to the production
// executors. Steps that reach a real side effect went with it:
//
//	event      -> stepCtx.EventBus.Publish, on the LIVE bus
//	action     -> engine.ExecuteToolByName, a real capability call
//	automation -> triggers another automation, unbounded
//
// And a second escape the issue did not list, which is worse because it
// defeats the interception that DID exist: a container resolved its children
// through the concrete *Registry. The sandbox wraps the registry at the
// StepExecutorRegistry seam, so a container delegated to production resolved
// its children against production too -- and a mutation inside a `for` wrote
// to the live graph even though a mutation is exactly what the sandbox
// catches.
//
// These tests assert the guarantee the issue said nobody had been able to
// write, by the only means that actually settles it: register a RECORDING
// executor as the production executor for the step type under test, run the
// step through the sandbox, and assert the production executor was never
// reached. A test that only inspected the manifest would pass while the write
// still happened.

import (
	"context"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

// writeReachRecorder stands in for a production executor and records every
// call. Reaching it during a dry-run IS the defect: in production these are
// the executors that call engine.Execute / EventBus.Publish.
type writeReachRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *writeReachRecorder) Execute(_ context.Context, step *automations.Step, _ *Context) (*automations.StepResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, step.ID)
	return &automations.StepResult{StepId: step.ID, Status: "success"}, nil
}

func (r *writeReachRecorder) reached() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// sandboxWithRecorder wraps a real registry in which the given step types have
// been replaced by one shared recording executor. The engine is booted with no
// database: the sandbox reads its function registry, never its rows.
func sandboxWithRecorder(t *testing.T, types ...automations.StepType) (*sandboxStepRegistry, *writeReachRecorder) {
	t.Helper()
	real := NewRegistry()
	rec := &writeReachRecorder{}
	for _, ty := range types {
		real.Register(ty, rec)
	}
	return newSandboxStepRegistry(real, bootEmbeddedEngine(t), "sandbox:dryrun:2943"), rec
}

func newStepCtx() *automations.StepContext {
	return &automations.StepContext{Evaluator: automations.NewEvaluator()}
}

func TestSandboxRefusesUnclassifiedBuiltin(t *testing.T) {
	sandbox, rec := sandboxWithRecorder(t, automations.StepTypeFunction)
	step := &automations.Step{ID: "send", Type: automations.StepTypeFunction,
		Function: &automations.FunctionStepConfig{Name: "unclassifiedBuiltin", Kind: "builtin"}}
	if _, err := sandbox.Execute(context.Background(), step, newStepCtx()); err == nil {
		t.Fatal("preview accepted an unclassified builtin")
	}
	if len(rec.reached()) != 0 {
		t.Fatal("builtin reached production executor")
	}
}

func TestSandboxClassifiesBuiltinByRegisteredExecutor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
	}{
		{"memqlDocs", true}, {"routerSetApiKey", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox, rec := sandboxWithRecorder(t, automations.StepTypeFunction)
			// A caller's kind cannot disguise an effect as a query.
			step := &automations.Step{ID: "probe", Type: automations.StepTypeFunction,
				Function: &automations.FunctionStepConfig{Name: tc.name, Kind: "query"}}
			_, err := sandbox.Execute(context.Background(), step, newStepCtx())
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v, err=%v", tc.allowed, err)
			}
			if (len(rec.reached()) > 0) != tc.allowed {
				t.Fatalf("production calls: %v", rec.reached())
			}
		})
	}
}

// TestSandboxInterceptsEveryWriteBearingStepType is the direct fix for the
// reported escape: each of these used to fall through `default:` to the
// production executor.
func TestSandboxInterceptsEveryWriteBearingStepType(t *testing.T) {
	for _, tc := range []struct {
		name string
		step *automations.Step
	}{
		{"event", &automations.Step{
			ID: "emit", Type: automations.StepTypeEvent,
			Event: &automations.EventStepConfig{Topic: "some.topic", Kind: "message"},
		}},
		{"action", &automations.Step{
			ID: "act", Type: automations.StepTypeAction,
		}},
		{"automation", &automations.Step{
			ID: "nested", Type: automations.StepTypeAutomation,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox, rec := sandboxWithRecorder(t, tc.step.Type)

			res, err := sandbox.Execute(context.Background(), tc.step, newStepCtx())
			if err != nil {
				t.Fatalf("dry-run of a %s step errored: %v", tc.name, err)
			}
			if res == nil || res.Status != "success" {
				t.Fatalf("expected a synthetic success so later steps can reference it, got %+v", res)
			}
			if reached := rec.reached(); len(reached) != 0 {
				t.Errorf("%s reached the PRODUCTION executor during a dry-run: %v.\n"+
					"dryrun.go promises the write never reaches engine.Execute; this is "+
					"the escape memql#2943 reported.", tc.name, reached)
			}
			// The operator approves against the manifest, so an intercepted
			// side effect must also be VISIBLE there. Silent interception
			// would trade one incomplete manifest for another.
			if got := len(sandbox.manifest().Mutations); got != 1 {
				t.Errorf("manifest recorded %d side effects, want 1 -- an intercepted write "+
					"that is not in the manifest leaves the approver reading an incomplete record", got)
			}
		})
	}
}

// TestSandboxInterceptsWritesNestedInsideContainers is the escape that made the
// existing interception ineffective rather than merely incomplete. The child
// here is a mutation call -- the one call the sandbox always caught -- so a
// failure means the container, not the classification, is the hole. Each
// container runs through the real executor, whose registry is the sandbox:
// its list runs on the executor's sequence runner.
func TestSandboxInterceptsWritesNestedInsideContainers(t *testing.T) {
	for name, src := range map[string]string{
		"for": `@trigger(event="probe.fired")
automation loops {
  args {
    items any
  }
  for item in args.items {
    mutation createUtterance(text: item)
  }
}`,
		"parallel": `@trigger(event="probe.fired")
automation fans {
  parallel {
    branch a {
      mutation createUtterance(text: "a")
    }
    branch b {
      mutation createUtterance(text: "b")
    }
  }
}`,
	} {
		t.Run(name, func(t *testing.T) {
			a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(src, "test.memql")
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			sandbox, rec := sandboxWithRecorder(t, automations.StepTypeFunction)
			ev := events.NewEvent("probe.fired", events.KindMessage, map[string]any{"items": []any{"a", "b"}})
			exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: sandbox, SandboxRun: true}).ExecuteWithEvent(context.Background(), a, "test", &ev)
			if err != nil {
				t.Fatalf("dry-run of a %s errored: %v (%s)", name, err, exec.Error)
			}
			if reached := rec.reached(); len(reached) != 0 {
				t.Errorf("a mutation nested in a %s reached the PRODUCTION executor: %v.\n"+
					"The container resolved its children against the real registry instead of "+
					"the sandbox, so wrapping the outer seam bought nothing (memql#2943).",
					name, reached)
			}
			if got := len(sandbox.manifest().Mutations); got != 2 {
				t.Errorf("manifest recorded %d writes, want the 2 the %s would have made", got, name)
			}
		})
	}
}

// TestSandboxRefusesAnUnclassifiedStepType pins the inverted default. The old
// `default:` arm forwarded anything unrecognised to production, which is how
// the escapes above arose in the first place: a step type added later was
// delegated by omission rather than by decision.
func TestSandboxRefusesAnUnclassifiedStepType(t *testing.T) {
	const madeUp automations.StepType = "someFutureSideEffectingStep"

	sandbox, rec := sandboxWithRecorder(t, madeUp)
	step := &automations.Step{ID: "future", Type: madeUp}

	_, err := sandbox.Execute(context.Background(), step, newStepCtx())
	if err == nil {
		t.Fatal("an unclassified step type was allowed through; the sandbox must fail closed, " +
			"because a step nobody has classified may write and the manifest would not show it")
	}
	if reached := rec.reached(); len(reached) != 0 {
		t.Errorf("an unclassified step reached the PRODUCTION executor: %v", reached)
	}
}

// TestSandboxDecidesACallByItsKind: a call statement names its callee's kind,
// and the kind decides -- a query is a read, run for real and metered; a
// mutation is a write, recorded and never performed.
func TestSandboxDecidesACallByItsKind(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		wantReach bool // did it reach the real (read) executor?
	}{
		{"query", true},
		{"mutation", false},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			sandbox, rec := sandboxWithRecorder(t, automations.StepTypeFunction)
			step := &automations.Step{
				ID:       "call",
				Type:     automations.StepTypeFunction,
				Function: &automations.FunctionStepConfig{Name: "utterances", Kind: tc.kind},
			}
			if err := automations.PrepareExpressions(&automations.Automation{Name: "probe", Steps: []*automations.Step{step}}); err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if _, err := sandbox.Execute(context.Background(), step, newStepCtx()); err != nil {
				t.Fatalf("dry-run errored: %v", err)
			}
			if reached := len(rec.reached()) > 0; reached != tc.wantReach {
				if tc.wantReach {
					t.Errorf("a query was intercepted instead of run; reads are supposed to execute for real and be metered")
				} else {
					t.Errorf("a MUTATION call reached the production executor and its row landed in the live graph")
				}
			}
		})
	}
}
