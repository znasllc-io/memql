package steps

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"google.golang.org/protobuf/types/known/structpb"
)

// Regression suite for memql#1706: first-class event-context binding.
//
// Every event-trigger Logic reads the triggering event through its declared
// `event` input (e.g. `args.event.payload.partitionId`). The LogicRunner must
// thread that event into the per-step argument-resolution scope so the
// references resolve to their event-derived values inside the NESTED steps --
// not to empty/undefined (the #1706 failure shape, where the first nested
// step that feeds an `args.event.*` reference to a @required argument fails
// with "required argument <x> is missing").
//
// This test fires each affected Logic (loaded from the real embedded DSL
// tree) through the real LogicRunner with a representative triggering event,
// captures the FULLY-RESOLVED query/args every nested step would send to the
// engine, and asserts:
//
//  1. the event-derived values appear in the resolved nested-step args
//     (the event threaded all the way into step scope), and
//  2. no unresolved `args.event.payload.` reference TEXT leaks through (no
//     dropped-to-nil / literal-passthrough).
//
// It is DB-free: the LogicRunner dispatches each construct call through the
// step registry, so a capturing registry renders each engine call exactly as
// the real function executor does (ResolveV1Map + renderV1CallArgs) without
// ever calling engine.Execute. An expression statement never reaches it: the
// sequence runner evaluates it in process.

// capturingRegistry renders each step's outbound call exactly like the real
// executors and records it, returning a canned two-row result so
// result-dependent guards (`.empty()`, `.count() == 2`) let the downstream
// event-referencing steps run and be captured too.
type capturingRegistry struct{ resolved []string }

func twoRowResult() *memql.ExecuteResult {
	var nodes []*memqlv1.MemoryNode
	for i := 0; i < 2; i++ {
		p, _ := structpb.NewStruct(map[string]any{"name": fmt.Sprintf("row%d", i)})
		nodes = append(nodes, &memqlv1.MemoryNode{Id: fmt.Sprintf("v1:test:row:%d", i), Payload: p})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}
}

func (r *capturingRegistry) Execute(ctx context.Context, step *automations.Step, sc *automations.StepContext) (*automations.StepResult, error) {
	var q string
	switch {
	case step.Function != nil:
		resolved, err := sc.Evaluator.ResolveV1Map(ctx, step.Function.Args)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", step.ID, err)
		}
		q = step.Function.Name + "(" + renderV1CallArgs(resolved) + ")"
	default:
		q = fmt.Sprintf("<%s/%s>", step.Type, step.ID)
	}
	r.resolved = append(r.resolved, q)
	return &automations.StepResult{StepId: step.ID, Status: "success", Result: twoRowResult()}, nil
}

func bootEmbeddedEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	return eng
}

func TestEventContextThreadsIntoNestedSteps(t *testing.T) {
	eng := bootEmbeddedEngine(t)

	cases := []struct {
		logic string
		event map[string]any
		// wantValues must each appear in at least one resolved nested-step query.
		wantValues []string
	}{
		{
			logic: "releaseWorkspaceOnRunTerminal",
			event: map[string]any{"topic": "node.updated", "kind": "node.updated", "payload": map[string]any{
				"id": "run-7f3a", "status": "completed",
			}},
			wantValues: []string{"run-7f3a"},
		},
		// (conflictDetection, this suite's first fixture, published, so the
		// flip moved its statements into the automation that called it and
		// deleted the logic (D14, memql#5373). The automation reads its bound
		// args, not an event envelope.)
		// (generateResponse -- the cognition.response.requested fixture --
		// went with the cognition namespace in epic memql#4988. logicAutoJoinAI
		// moved to the product pack in B2 (#2038) alongside the `space`
		// concept; the pack's own load tests cover the moved logic.)
		// (logicEnsureDailySpaceOnAuthSession -- the coalesce-in-step-body
		// event-threading fixture, memql#1065 -- moved to the product pack in
		// #1976; the remaining core logics keep this coverage. The pack's
		// own load tests cover the moved logic.)
	}

	for _, tc := range cases {
		t.Run(tc.logic, func(t *testing.T) {
			fn, err := eng.Functions().Get(tc.logic)
			if err != nil || fn == nil {
				t.Fatalf("Functions().Get(%s): %v", tc.logic, err)
			}
			if fn.LogicBody == nil {
				t.Fatalf("%s has no statement body", tc.logic)
			}

			reg := &capturingRegistry{}
			runner := automations.NewLogicRunner(eng, reg, eng.Logger)
			if _, err := runner.RunLogicBody(context.Background(), tc.logic, fn.LogicBody, map[string]any{"event": tc.event}); err != nil {
				t.Fatalf("RunLogicBody(%s): %v", tc.logic, err)
			}

			if len(reg.resolved) == 0 {
				t.Fatalf("%s: no nested steps were dispatched", tc.logic)
			}
			joined := strings.Join(reg.resolved, "\n")
			t.Logf("%s resolved nested-step queries:\n%s", tc.logic, joined)

			// (1) Every event-derived value must have threaded into a nested step.
			for _, want := range tc.wantValues {
				if !strings.Contains(joined, want) {
					t.Errorf("%s: event-derived value %q did not thread into any nested step (the #1706 failure -- event.* resolved to empty); resolved:\n%s",
						tc.logic, want, joined)
				}
			}

			// (2) No unresolved event-reference TEXT may leak into a step's args.
			for _, leak := range []string{"args.event.payload", "event.payload"} {
				if strings.Contains(joined, leak) {
					t.Errorf("%s: unresolved event reference %q leaked into a nested step's args (must resolve to a value, never pass through as text):\n%s",
						tc.logic, leak, joined)
				}
			}
		})
	}
}
