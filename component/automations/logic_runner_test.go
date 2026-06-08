package automations

import (
	"context"
	"strings"
	"testing"
	"time"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// recordingStepRegistry is a minimal StepExecutorRegistry stub for the
// logic-runner end-to-end tests. It records every dispatched step ID and
// returns a canned success result, so a Logic body's intermediate
// side-effect steps run without a live engine / DB. The `_return` step
// is resolved by RunLogic's local-expression path before it ever reaches
// the registry, so it never lands here for a literal return.
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

// parseLogicBody is the test helper. Given a Logic source string, it
// runs the struct-form normaliser + parser and returns the parsed
// *AutomationDef body that the function loader would store as
// fn.LogicSteps for a multi-step Logic. This is the same shape
// LogicRunner.RunLogic receives in production.
func parseLogicBody(t *testing.T, src string) *languageParser.AutomationDef {
	t.Helper()

	normalised, err := languageParser.NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	lexer := languageParser.NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	parser := languageParser.NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file, ok := ast.(*languageParser.File)
	if !ok {
		t.Fatalf("expected *File, got %T", ast)
	}
	for _, def := range file.Definitions {
		fd, ok := def.(*languageParser.FunctionDef)
		if !ok {
			continue
		}
		if fd.Type != languageParser.FunctionTypeLogic {
			continue
		}
		body, ok := fd.Body.(*languageParser.AutomationDef)
		if !ok {
			t.Fatalf("expected *AutomationDef body, got %T", fd.Body)
		}
		return body
	}
	t.Fatalf("no logic function found in source")
	return nil
}

// TestLogicRunner_CompilesMultiStepBody pins F.5's compile path:
// a parsed multi-step Logic body translates cleanly to a runtime
// *Automation through the compiler + JSON loader. This is the
// path RunLogic walks before dispatching steps via the registry.
// The runtime *Automation must carry one runtime *Step per parsed
// step (excluding the synthetic `_return` step, which compiles to
// its own `_return` step on the output side) and the steps must
// arrive in topological dependency order.
func TestLogicRunner_CompilesMultiStepBody(t *testing.T) {
	src := `@useQuery(queryFoo, queryBar)
@description("test")
logic doStuff {
  args {
    spaceId  string  @required
  }
  body {
    first := queryFoo({ spaceId: args.spaceId })
    second := queryBar({ id: first.First().id })
    return coalesce(second.First(), first.First())
  }
}`
	body := parseLogicBody(t, src)

	runner := &LogicRunner{
		compiler: nil, // populated below
		loader:   nil,
	}
	// Populate via the real constructor wiring (engine + registry
	// optional for this test; compileBodyToAutomation doesn't touch
	// them) so the same code path RunLogic uses in production runs.
	r := NewLogicRunner(nil, nil, nil)
	runner.compiler = r.compiler
	runner.loader = r.loader

	auto, err := runner.compileBodyToAutomation("doStuff", body)
	if err != nil {
		t.Fatalf("compileBodyToAutomation: %v", err)
	}
	if auto == nil {
		t.Fatalf("expected non-nil *Automation")
	}
	if len(auto.Steps) < 2 {
		t.Fatalf("expected at least 2 steps (first, second [+ _return]); got %d", len(auto.Steps))
	}

	// Topological order means `first` lands before `second`.
	idsInOrder := make([]string, 0, len(auto.Steps))
	for _, s := range auto.Steps {
		if s != nil {
			idsInOrder = append(idsInOrder, s.ID)
		}
	}
	firstIdx, secondIdx := -1, -1
	for i, id := range idsInOrder {
		switch id {
		case "first":
			firstIdx = i
		case "second":
			secondIdx = i
		}
	}
	if firstIdx < 0 || secondIdx < 0 {
		t.Fatalf("expected both `first` and `second` steps in the compiled output, got %v", idsInOrder)
	}
	if firstIdx >= secondIdx {
		t.Errorf("expected `first` before `second` (it's a dependency); got order %v", idsInOrder)
	}
}

// TestLogicRunner_HandlesConditionalSteps pins that a Logic body
// using the `name := if <cond> { funcCall(...) }` shape -- which is
// the dominant pattern across cluster/cognition/identity logics --
// compiles into a runtime *Step carrying a non-empty Condition
// string. The runner's step loop evaluates the condition before
// dispatching, mirroring the automation executor's behaviour.
func TestLogicRunner_HandlesConditionalSteps(t *testing.T) {
	src := `@useQuery(queryThing)
@useMutation(mutationCreateThing)
@description("test")
logic provisionThing {
  args {
    name  string  @required
  }
  body {
    existing := queryThing({ name: args.name })
    created := if existing.Empty() {
      mutationCreateThing({ name: args.name })
    }
    return coalesce(created, existing.First())
  }
}`
	body := parseLogicBody(t, src)

	r := NewLogicRunner(nil, nil, nil)
	auto, err := r.compileBodyToAutomation("provisionThing", body)
	if err != nil {
		t.Fatalf("compileBodyToAutomation: %v", err)
	}

	var createdStep *Step
	for _, s := range auto.Steps {
		if s != nil && s.ID == "created" {
			createdStep = s
			break
		}
	}
	if createdStep == nil {
		t.Fatalf("expected `created` step in compiled output")
	}
	if createdStep.Condition == "" {
		t.Errorf("expected `created` step to carry a Condition; got empty string")
	}
	if !strings.Contains(createdStep.Condition, "existing") {
		t.Errorf("expected condition to reference the `existing` step; got %q", createdStep.Condition)
	}
}

// TestLogicRunner_SeedsCallerArgsEverywhere pins the evaluator setup:
// caller args are addressable via $args, $ctx.input, and (when an
// `event` key is present) $event, so step expressions written for
// any of those forms resolve. This matters because real Logic bodies
// reference `args.event.payload.X` while step expressions sometimes
// compile to `$event.payload.X` after the compiler's rewrite.
func TestLogicRunner_SeedsCallerArgsEverywhere(t *testing.T) {
	r := NewLogicRunner(nil, nil, nil)
	args := map[string]any{
		"event": map[string]any{
			"payload": map[string]any{"id": "user-123"},
		},
		"spaceId": "space-abc",
	}
	evaluator := r.newEvaluatorForLogic(args)

	// `args` custom variable
	val, err := evaluator.EvaluateValue(`$args.spaceId`)
	if err != nil {
		t.Fatalf("evaluate $args.spaceId: %v", err)
	}
	if val != "space-abc" {
		t.Errorf("$args.spaceId = %#v, want %q", val, "space-abc")
	}

	// `event` custom variable plumbed from args
	val, err = evaluator.EvaluateValue(`$event.payload.id`)
	if err != nil {
		t.Fatalf("evaluate $event.payload.id: %v", err)
	}
	if val != "user-123" {
		t.Errorf("$event.payload.id = %#v, want %q", val, "user-123")
	}

	// `ctx.input` mirrors args (legacy form still supported)
	val, err = evaluator.EvaluateValue(`$ctx.input.spaceId`)
	if err != nil {
		t.Fatalf("evaluate $ctx.input.spaceId: %v", err)
	}
	if val != "space-abc" {
		t.Errorf("$ctx.input.spaceId = %#v, want %q", val, "space-abc")
	}
}

// TestLogicRunner_PreservesReturnStep pins the compile->JSON->loader
// round-trip: the compiler peels the parser's synthetic `_return`
// step out of the steps slice and emits it as a top-level
// `_return` JSON field, the automations.Loader has no struct field
// for it, and a vanilla unmarshal drops it on the floor. The
// runtime then refuses to run the Logic with "logic %q has no
// `_return` step (body must end with `return <expr>`)". Every
// multi-statement Logic across the DSL trees hit this -- including
// logicRevokeExpiredDelegations, the cluster/identity sweeps, etc.
// compileBodyToAutomation re-attaches a synthetic Step at the end
// of the slice; this test guards that fix.
func TestLogicRunner_PreservesReturnStep(t *testing.T) {
	src := `@useQuery(queryFoo)
@useMutation(mutationBar)
@description("repro")
logic logicSweep {
  args {
    now string @required
  }
  body {
    rows := queryFoo({ now: args.now })
    for item := range rows.Nodes() {
      tick := mutationBar({ id: item.id })
    }
    return rows.Len()
  }
}`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(nil, nil, nil)

	auto, err := r.compileBodyToAutomation("logicSweep", body)
	if err != nil {
		t.Fatalf("compileBodyToAutomation: %v", err)
	}

	var returnStep *Step
	for _, s := range auto.Steps {
		if s != nil && s.ID == "_return" {
			returnStep = s
			break
		}
	}
	if returnStep == nil {
		t.Fatalf("expected a _return step in compiled automation; got %d steps without one (the JSON round-trip drops the compiler's top-level `_return` field unless stitched back)",
			len(auto.Steps))
	}
	if returnStep.Type != StepTypeQuery {
		t.Errorf("_return step type = %v, want %v", returnStep.Type, StepTypeQuery)
	}
	if returnStep.Query == nil || strings.TrimSpace(returnStep.Query.Query) == "" {
		t.Errorf("_return step missing Query expression: %+v", returnStep.Query)
	}
}

// TestLogicRunner_TryEvaluateReturnLocally_PureStepMethod pins the
// pure step-method short-circuit: `<stepName>.<method>()` in a
// Logic's `return` expression resolves against the bound step
// result via the local Evaluator, bypassing engine.Execute (which
// doesn't recognise the dotted shape and would fail with
// `function "stepName.method" not found`).
//
// This is the path that lets logicRevokeExpiredDelegations,
// purgeExpired{ArchivedSpaces,PolicyTraces}, and the rest of the
// `return X.Len()` family run end-to-end.
func TestLogicRunner_TryEvaluateReturnLocally_PureStepMethod(t *testing.T) {
	evaluator := NewEvaluator()
	evaluator.SetStepResult("expiredDelegations", &StepResult{
		Status: "success",
		Result: map[string]any{
			"Bundle": map[string]any{
				"nodes": []any{
					map[string]any{"id": "d1"},
					map[string]any{"id": "d2"},
					map[string]any{"id": "d3"},
				},
			},
		},
	})

	// .Len() -> count of nodes
	val, handled, err := tryEvaluateReturnLocally("expiredDelegations.Len()", evaluator)
	if err != nil {
		t.Fatalf("tryEvaluateReturnLocally: %v", err)
	}
	if !handled {
		t.Fatalf("expected handled=true for `expiredDelegations.Len()`; bound step ID should be recognised")
	}
	if val != 3 {
		t.Errorf(".Len() = %#v, want 3", val)
	}

	// .Empty() -> false (3 nodes)
	val, handled, err = tryEvaluateReturnLocally("expiredDelegations.Empty()", evaluator)
	if err != nil {
		t.Fatalf("tryEvaluateReturnLocally: %v", err)
	}
	if !handled || val != false {
		t.Errorf(".Empty() = (handled=%v val=%#v); want (true, false)", handled, val)
	}

	// .First().id navigation
	val, handled, err = tryEvaluateReturnLocally("expiredDelegations.First()", evaluator)
	if err != nil {
		t.Fatalf("tryEvaluateReturnLocally: %v", err)
	}
	if !handled {
		t.Errorf(".First() should be handled locally")
	}
	if m, ok := val.(map[string]any); !ok || m["id"] != "d1" {
		t.Errorf(".First() = %#v, want first node {id: d1}", val)
	}
}

// TestLogicRunner_TryEvaluateReturnLocally_BareStepVariable pins the
// memql#363 regression fix: a `return nodeRecord` after a
// `nodeRecord := mutation...` step must resolve to the step's bound
// value, not surface as `unknown spec "nodeRecord"` when the engine's
// query parser treats the bare identifier as a spec name.
func TestLogicRunner_TryEvaluateReturnLocally_BareStepVariable(t *testing.T) {
	evaluator := NewEvaluator()
	evaluator.SetStepResult("nodeRecord", &StepResult{
		Status: "success",
		Result: map[string]any{
			"Bundle": map[string]any{
				"nodes": []any{map[string]any{"id": "v1:cluster:node:bff-local"}},
			},
		},
	})

	val, handled, err := tryEvaluateReturnLocally("nodeRecord", evaluator)
	if err != nil {
		t.Fatalf("tryEvaluateReturnLocally: %v", err)
	}
	if !handled {
		t.Fatalf("expected handled=true for bare step identifier `nodeRecord`")
	}
	bundle, ok := val.(map[string]any)
	if !ok {
		t.Fatalf("bare step-variable return: got %T, want map[string]any", val)
	}
	if _, ok := bundle["Bundle"]; !ok {
		t.Errorf("bare step-variable return: missing Bundle key, got %#v", bundle)
	}
}

// TestLogicRunner_TryEvaluateReturnLocally_FallsThrough pins that
// expressions which AREN'T pure step-method calls report handled=false
// so the caller falls back to engine.Execute. This protects compound
// expressions, mutation calls, and literals from being silently
// short-circuited.
func TestLogicRunner_TryEvaluateReturnLocally_FallsThrough(t *testing.T) {
	evaluator := NewEvaluator()
	evaluator.SetStepResult("rows", &StepResult{
		Status: "success",
		Result: map[string]any{"Bundle": map[string]any{"nodes": []any{}}},
	})

	tests := []struct {
		name string
		expr string
	}{
		{"compound coalesce", `coalesce(rows.First(), "fallback")`},
		{"mutation call", `mutationCreateThing({name: "x"})`},
		{"builtin call", `ensureDailySpaceForCaller({})`},
		{"unknown step", `notARealStep.Len()`},
		{"bare unknown identifier", `notARealStep`},
		{"single-segment call", `someFunc()`},
		{"empty expr", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, handled, err := tryEvaluateReturnLocally(tt.expr, evaluator)
			if err != nil {
				t.Errorf("expected no error for %q, got %v", tt.expr, err)
			}
			if handled {
				t.Errorf("expected handled=false for %q (should fall through to engine.Execute)", tt.expr)
			}
		})
	}
}

