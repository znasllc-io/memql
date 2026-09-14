package automations

import (
	"context"
	"testing"
	"time"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// dot_access_logic_test.go pins #2542 item 4 END-TO-END through the
// LogicRunner: a multi-step logic body that plucks a scalar field off a
// step result via `.first().field` must load AND evaluate in process, never
// leaking into engine.Execute as an unknown function name.

// bundleStepRegistry is a step-registry stub that serves every engine-bound
// step a canned query-result Bundle, so a logic's query step binds a node
// list without a live engine / DB. An in-process expression step evaluates
// as the real query executor evaluates it (inProcessStep). It records the
// engine-bound step IDs.
type bundleStepRegistry struct {
	dispatched []string
	nodes      []any
}

func (r *bundleStepRegistry) Execute(ctx context.Context, step *Step, sc *StepContext) (*StepResult, error) {
	if res, handled, err := inProcessStep(ctx, step, sc); handled {
		return res, err
	}
	r.dispatched = append(r.dispatched, step.ID)
	now := time.Now()
	return &StepResult{
		StepId:      step.ID,
		Status:      "success",
		StartedAt:   now,
		CompletedAt: now,
		Result:      map[string]any{"Bundle": map[string]any{"nodes": r.nodes}},
	}, nil
}

func dotAccessLogicBody(t *testing.T, returnExpr string) *languageParser.AutomationDef {
	t.Helper()
	src := `@enabled
@description("pluck a scalar field off a step result (#2542 item 4)")
logic logicPluckStepField {
  args {
    id string @required
  }
  body {
    q := queryThing( id: args.id )
    return ` + returnExpr + `
  }
}
`
	return parseLogicBody(t, src)
}

// TestLogicRunner_RunLogic_DotAccessAfterFirst is the issue's exact shape:
// `q := query(...); return q.first().createdAt`. The query step is served
// by the stub registry; the `.first().createdAt` return resolves locally
// against the bound step result. Only the query step may reach the
// registry -- the return must never be dispatched into engine.Execute.
func TestLogicRunner_RunLogic_DotAccessAfterFirst(t *testing.T) {
	body := dotAccessLogicBody(t, "q.first().createdAt")
	registry := &bundleStepRegistry{nodes: []any{
		map[string]any{"id": "r1", "createdAt": "2026-01-01T00:00:00Z", "active": true},
		map[string]any{"id": "r2", "createdAt": "2026-02-02T00:00:00Z", "active": false},
	}}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "logicPluckStepField", body, map[string]any{"id": "x"})
	if err != nil {
		t.Fatalf("RunLogic returned error (#2542 item 4 -- .first().field must load and evaluate): %v", err)
	}
	if out != "2026-01-01T00:00:00Z" {
		t.Errorf("RunLogic return = %#v, want the first node's createdAt", out)
	}
	if len(registry.dispatched) != 1 || registry.dispatched[0] != "q" {
		t.Errorf("dispatched steps = %v, want exactly [q] (the return must resolve locally)", registry.dispatched)
	}
}

// TestLogicRunner_RunLogic_DotAccessAfterFirst_EmptyResult pins the empty
// edge: an empty query result makes `.first()` nil, and the field access
// yields a clean nil return -- no panic, no error.
func TestLogicRunner_RunLogic_DotAccessAfterFirst_EmptyResult(t *testing.T) {
	body := dotAccessLogicBody(t, "q.first().createdAt")
	registry := &bundleStepRegistry{nodes: []any{}}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "logicPluckStepField", body, map[string]any{"id": "x"})
	if err != nil {
		t.Fatalf("RunLogic over an empty result must degrade to nil, not error: %v", err)
	}
	if out != nil {
		t.Errorf("RunLogic return = %#v, want nil for an empty result", out)
	}
}

// TestLogicRunner_RunLogic_DotAccessAfterChain pins the chain form: a
// genuine collection chain over the step result followed by the field
// pluck (`q.skip(1).first().createdAt`) routes through the in-memory
// chain evaluator with the trailing field applied. The chain is
// deliberately lambda-less: a lambda in TERMINAL RETURN position hits the
// pre-existing compiler-serializer gap (`<<unsupported expression
// *ast.LambdaExpr>>`, the #2542 items 1-2 territory), which is not part
// of item 4; the lambda-carrying chain + trailing field is covered at the
// string-evaluator altitude in component/memql/dot_access_test.go.
func TestLogicRunner_RunLogic_DotAccessAfterChain(t *testing.T) {
	body := dotAccessLogicBody(t, "q.skip(1).first().createdAt")
	registry := &bundleStepRegistry{nodes: []any{
		map[string]any{"id": "r1", "createdAt": "2026-01-01T00:00:00Z", "active": false},
		map[string]any{"id": "r2", "createdAt": "2026-02-02T00:00:00Z", "active": true},
	}}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "logicPluckStepField", body, map[string]any{"id": "x"})
	if err != nil {
		t.Fatalf("RunLogic returned error (chain + trailing field must evaluate in-memory): %v", err)
	}
	if out != "2026-02-02T00:00:00Z" {
		t.Errorf("RunLogic return = %#v, want the second node's createdAt", out)
	}
	if len(registry.dispatched) != 1 || registry.dispatched[0] != "q" {
		t.Errorf("dispatched steps = %v, want exactly [q] (the chain return must resolve locally)", registry.dispatched)
	}
}

