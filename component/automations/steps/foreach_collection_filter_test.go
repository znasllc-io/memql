package steps

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
)

// collectingExecutor records the `item.id` of every item whose nested step
// actually runs, so a test can assert which items survived the forEach filter.
type collectingExecutor struct {
	seen []string
}

func (e *collectingExecutor) Execute(_ context.Context, step *automations.Step, stepCtx *Context) (*automations.StepResult, error) {
	if v, err := stepCtx.Evaluator.EvaluateStepReference("item.id"); err == nil {
		if s, ok := v.(string); ok {
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

func runForEachWithFilter(t *testing.T, eval *automations.Evaluator, items []any, filter string) []string {
	t.Helper()
	reg := NewRegistry()
	coll := &collectingExecutor{}
	reg.Register(automations.StepType("collect"), coll)

	exec := &ForEachExecutor{Registry: reg}
	eval.SetInput(items)

	step := &automations.Step{
		ID:   "loop",
		Type: automations.StepTypeForEach,
		ForEach: &automations.ForEachStepConfig{
			Source: "$input",
			As:     "item",
			Filter: filter,
			Do: []*automations.Step{
				{ID: "do", Type: automations.StepType("collect")},
			},
		},
	}
	if _, err := exec.Execute(context.Background(), step, &Context{Evaluator: eval}); err != nil {
		t.Fatalf("forEach execute: %v", err)
	}
	return coll.seen
}

// TestForEachFilter_CollectionChain pins gap 3a (#2318): a forEach filter that
// is a genuine collection / lambda chain over the bound item routes through the
// in-memory collection surface; only items whose chain predicate is truthy
// survive.
func TestForEachFilter_CollectionChain(t *testing.T) {
	items := []any{
		map[string]any{"id": "a", "tags": []any{"vip", "x"}},
		map[string]any{"id": "b", "tags": []any{"y"}},
		map[string]any{"id": "c", "tags": []any{"z", "vip"}},
	}
	seen := runForEachWithFilter(t, automations.NewEvaluator(), items, `item.tags.any(t => t == "vip")`)
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "c" {
		t.Errorf("filtered items = %v, want [a c] (the vip-tagged rows)", seen)
	}
}

// TestForEachFilter_CollectionChainOuterArg pins gap 3b (#2318) on the filter
// path: a forEach filter chain whose lambda body references an outer `args.X`
// resolves it against the caller args threaded into the evaluator.
func TestForEachFilter_CollectionChainOuterArg(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetCustom("args", map[string]any{"wanted": "gold"})

	items := []any{
		map[string]any{"id": "a", "tags": []any{"gold"}},
		map[string]any{"id": "b", "tags": []any{"silver"}},
		map[string]any{"id": "c", "tags": []any{"bronze", "gold"}},
	}
	seen := runForEachWithFilter(t, eval, items, `item.tags.any(t => t == args.wanted)`)
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "c" {
		t.Errorf("filtered items = %v, want [a c] (tags containing args.wanted=gold)", seen)
	}
}

// TestForEachFilter_LegacyStringConditionUnchanged pins that an ordinary
// string-condition filter keeps the legacy EvaluateCondition path -- #2318 must
// not regress existing forEach filters.
func TestForEachFilter_LegacyStringConditionUnchanged(t *testing.T) {
	items := []any{
		map[string]any{"id": "a", "env": "development"},
		map[string]any{"id": "b", "env": "staging"},
		map[string]any{"id": "c", "env": "development"},
	}
	seen := runForEachWithFilter(t, automations.NewEvaluator(), items, `item.env == "development"`)
	if len(seen) != 2 || seen[0] != "a" || seen[1] != "c" {
		t.Errorf("filtered items = %v, want [a c] (development rows)", seen)
	}
}
