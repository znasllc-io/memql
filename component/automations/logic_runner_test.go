package automations

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

// recordingStepRegistry is a minimal StepExecutorRegistry stub for the
// logic-runner end-to-end tests. Every step that reaches it -- a construct
// call, the one kind of statement that would reach the engine -- is recorded
// and answered with a canned success result, so a logic's calls run without a
// live engine / DB. An expression or return statement never reaches it: the
// sequence runner evaluates those in process. `dispatched` is therefore
// exactly the statements that would make an engine round trip.
type recordingStepRegistry struct {
	dispatched []string
}

func (r *recordingStepRegistry) Execute(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	r.dispatched = append(r.dispatched, step.ID)
	now := time.Now()
	return &StepResult{
		StepId:      step.ID,
		Status:      "success",
		StartedAt:   now,
		CompletedAt: now,
		Result:      map[string]any{"ok": true},
	}, nil
}

// TestLogicRunner_BindsCallerArgsAndTheAmbientRoots pins the evaluator a
// logic's statements read: the caller's args under `args` -- an event the
// logic acts on arrives as an argument and is read args.event -- and the
// ambient roots every body may read, with the denying actor when the call
// carries no auth context.
func TestLogicRunner_BindsCallerArgsAndTheAmbientRoots(t *testing.T) {
	r := NewLogicRunner(nil, nil, nil)
	ev := r.newEvaluatorForLogic(context.Background(), map[string]any{
		"event": map[string]any{
			"topic":   "node.created",
			"payload": map[string]any{"activePartitionId": "space-xyz", "id": "user-2"},
		},
		"partitionId": "space-abc",
	})
	for src, want := range map[string]any{
		`args.partitionId`:                     "space-abc",
		`args.event.payload.activePartitionId`: "space-xyz",
		`actor.isClusterOwner`:                 false,
	} {
		val, err := evalV1(ev, src)
		if err != nil {
			t.Fatalf("evaluate %s: %v", src, err)
		}
		if val != want {
			t.Errorf("%s = %#v, want %#v", src, val, want)
		}
	}
	if val, err := evalV1(ev, `now`); err != nil || val == "" {
		t.Errorf("now = %#v (err %v), want the run clock", val, err)
	}
}

// TestLogicRunner_ReturnsOverAQuerysRows pins the `return X.count()` family:
// `<name>.<method>()` in a logic's return resolves against the rows the
// query statement bound, in process -- the path revokeExpiredDelegations,
// purgeExpiredArchivedSpaces and the rest of the sweeps take.
func TestLogicRunner_ReturnsOverAQuerysRows(t *testing.T) {
	rows := []map[string]any{
		{"id": "d1", "payload": map[string]any{}},
		{"id": "d2", "payload": map[string]any{}},
		{"id": "d3", "payload": map[string]any{}},
	}
	for ret, want := range map[string]any{
		"expiredDelegations.count()": int64(3),
		"expiredDelegations.empty()": false,
		"expiredDelegations.first()": rows[0],
	} {
		got := runChainLogic(t, `logic sweep {
  expiredDelegations := query expired()
  return `+ret+`
}`, map[string]any{"expired": rowsResult(rows...)})
		if !reflect.DeepEqual(got, want) {
			t.Errorf("return %s = %#v, want %#v", ret, got, want)
		}
	}
}

// TestLogicRunner_ReturnsTheRowAMutationWrote pins the memql#363 regression
// fix: a `return nodeRecord` after a `nodeRecord := mutation ...` statement
// resolves to the row the mutation wrote, never to a spec or a literal.
func TestLogicRunner_ReturnsTheRowAMutationWrote(t *testing.T) {
	node := map[string]any{"id": "v1:cluster:node:bff-local", "payload": map[string]any{"health": "ok"}}
	got := runChainLogic(t, `logic registerNode {
  nodeRecord := mutation createNode(id: "bff-local")
  return nodeRecord
}`, map[string]any{"createNode": rowsResult(node)})
	if !reflect.DeepEqual(got, node) {
		t.Fatalf("return nodeRecord = %#v, want the row the mutation wrote", got)
	}
}

// TestLogicRunner_RefusesWithoutARegistry pins that a runner wired with no
// step registry refuses a call rather than panicking.
func TestLogicRunner_RefusesWithoutARegistry(t *testing.T) {
	_, body := compiledLogic(t, `logic x {
  return 1
}`)
	_, err := NewLogicRunner(nil, nil, nil).RunLogicBody(context.Background(), "x", body, nil)
	if err == nil || !strings.Contains(err.Error(), "no step registry") {
		t.Fatalf("want the no-step-registry refusal, got %v", err)
	}
}