// TestLogicRunner_RunLogic_ArgsRootedDotAccess pins the ARGS-rooted sibling
// of the step pluck in a MULTI-STEP body: `q := query(...); return
// args.rows.first().createdAt`. The compiler emits the `_return` step as
// the $-prefixed string `$args.rows.first().createdAt`; it must resolve
// locally against the seeded caller args (via the chain evaluator's $-path
// base resolution), never dispatch into engine.Execute as an unparseable
// query. Only the query step may reach the registry.
func TestLogicRunner_RunLogic_ArgsRootedDotAccess(t *testing.T) {
	src := `@enabled
@description("pluck a scalar field off a caller-arg collection (#2542 item 4)")
logic logicPluckArgField {
  args {
    id string @required
    rows object @required
  }
  body {
    q := queryThing( id: args.id )
    return args.rows.first().createdAt
  }
}
`
	body := parseLogicBody(t, src)
	registry := &bundleStepRegistry{nodes: []any{}}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "logicPluckArgField", body, map[string]any{
		"id": "x",
		"rows": []any{
			map[string]any{"id": "r1", "createdAt": "2026-01-01T00:00:00Z"},
			map[string]any{"id": "r2", "createdAt": "2026-02-02T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("RunLogic returned error (args-rooted .first().field must resolve locally): %v", err)
	}
	if out != "2026-01-01T00:00:00Z" {
		t.Errorf("RunLogic return = %#v, want the first arg row's createdAt", out)
	}
	if len(registry.dispatched) != 1 || registry.dispatched[0] != "q" {
		t.Errorf("dispatched steps = %v, want exactly [q] (the return must resolve locally)", registry.dispatched)
	}
}

// TestReturnDotAccessAfterAccessor pins the return shapes directly,
// table-driven across the accessor + field shapes over a bound step and over
// a caller-arg collection. An accessor over an empty collection is ABSENT; an
// unknown step is an unknown name, refused.
func TestReturnDotAccessAfterAccessor(t *testing.T) {
	newEval := func() *Evaluator {
		e := NewEvaluator()
		e.SetStepResult("rows", &StepResult{
			Status: "success",
			Result: map[string]any{"Bundle": map[string]any{"nodes": []any{
				map[string]any{"id": "a", "createdAt": "2026-01-01T00:00:00Z", "payload": map[string]any{"name": "alice"}},
				map[string]any{"id": "b", "createdAt": "2026-02-02T00:00:00Z", "payload": map[string]any{"name": "bob"}},
			}}},
		})
		e.SetStepResult("none", &StepResult{
			Status: "success",
			Result: map[string]any{"Bundle": map[string]any{"nodes": []any{}}},
		})
		e.SetCustom("args", map[string]any{
			"members": []any{
				map[string]any{"id": "m1", "joinedAt": "2025-05-05T00:00:00Z", "payload": map[string]any{"name": "mia"}},
				map[string]any{"id": "m2", "joinedAt": "2025-06-06T00:00:00Z", "payload": map[string]any{"name": "moe"}},
			},
			"empty": []any{},
		})
		return e
	}

	cases := []struct {
		name string
		expr string
		want any // absentWant: memql.Absent
	}{
		{"first then field", "rows.first().createdAt", "2026-01-01T00:00:00Z"},
		{"last then field", "rows.last().id", "b"},
		{"first then nested field", "rows.first().payload.name", "alice"},
		{"empty result first then field", "none.first().createdAt", memql.Absent},
		{"args root first then field", "args.members.first().joinedAt", "2025-05-05T00:00:00Z"},
		{"args root last then field", "args.members.last().id", "m2"},
		{"args root first then nested field", "args.members.first().payload.name", "mia"},
		{"args root empty then field", "args.empty.first().joinedAt", memql.Absent},
		{"chain then field", `rows.where(r => r.id == "b").first().createdAt`, "2026-02-02T00:00:00Z"},
		{"?? over an accessor", `rows.first().id ?? "fallback"`, "a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val, err := evalV1(newEval(), tc.expr)
			if err != nil {
				t.Fatalf("%s: %v", tc.expr, err)
			}
			if tc.want == memql.Absent {
				if !memql.IsAbsent(val) {
					t.Errorf("%s = %#v, want absent", tc.expr, val)
				}
				return
			}
			if val != tc.want {
				t.Errorf("%s = %#v, want %#v", tc.expr, val, tc.want)
			}
		})
	}

	if val, err := evalV1(newEval(), "notAStep.first().createdAt"); err == nil {
		t.Errorf("an unknown step read = %#v, want an unknown-name refusal", val)
	}
}
