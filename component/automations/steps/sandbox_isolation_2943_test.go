package steps

// sandbox_isolation_2943_test.go -- memql#2943.
//
// dryrun.go promised, in writing, that under the isolated tier a would-be
// write "never reaches engine.Execute, so zero rows land in the live graph".
// That was false. sandboxStepRegistry.Execute intercepted three things --
// mutation steps, webhook steps, and function steps whose function is a
// mutation or logic -- and its `default:` arm forwarded EVERYTHING ELSE to the
// production executors. Steps that reach a real side effect went with it:
//
//	emitConceptCard -> stepCtx.Engine.Execute(insert ...)
//	event           -> stepCtx.EventBus.Publish, on the LIVE bus
//	action          -> engine.ExecuteToolByName, a real capability call
//	automation      -> triggers another automation, unbounded
//
// And a second escape the issue did not list, which is worse because it
// defeats the interception that DID exist: ForEachExecutor, ParallelExecutor
// and SwitchExecutor each held the concrete *Registry and resolved their
// children through it. The sandbox wraps the registry at the
// StepExecutorRegistry seam, so a container delegated to production resolved
// its children against production too -- and `forEach { mutation }` wrote to
// the live graph even though `mutation` is exactly what the sandbox catches.
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
	"github.com/znasllc-io/memql/component/memql"
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
// been replaced by one shared recording executor.
func sandboxWithRecorder(t *testing.T, types ...automations.StepType) (*sandboxStepRegistry, *writeReachRecorder) {
	t.Helper()
	real := NewRegistry()
	rec := &writeReachRecorder{}
	for _, ty := range types {
		real.Register(ty, rec)
	}
	return newSandboxStepRegistry(real, nil, "sandbox:dryrun:2943", memql.DryRunModeIsolated, ""), rec
}

func newStepCtx() *automations.StepContext {
	return &automations.StepContext{Evaluator: automations.NewEvaluator()}
}

// TestSandboxInterceptsEveryWriteBearingStepType is the direct fix for the
// reported escape: each of these used to fall through `default:` to the
// production executor.
func TestSandboxInterceptsEveryWriteBearingStepType(t *testing.T) {
	for _, tc := range []struct {
		name string
		step *automations.Step
	}{
		{"emitConceptCard", &automations.Step{
			ID: "card", Type: automations.StepTypeEmitConceptCard,
			EmitConceptCard: &automations.EmitConceptCardStepConfig{
				CardType: "lead_captured", PartitionId: "space-1",
			},
		}},
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
// here is a `mutation` -- the one step type the sandbox always caught -- so a
// failure means the container, not the classification, is the hole.
func TestSandboxInterceptsWritesNestedInsideContainers(t *testing.T) {
	child := automations.Step{
		ID:   "write",
		Type: automations.StepTypeMutation,
		Mutation: &automations.MutationStepConfig{
			Concept: "v1:cognition:utterance",
			Payload: map[string]any{"text": "written during a dry run"},
		},
	}

	for _, tc := range []struct {
		name string
		step *automations.Step
	}{
		{"forEach", &automations.Step{
			ID: "loop", Type: automations.StepTypeForEach,
			ForEach: &automations.ForEachStepConfig{
				Source: "$input.items",
				As:     "item",
				Do:     []*automations.Step{&child},
			},
		}},
		{"parallel", &automations.Step{
			ID: "fan", Type: automations.StepTypeParallel,
			Parallel: &automations.ParallelStepConfig{
				Branches: []*automations.Step{&child},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox, rec := sandboxWithRecorder(t, automations.StepTypeMutation)

			stepCtx := newStepCtx()
			stepCtx.Evaluator.SetInput(map[string]any{"items": []any{"a", "b"}})

			if _, err := sandbox.Execute(context.Background(), tc.step, stepCtx); err != nil {
				t.Fatalf("dry-run of a %s step errored: %v", tc.name, err)
			}
			if reached := rec.reached(); len(reached) != 0 {
				t.Errorf("a mutation nested in a %s reached the PRODUCTION executor: %v.\n"+
					"The container resolved its children against the real registry instead of "+
					"the sandbox, so wrapping the outer seam bought nothing (memql#2943).",
					tc.name, reached)
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

// TestSandboxInterceptsAQueryStepThatWrites covers the case the step TYPE
// cannot settle. query.go runs its text through engine.Execute, which its own
// doc comment says "executes MemQL queries and mutations" -- so a `query:` step
// carrying an insert is a write wearing a read's label.
func TestSandboxInterceptsAQueryStepThatWrites(t *testing.T) {
	for _, tc := range []struct {
		name      string
		query     string
		wantReach bool // did it correctly reach the real (read) executor?
	}{
		{"a plain read is delegated", `concept=="v1:cognition:utterance"`, true},
		{"an insert is intercepted", `insert("v1:cognition:utterance", id="x", payload={})`, false},
		{"an update is intercepted", `update("v1:cognition:utterance", id="x", payload={})`, false},
		{"a delete is intercepted", `delete("v1:cognition:utterance", id="x")`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox, rec := sandboxWithRecorder(t, automations.StepTypeQuery)
			step := &automations.Step{
				ID:    "q",
				Type:  automations.StepTypeQuery,
				Query: &automations.QueryStepConfig{Query: tc.query},
			}

			if _, err := sandbox.Execute(context.Background(), step, newStepCtx()); err != nil {
				t.Fatalf("dry-run errored: %v", err)
			}
			reached := len(rec.reached()) > 0
			if reached != tc.wantReach {
				if tc.wantReach {
					t.Errorf("a read-only query was intercepted instead of run; reads are supposed "+
						"to execute for real and be metered. query=%q", tc.query)
				} else {
					t.Errorf("a WRITING query reached the production executor and its row landed in "+
						"the live graph. query=%q", tc.query)
				}
			}
		})
	}
}