// TestLogicRunner_SeedThenLiteralReturn reproduces the exact
// staging-confirmed memql#1090 shape end-to-end through RunLogicBody: a logic
// with a side-effect statement followed by `return 1`. The "1" was once
// dispatched through the step registry into engine.Execute, where the
// unbounded-query guard rejected it ("query must include at least one filter
// or relationship expression") -- the automation failed at boot. The literal
// resolves locally, and ONLY the side-effect statement reaches the registry.
func TestLogicRunner_SeedThenLiteralReturn(t *testing.T) {
	src := `@enabled
@description("repro of logicSeedKnowledgeDomains shape")
logic logicSeedKnowledgeDomains {
  args {
    event object @required
  }
  seed := builtin knowledgeSeedStandardDomains()
  return 1
}
`
	_, bodySteps := compiledLogic(t, src)
	registry := &recordingStepRegistry{}
	// A zero-value engine is enough: the side-effect step is served by the
	// stub registry, and the literal `return 1` is resolved locally before
	// any engine.Execute call. EventBus() on a zero-value engine is nil,
	// which RunLogicBody tolerates.
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogicBody(context.Background(), "logicSeedKnowledgeDomains", bodySteps, map[string]any{
		"event": map[string]any{"payload": map[string]any{"id": "u1"}},
	})
	if err != nil {
		t.Fatalf("RunLogicBody returned error (memql#1090 regression): %v", err)
	}
	if out != int64(1) {
		t.Errorf("return = %#v, want int64(1)", out)
	}
	// Only the side-effect step should have reached the registry; the
	// literal return must NOT have been dispatched into engine.Execute.
	if len(registry.dispatched) != 1 || registry.dispatched[0] != "seed" {
		t.Errorf("dispatched steps = %v, want exactly [seed] (the literal return must bypass the engine)", registry.dispatched)
	}
}

// TestLogicRunner_CollectionChainStatement pins gap 2 (#2317) END-TO-END: a
// multi-statement logic whose intermediate statement is a collection-method /
// lambda chain over a caller arg must LOAD and EVALUATE: `args.members`
// resolves, the `where(m => m.active)` filter runs in memory, `active` binds
// the filtered collection, and the trailing `active.count()` counts it.
// Neither statement reaches the step registry -- both resolve locally -- so a
// zero-value engine + stub registry is enough.
func TestLogicRunner_CollectionChainStatement(t *testing.T) {
	src := `@enabled
@description("collection-chain statement probe (#2317)")
logic logicProbe {
  args {
    members []object @required
  }
  active := args.members.where(m => m.active)
  return active.count()
}
`
	_, bodySteps := compiledLogic(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	members := []any{
		map[string]any{"name": "alice", "active": true},
		map[string]any{"name": "bob", "active": false},
		map[string]any{"name": "carol", "active": true},
	}
	out, err := r.RunLogicBody(context.Background(), "logicProbe", bodySteps, map[string]any{
		"members": members,
	})
	if err != nil {
		t.Fatalf("RunLogicBody returned error (#2317 collection-chain statement must load + evaluate): %v", err)
	}
	if !numericEquals(out, 2) {
		t.Errorf("return = %#v (%T), want 2 (the active member count)", out, out)
	}
	// Neither statement should have hit the registry -- both are resolved
	// in memory by the sequence runner.
	if len(registry.dispatched) != 0 {
		t.Errorf("dispatched steps = %v, want none (the collection chain + count resolve locally)", registry.dispatched)
	}
}

// numericEquals reports whether v is the integer n regardless of whether it
// arrived as int / int64 / float64 (the collection-count path can surface any
// of these depending on the resolver leg).
func numericEquals(v any, n int) bool {
	switch x := v.(type) {
	case int:
		return x == n
	case int64:
		return x == int64(n)
	case float64:
		return x == float64(n)
	default:
		return false
	}
}

// emptyQueryStepRegistry is a step-registry stub whose every call returns a
// read of zero rows, so a downstream `existing.empty()` guard evaluates true
// (the first-boot path where the seeded records do not yet exist and the
// conditional mutations must run). It records each call's step ID so a test
// can assert exactly which statements reached the engine.
type emptyQueryStepRegistry struct {
	dispatched []string
}

func (r *emptyQueryStepRegistry) Execute(_ context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	r.dispatched = append(r.dispatched, step.ID)
	now := time.Now()
	return &StepResult{
		StepId:      step.ID,
		Status:      "success",
		StartedAt:   now,
		CompletedAt: now,
		Result:      &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{}},
	}, nil
}

