package steps

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/events"
)

// collectingExecutor records the `id` argument of every call that actually
// runs, so a test can assert which items survived the `for` filter.
type collectingExecutor struct {
	seen []string
}

func (e *collectingExecutor) Execute(ctx context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	if args, err := stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args); err == nil {
		if s, ok := args["id"].(string); ok {
			e.seen = append(e.seen, s)
		}
	}
	return &automations.StepResult{
		StepId:      step.ID,
		Status:      "success",
		StartedAt:   time.Now(),
		CompletedAt: time.Now(),
		Result:      "ok",
	}, nil
}

// runForEachWithFilter runs `for item in args.items if <filter>` over items
// through the executor, with the extra args declared, and returns the ids the
// loop body saw.
func runForEachWithFilter(t *testing.T, extraArgs string, payload map[string]any, filter string) []string {
	t.Helper()
	a, err := automations.NewLoader(automations.LoaderOptions{}).CompileSource(`@trigger(event="probe.fired")
automation loops {
  args {
    items any
`+extraArgs+`  }
  for item in args.items if `+filter+` {
    builtin collect(id: item.id)
  }
}`, "test.memql")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	coll := &collectingExecutor{}
	reg := NewRegistry()
	reg.Register(automations.StepTypeFunction, coll)
	ev := events.NewEvent("probe.fired", events.KindMessage, payload)
	if exec, err := automations.NewExecutor(automations.ExecutorOptions{StepRegistry: reg}).ExecuteWithEvent(context.Background(), a, "test", &ev); err != nil {
		t.Fatalf("run: %v (%s)", err, exec.Error)
	}
	return coll.seen
}

// TestForEachFilter_CollectionChain pins gap 3a (#2318): a `for` filter that
// is a collection / lambda chain over the loop's variable decides per item;
// only items whose chain predicate holds survive.
func TestForEachFilter_CollectionChain(t *testing.T) {
	items := []any{
		map[string]any{"id": "a", "tags": []any{"vip", "x"}},
		map[string]any{"id": "b", "tags": []any{"y"}},
		map[string]any{"id": "c", "tags": []any{"z", "vip"}},
	}
	seen := runForEachWithFilter(t, "", map[string]any{"items": items}, `item.tags.any(t => t == "vip")`)
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "c" {
		t.Errorf("filtered items = %v, want [a c] (the vip-tagged rows)", seen)
	}
}

// TestForEachFilter_CollectionChainOuterArg pins gap 3b (#2318) on the filter
// path: a `for` filter chain whose lambda body references an outer `args.X`
// resolves it against the run's args.
func TestForEachFilter_CollectionChainOuterArg(t *testing.T) {
	items := []any{
		map[string]any{"id": "a", "tags": []any{"gold"}},
		map[string]any{"id": "b", "tags": []any{"silver"}},
		map[string]any{"id": "c", "tags": []any{"bronze", "gold"}},
	}
	seen := runForEachWithFilter(t, "    wanted any\n", map[string]any{"items": items, "wanted": "gold"}, `item.tags.any(t => t == args.wanted)`)
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "c" {
		t.Errorf("filtered items = %v, want [a c] (tags containing args.wanted=gold)", seen)
	}
}

// TestForEachFilter_PlainComparison pins that an ordinary comparison filter
// decides per item too -- #2318 must not regress plain `for` filters.
func TestForEachFilter_PlainComparison(t *testing.T) {
	items := []any{
		map[string]any{"id": "a", "env": "development"},
		map[string]any{"id": "b", "env": "staging"},
		map[string]any{"id": "c", "env": "development"},
	}
	seen := runForEachWithFilter(t, "", map[string]any{"items": items}, `item.env == "development"`)
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "c" {
		t.Errorf("filtered items = %v, want [a c] (development rows)", seen)
	}
}