// TestLogicRunner_TryEvaluateBuiltinLocally_Coalesce pins the
// positional-builtin short-circuit (#362). Before this lands,
// `return coalesce(stepX, stepY.First())` and step-RHS
// `enabled := coalesce(args.X, true)` both fall through to
// engine.Execute, which fails the lookup with `function "coalesce"
// not found`. The local short-circuit evaluates each arg against
// the bound Evaluator (args via custom-var resolution, step refs
// via EvaluateStepReference, literals via strconv parsing) and
// applies the first-non-empty semantics.
func TestLogicRunner_TryEvaluateBuiltinLocally_Coalesce(t *testing.T) {

	t.Run("first non-empty arg wins", func(t *testing.T) {
		evaluator := NewEvaluator()
		val, handled, err := tryEvaluateBuiltinLocally(`coalesce("first", "second")`, evaluator)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !handled {
			t.Fatalf("expected handled=true for `coalesce(\"first\", \"second\")`")
		}
		if val != "first" {
			t.Errorf("val = %#v, want %q", val, "first")
		}
	})

	t.Run("empty string is skipped to fallback", func(t *testing.T) {
		evaluator := NewEvaluator()
		val, _, err := tryEvaluateBuiltinLocally(`coalesce("", "fallback")`, evaluator)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if val != "fallback" {
			t.Errorf("val = %#v, want %q", val, "fallback")
		}
	})

	t.Run("args.X.Y resolves via the custom-var path", func(t *testing.T) {
		// Mirrors cluster/logic.memql line 26:
		//   coalesce(args.event.payload.database.host, "localhost")
		evaluator := NewEvaluator()
		evaluator.SetCustom("args", map[string]any{
			"event": map[string]any{
				"payload": map[string]any{
					"database": map[string]any{"host": "live.host"},
				},
			},
		})
		val, handled, err := tryEvaluateBuiltinLocally(
			`coalesce(args.event.payload.database.host, "localhost")`,
			evaluator,
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !handled {
			t.Fatalf("expected handled=true")
		}
		if val != "live.host" {
			t.Errorf("val = %#v, want %q", val, "live.host")
		}
	})

	t.Run("missing args.X.Y falls through to literal fallback", func(t *testing.T) {
		evaluator := NewEvaluator()
		evaluator.SetCustom("args", map[string]any{
			"event": map[string]any{"payload": map[string]any{}},
		})
		val, _, err := tryEvaluateBuiltinLocally(
			`coalesce(args.event.payload.database.host, "localhost")`,
			evaluator,
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if val != "localhost" {
			t.Errorf("val = %#v, want %q (default)", val, "localhost")
		}
	})

	t.Run("bare step ref + step.method() return values", func(t *testing.T) {
		// Mirrors cluster/logic.memql line 52:
		//   return coalesce(clusterRecord, existing.First())
		// When the create-branch ran, clusterRecord is bound. When it
		// didn't, clusterRecord.Status=="skipped" and the fallback is
		// the first existing row.
		evaluator := NewEvaluator()
		evaluator.SetStepResult("clusterRecord", &StepResult{
			Status: "success",
			Result: map[string]any{"id": "new-cluster"},
		})
		val, _, err := tryEvaluateBuiltinLocally(
			`coalesce(clusterRecord, existing.First())`,
			evaluator,
		)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		// clusterRecord resolves to its StepResult — non-nil so coalesce
		// picks it. We just check it's non-nil because the bare-step
		// path returns the *StepResult itself.
		if val == nil {
			t.Fatalf("expected non-nil clusterRecord arg, got nil")
		}
	})

	t.Run("non-builtin name returns handled=false", func(t *testing.T) {
		evaluator := NewEvaluator()
		val, handled, err := tryEvaluateBuiltinLocally(`someUserFunc("a", "b")`, evaluator)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if handled {
			t.Errorf("expected handled=false for unknown name; got val=%#v", val)
		}
	})

	t.Run("bare step name with no parens returns handled=false", func(t *testing.T) {
		// The bare step-name case (no parens / no method) does NOT match
		// the `<name>(...)` shape; it falls through to engine.Execute.
		// This pins that the builtin matcher doesn't accidentally
		// swallow bare identifiers.
		evaluator := NewEvaluator()
		_, handled, _ := tryEvaluateBuiltinLocally("clusterRecord", evaluator)
		if handled {
			t.Errorf("expected handled=false for bare identifier")
		}
	})
}

// TestReconstructPositionalBuiltinCall pins the fix for the
// `getGA := coalesce(a, b)` step-ASSIGNMENT bug (#362 / autoJoinSI
// missing greeting). Positional-builtin assignments compile to
// StepTypeFunction; without reconstruction they fall to
// FunctionExecutor's named-arg serialization (`coalesce(0="a", 1="b")`)
// which the MemQL parser rejects with `expected ')', got "="`.
// reconstructPositionalBuiltinCall rebuilds the positional call string
// so the local Evaluator handles it instead of engine.Execute.
func TestReconstructPositionalBuiltinCall(t *testing.T) {
	t.Run("coalesce positional args reconstruct in order, literals preserved", func(t *testing.T) {
		fn := &FunctionStepConfig{
			Name: "coalesce",
			Args: map[string]any{"0": "getActiveGA", "1": `""`},
		}
		got, ok := reconstructPositionalBuiltinCall(fn)
		if !ok {
			t.Fatalf("expected ok=true for coalesce")
		}
		if got != `coalesce(getActiveGA, "")` {
			t.Errorf("got %q, want %q", got, `coalesce(getActiveGA, "")`)
		}
	})

	t.Run("non-positional-builtin name is not handled", func(t *testing.T) {
		fn := &FunctionStepConfig{Name: "queryUserById", Args: map[string]any{"0": "x"}}
		if _, ok := reconstructPositionalBuiltinCall(fn); ok {
			t.Errorf("expected ok=false for non-positional-builtin %q", fn.Name)
		}
	})

	t.Run("reconstructed coalesce evaluates locally (the bug repro)", func(t *testing.T) {
		evaluator := NewEvaluator()
		fn := &FunctionStepConfig{
			Name: "coalesce",
			Args: map[string]any{"0": `""`, "1": `"fallback"`},
		}
		callStr, ok := reconstructPositionalBuiltinCall(fn)
		if !ok {
			t.Fatalf("expected ok=true")
		}
		val, handled, err := tryEvaluateBuiltinLocally(callStr, evaluator)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !handled {
			t.Fatalf("expected handled=true for reconstructed %q", callStr)
		}
		if val != "fallback" {
			t.Errorf("val = %#v, want %q", val, "fallback")
		}
	})
}

// TestSplitTopLevelArgs pins the arg splitter -- the helper has to
// keep nested parens (`x.method()`, `inner(a, b)`), brackets, braces,
// and quoted strings as a single arg even when they contain commas.
func TestSplitTopLevelArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`a, b`, []string{"a", "b"}},
		{`"a, b", c`, []string{`"a, b"`, "c"}},
		{`x.method(), y`, []string{"x.method()", "y"}},
		{`outer(a, b), z`, []string{"outer(a, b)", "z"}},
		{`{a: 1, b: 2}, x`, []string{"{a: 1, b: 2}", "x"}},
		{``, nil},
		{`single`, []string{"single"}},
	}
	for _, tc := range cases {
		got, err := splitTopLevelArgs(tc.in)
		if err != nil {
			t.Fatalf("splitTopLevelArgs(%q): %v", tc.in, err)
		}
		if len(got) != len(tc.want) {
			t.Errorf("splitTopLevelArgs(%q) = %#v, want %#v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitTopLevelArgs(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

// TestLogicRunner_RejectsNilBody pins that the runner errors on a
// nil body rather than panicking. Production never hits this path
// (the function loader only stamps LogicSteps when there's a multi-
// step body) but the defensive check matters for direct callers.
func TestLogicRunner_RejectsNilBody(t *testing.T) {
	r := NewLogicRunner(nil, nil, nil)
	_, err := r.RunLogic(nil, "x", nil, nil)
	if err == nil {
		t.Fatalf("expected error for nil body, got nil")
	}
	if !strings.Contains(err.Error(), "logic body is nil") {
		t.Errorf("expected `logic body is nil` error, got %v", err)
	}
}

// TestLogicRunner_EvaluateLocalExpr_ScalarLiterals pins the memql#1090
// fix: a Logic body whose trailing `return <expr>` is a bare scalar
// literal must resolve to that literal's typed value through the
// logic-time local resolver, WITHOUT routing the literal string into
// engine.Execute. The staging-confirmed repro was the
// `logicSeedKnowledgeDomains` body `seed := <builtin>; return 1`: the
// "1" return string reached engine.Execute, where Parse produced a
// LiteralValueNode root that cloneExpressionNode dropped to nil, and
// the unbounded-query guard rejected the call with "query must include
// at least one filter or relationship expression". EvaluateLocalExpr is
// the convergence entry the LogicRunner return path drives (RunLogic
// calls it on returnStep.Query.Query before falling back to the step
// registry / engine.Execute), so asserting handled=true here proves the
// literal never reaches the guard.
func TestLogicRunner_EvaluateLocalExpr_ScalarLiterals(t *testing.T) {
	evaluator := NewEvaluator()
	tests := []struct {
		name string
		expr string
		want any
	}{
		{"int literal (the seedKnowledgeDomains return)", "1", int64(1)},
		{"zero", "0", int64(0)},
		{"negative int", "-7", int64(-7)},
		{"float literal", "3.14", 3.14},
		{"double-quoted string", `"hello"`, "hello"},
		{"single-quoted string", `'world'`, "world"},
		{"empty string literal", `""`, ""},
		{"bool true", "true", true},
		{"bool false", "false", false},
		{"null literal", "null", nil},
		{"whitespace padded int", "  42  ", int64(42)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, handled, err := EvaluateLocalExpr(tt.expr, evaluator)
			if err != nil {
				t.Fatalf("EvaluateLocalExpr(%q) err = %v", tt.expr, err)
			}
			if !handled {
				t.Fatalf("EvaluateLocalExpr(%q) handled=false; literal must resolve locally and never reach engine.Execute (memql#1090)", tt.expr)
			}
			if val != tt.want {
				t.Errorf("EvaluateLocalExpr(%q) = %#v, want %#v", tt.expr, val, tt.want)
			}
		})
	}
}

// TestLogicRunner_EvaluateLocalExpr_PreservesNonLiterals pins that the
// memql#1090 literal short-circuit does NOT swallow genuine queries,
// function calls, or other non-literal returns. A real concept query
// (`return queryFoo({...})`) and a top-level mutation / builtin call
// must report handled=false from the literal+step-ref+positional-builtin
// resolver so the LogicRunner falls back to engine.Execute and the query
// actually runs. Bare step refs / step-method calls / coalesce stay on
// their existing local paths (handled=true) and are covered by their own
// tests; here we focus on the "must reach the engine" set.
func TestLogicRunner_EvaluateLocalExpr_PreservesNonLiterals(t *testing.T) {
	evaluator := NewEvaluator()
	fallThrough := []struct {
		name string
		expr string
	}{
		{"genuine concept query", `queryActiveSpaces({ownerUserId: "u1"})`},
		{"top-level mutation call", `mutationCreateThing({name: "x"})`},
		{"non-positional builtin call", `ensureDailySpaceForCaller({})`},
		{"identifier that looks numeric-ish but isnt a literal", `v1abc`},
		{"comparison filter", `payload.status=="active"`},
	}
	for _, tt := range fallThrough {
		t.Run(tt.name, func(t *testing.T) {
			val, handled, err := EvaluateLocalExpr(tt.expr, evaluator)
			if err != nil {
				t.Fatalf("EvaluateLocalExpr(%q) err = %v", tt.expr, err)
			}
			if handled {
				t.Fatalf("EvaluateLocalExpr(%q) handled=true val=%#v; a real query/call must fall through to engine.Execute", tt.expr, val)
			}
		})
	}
}

// TestTryEvaluateLiteralLocally_StrictMatching pins the literal matcher's
// strictness directly: only number / quoted-string / true / false / null
// match, so no genuine query, identifier, or call is misclassified as a
// literal. Guards the memql#1090 fix against over-reach.
func TestTryEvaluateLiteralLocally_StrictMatching(t *testing.T) {
	literals := map[string]any{
		"1":       int64(1),
		"1.5":     1.5,
		`"x"`:     "x",
		"true":    true,
		"false":   false,
		"null":    nil,
	}
	for expr, want := range literals {
		val, ok := tryEvaluateLiteralLocally(expr)
		if !ok {
			t.Errorf("tryEvaluateLiteralLocally(%q) = (_, false), want literal match", expr)
			continue
		}
		if val != want {
			t.Errorf("tryEvaluateLiteralLocally(%q) = %#v, want %#v", expr, val, want)
		}
	}

	nonLiterals := []string{
		"",
		"someStep",
		"rows.Len()",
		"coalesce(a, b)",
		`queryFoo({id: "x"})`,
		"payload.active==true",
		`"unterminated`,
		"v1:cluster:node",
	}
	for _, expr := range nonLiterals {
		if _, ok := tryEvaluateLiteralLocally(expr); ok {
			t.Errorf("tryEvaluateLiteralLocally(%q) matched as literal; should not", expr)
		}
	}
}

// TestLogicRunner_RunLogic_SeedThenLiteralReturn reproduces the exact
// staging-confirmed memql#1090 shape end-to-end through RunLogic: a Logic
// body with a side-effect step followed by `return 1` --
//
//	logic logicSeedKnowledgeDomains {
//	  body {
//	    seed := knowledgeSeedStandardDomains({})
//	    return 1
//	  }
//	}
//
// Before the fix, the "1" return string was dispatched through the step
// registry into engine.Execute, where the unbounded-query guard rejected
// it ("query must include at least one filter or relationship
// expression") -- the automation failed at boot. After the fix, RunLogic
// resolves the literal locally and returns it, and ONLY the side-effect
// step reaches the registry.
func TestLogicRunner_RunLogic_SeedThenLiteralReturn(t *testing.T) {
	src := `@enabled
@description("repro of logicSeedKnowledgeDomains shape")
logic logicSeedKnowledgeDomains {
  args {
    event object @required
  }
  body {
    seed := knowledgeSeedStandardDomains({})
    return 1
  }
}
`
	body := parseLogicBody(t, src)
	registry := &recordingStepRegistry{}
	// A zero-value engine is enough: the side-effect step is served by the
	// stub registry, and the literal `return 1` is resolved locally before
	// any engine.Execute call. EventBus() on a zero-value engine is nil,
	// which RunLogic tolerates.
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "logicSeedKnowledgeDomains", body, map[string]any{
		"event": map[string]any{"payload": map[string]any{"id": "u1"}},
	})
	if err != nil {
		t.Fatalf("RunLogic returned error (memql#1090 regression): %v", err)
	}
	if out != int64(1) {
		t.Errorf("RunLogic return = %#v, want int64(1)", out)
	}
	// Only the side-effect step should have reached the registry; the
	// literal return must NOT have been dispatched into engine.Execute.
	if len(registry.dispatched) != 1 || registry.dispatched[0] != "seed" {
		t.Errorf("dispatched steps = %v, want exactly [seed] (the literal return must bypass the engine)", registry.dispatched)
	}
}