// TestLogicRunner_WelcomeCurriculumShape reproduces the second staging seed
// automation that failed at boot with the SAME filterless-query guard error --
// `logicSeedWelcomeCurriculum` (memql#1129): a lookup query, then CONDITIONAL
// mutations gated on `existing.empty()`, then a literal `return 1`. The
// conditionals run on the empty-query first-boot path, and the literal return
// is resolved locally (never reaching the unbounded-query guard).
func TestLogicRunner_WelcomeCurriculumShape(t *testing.T) {
	src := `@enabled
@description("repro of logicSeedWelcomeCurriculum shape")
logic logicSeedWelcomeCurriculum {
  args {
    event object @required
  }
  existing := query queryCurriculumBySlug(slug: "exampleapp.welcome.v1")
  if existing.empty() {
    insertCurriculum := mutation mutationCreateCurriculum(
      curriculumId: "v1:curriculum:curriculum:exampleapp-welcome-v1",
      slug: "exampleapp.welcome.v1",
      name: "Welcome to ExampleApp",
      version: 1,
      active: true
    )
    insertGreeting := mutation mutationCreateSegment(
      segmentId: "v1:curriculum:segment:exampleapp-welcome-v1-greeting",
      curriculumId: "v1:curriculum:curriculum:exampleapp-welcome-v1",
      slug: "greeting",
      recommendedSteps: "[{\"name\":\"uiHighlight\",\"arguments\":{\"target\":null}}]",
      orderHint: 1
    )
  }
  return 1
}
`
	_, bodySteps := compiledLogic(t, src)
	registry := &emptyQueryStepRegistry{}
	// A zero-value engine is enough: the lookup and the conditional mutations
	// are served by the stub registry, and the literal `return 1` is resolved
	// locally.
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogicBody(context.Background(), "logicSeedWelcomeCurriculum", bodySteps, map[string]any{
		"event": map[string]any{"payload": map[string]any{"id": "u1"}},
	})
	if err != nil {
		t.Fatalf("RunLogicBody returned error (memql#1129 regression -- filterless-query guard must not trip on the welcome-curriculum shape): %v", err)
	}
	if out != int64(1) {
		t.Errorf("return = %#v, want int64(1)", out)
	}
	// The lookup + BOTH conditional mutations reached the registry (the
	// empty-query first-boot path runs the branch); the literal return did not.
	if want := []string{"existing", "insertCurriculum", "insertGreeting"}; !reflect.DeepEqual(registry.dispatched, want) {
		t.Fatalf("dispatched = %v, want %v (the literal return must bypass the engine; the branch must run on an empty read)", registry.dispatched, want)
	}
}

// TestLogicRunner_TerminalReturnArithmetic pins #2542 item 1 END-TO-END: a
// logic whose terminal return is binary arithmetic over the names its prior
// statements bound must LOAD AND EVALUATE (both operands resolve in process
// under the #2316 numeric rules). Nothing reaches the engine: the `??`
// statements and the return all evaluate in process.
func TestLogicRunner_TerminalReturnArithmetic(t *testing.T) {
	src := `@enabled
@description("aov-style terminal-return arithmetic (#2542)")
logic aov {
  args {
    revenue int @required
    orders int @required
  }
  r := args.revenue ?? 0
  o := args.orders ?? 1
  return r / o
}
`
	_, bodySteps := compiledLogic(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogicBody(context.Background(), "aov", bodySteps, map[string]any{
		"revenue": 100,
		"orders":  4,
	})
	if err != nil {
		t.Fatalf("RunLogicBody returned error (#2542 terminal-return arithmetic must evaluate): %v", err)
	}
	if !numericEquals(out, 25) {
		t.Errorf("return = %#v (%T), want 25 (100 / 4)", out, out)
	}
	if len(registry.dispatched) != 0 {
		t.Errorf("dispatched steps = %v, want none (coalesce + arithmetic resolve locally)", registry.dispatched)
	}
}

// Division by zero must surface as a clean logic error naming the failure,
// never a panic and never a silent nil return.
func TestLogicRunner_TerminalReturnArithmetic_DivisionByZero(t *testing.T) {
	src := `@enabled
@description("division by zero surfaces cleanly")
logic ratio {
  args {
    a int @required
    b int @required
  }
  x := args.a ?? 0
  y := args.b ?? 0
  return x / y
}
`
	_, bodySteps := compiledLogic(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	_, err := r.RunLogicBody(context.Background(), "ratio", bodySteps, map[string]any{"a": 10, "b": 0})
	if err == nil {
		t.Fatalf("RunLogicBody succeeded on x / 0; want a division-by-zero error")
	}
	if !strings.Contains(err.Error(), "division_by_zero") {
		t.Errorf("error = %q, want the division_by_zero refusal", err.Error())
	}
}

// Modulo requires whole-number operands: a fractional operand must surface
// the operand_type refusal, not compute a bogus value.
func TestLogicRunner_TerminalReturnArithmetic_FloatModulo(t *testing.T) {
	src := `@enabled
@description("float modulo surfaces cleanly")
logic remainder {
  args {
    a float @required
    b int @required
  }
  x := args.a ?? 0
  y := args.b ?? 1
  return x % y
}
`
	_, bodySteps := compiledLogic(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	_, err := r.RunLogicBody(context.Background(), "remainder", bodySteps, map[string]any{"a": 10.5, "b": 3})
	if err == nil {
		t.Fatalf("RunLogicBody succeeded on float %% int; want an integer-operands error")
	}
	if !strings.Contains(err.Error(), "needs whole numbers") {
		t.Errorf("error = %q, want the operand_type whole-numbers refusal", err.Error())
	}
}

// TestLogicRunner_TerminalReturnComparison pins #2542 item 5 END-TO-END: a
// logic whose terminal return is an expression-led comparison over a computed
// intermediate must EVALUATE to the boolean, both operands resolved locally
// and the ordering operator applied, rather than reaching the step registry.
// Nothing reaches the registry: the arithmetic statement and the comparison
// return both resolve locally.
func TestLogicRunner_TerminalReturnComparison(t *testing.T) {
	src := `@enabled
@description("expression-led comparison terminal return (#2542 item 5)")
logic isProfitable {
  args {
    revenue int @required
    cost int @required
  }
  delta := args.revenue - args.cost
  return delta - 5 > 0
}
`
	_, bodySteps := compiledLogic(t, src)

	cases := []struct {
		name    string
		revenue int
		cost    int
		want    bool
	}{
		{"positive", 20, 3, true},   // 20-3=17; 17-5=12; 12 > 0 -> true
		{"negative", 20, 18, false}, // 20-18=2; 2-5=-3; -3 > 0 -> false
		{"boundary", 20, 15, false}, // 20-15=5; 5-5=0; 0 > 0 -> false
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			registry := &recordingStepRegistry{}
			r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)
			out, err := r.RunLogicBody(context.Background(), "isProfitable", bodySteps, map[string]any{
				"revenue": tc.revenue,
				"cost":    tc.cost,
			})
			if err != nil {
				t.Fatalf("RunLogicBody (#2542 comparison terminal return must evaluate): %v", err)
			}
			if out != tc.want {
				t.Errorf("return = %#v (%T), want %v", out, out, tc.want)
			}
			if len(registry.dispatched) != 0 {
				t.Errorf("dispatched steps = %v, want none (arithmetic statement + comparison return resolve locally)", registry.dispatched)
			}
		})
	}
}

// TestLogicRunner_TerminalReturnComparison_LiteralLed pins the literal-led
// shape (`0 < delta`) -- the other half of the expression-led comparison
// grammar (the parser's non-identifier-led left side). It must evaluate in
// process as well, never dispatch to the registry.
func TestLogicRunner_TerminalReturnComparison_LiteralLed(t *testing.T) {
	src := `@enabled
@description("literal-led comparison terminal return (#2542 item 5)")
logic hasSurplus {
  args {
    revenue int @required
    cost int @required
  }
  delta := args.revenue - args.cost
  return 0 < delta
}
`
	_, bodySteps := compiledLogic(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogicBody(context.Background(), "hasSurplus", bodySteps, map[string]any{"revenue": 10, "cost": 4})
	if err != nil {
		t.Fatalf("RunLogicBody (literal-led comparison must evaluate): %v", err)
	}
	if out != true {
		t.Errorf("return = %#v (%T), want true (0 < 6)", out, out)
	}
	if len(registry.dispatched) != 0 {
		t.Errorf("dispatched steps = %v, want none", registry.dispatched)
	}
}

// TestLogicRunner_DateBuiltinStepValueAndReturn pins #2541 END-TO-END: a date
// builtin bound as a statement's value (`delta := daysBetween(...)`) and read
// back through the terminal return. This is the day-delta half of the issue's
// minimal date surface (streak logic: "is today exactly one day after the
// last").
func TestLogicRunner_DateBuiltinStepValueAndReturn(t *testing.T) {
	src := `@enabled
@description("day-delta between two datetimes (#2541)")
logic streakDelta {
  args {
    prev string @required
    curr string @required
  }
  delta := daysBetween(args.prev, args.curr)
  return delta
}
`
	_, bodySteps := compiledLogic(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogicBody(context.Background(), "streakDelta", bodySteps, map[string]any{
		"prev": "2026-07-13T22:00:00Z",
		"curr": "2026-07-14T22:00:00Z",
	})
	if err != nil {
		t.Fatalf("RunLogicBody returned error (#2541 date-builtin statement value must evaluate): %v", err)
	}
	if !numericEquals(out, 1) {
		t.Errorf("return = %#v (%T), want 1 (one day apart)", out, out)
	}
	if len(registry.dispatched) != 0 {
		t.Errorf("dispatched steps = %v, want none (the date builtin resolves locally)", registry.dispatched)
	}
}

// A date builtin in the TERMINAL RETURN position, over a prior statement's
// value.
func TestLogicRunner_DateBuiltinTerminalReturn(t *testing.T) {
	src := `@enabled
@description("addDuration in terminal return (#2541)")
logic boundary {
  args {
    start string @required
  }
  seed := args.start ?? ""
  return addDuration(seed, "P1D")
}
`
	_, bodySteps := compiledLogic(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	out, err := r.RunLogicBody(context.Background(), "boundary", bodySteps, map[string]any{
		"start": "2026-03-10",
	})
	if err != nil {
		t.Fatalf("RunLogicBody returned error (#2541 date builtin in terminal return must evaluate): %v", err)
	}
	if out != "2026-03-11T00:00:00Z" {
		t.Errorf("return = %#v, want 2026-03-11T00:00:00Z", out)
	}
}

// A date builtin nested INSIDE terminal-return arithmetic:
// `return daysBetween(a, b) / 7` (weeks between two dates). Exercises the
// arithmetic operand walker's date-builtin dispatch.
func TestLogicRunner_DateBuiltinInsideArithmeticReturn(t *testing.T) {
	src := `@enabled
@description("daysBetween inside terminal-return arithmetic (#2541/#2542)")
logic weeksBetween {
  args {
    a string @required
    b string @required
  }
  seed := args.a ?? ""
  return daysBetween(args.a, args.b) / 7
}
`
	_, bodySteps := compiledLogic(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	out, err := r.RunLogicBody(context.Background(), "weeksBetween", bodySteps, map[string]any{
		"a": "2026-07-01",
		"b": "2026-07-15",
	})
	if err != nil {
		t.Fatalf("RunLogicBody returned error: %v", err)
	}
	if !numericEquals(out, 2) {
		t.Errorf("return = %#v (%T), want 2 (14 days / 7)", out, out)
	}
}

// TestLogicRunner_CompiledReturnIsCanonicalSource pins the serializer half of
// #2541/#2542: a compiled return carries canonical v1 source -- re-parseable,
// never an `<<unsupported expression %T>>` marker, and never rewritten into
// another dialect (no `$args`).
func TestLogicRunner_CompiledReturnIsCanonicalSource(t *testing.T) {
	cases := []struct {
		name       string
		ret        string
		wantReturn string
	}{
		{"arithmetic", "return r / o", "r / o"},
		{"nested_arithmetic", "return (r * 100) / o", "(r * 100) / o"},
		{"addDuration", `return addDuration(r, "P1D")`, `addDuration(r, "P1D")`},
		{"daysBetween_args", "return daysBetween(args.a, args.b)", "daysBetween(args.a, args.b)"},
		{"date_in_arithmetic", "return daysBetween(args.a, args.b) / 7", "daysBetween(args.a, args.b) / 7"},
		{"ternary_over_chain", "return r.count() > 0 ? r.count() : 0", "r.count() > 0 ? r.count() : 0"},
		{"lambda_chain", "return o.where(m => m.vip).count()", "o.where(m => m.vip).count()"},
		{"projection", "return o.groupBy(s => s.worker).select(g => {worker: g.key, n: g.items.count()})", "o.groupBy(s => s.worker).select(g => {worker: g.key, n: g.items.count()})"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, steps := compiledLogic(t, `@enabled
@description("round-trip probe")
logic probe {
  args {
    a string @required
    b string @required
  }
  r := args.a ?? ""
  o := args.b ?? ""
  `+tc.ret+`
}`)
			var got string
			for _, s := range steps {
				if s["type"] == "return" {
					ret, _ := s["return"].(map[string]any)
					got, _ = ret["value"].(string)
				}
			}
			if got == "" {
				t.Fatalf("no return step in the compiled body: %v", steps)
			}
			if strings.Contains(got, "<<unsupported") || strings.Contains(got, "$args") {
				t.Fatalf("return = %q is not canonical v1 source", got)
			}
			if got != tc.wantReturn {
				t.Errorf("return = %q, want %q", got, tc.wantReturn)
			}
		})
	}
}
