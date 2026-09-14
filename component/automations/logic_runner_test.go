package automations

import (
	"context"
	"strings"
	"testing"
	"time"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// inProcessStep runs a step the way the real step registry runs an
// in-process expression: steps.QueryExecutor evaluates a query step that is
// not a construct call over the run (Evaluator.InProcessQuery), with no
// engine round trip. handled is false for every other step -- the ones that
// reach the engine, which a stub registry records instead.
func inProcessStep(ctx context.Context, step *Step, sc *StepContext) (result *StepResult, handled bool, err error) {
	if sc == nil || sc.Evaluator == nil {
		return nil, false, nil
	}
	v, inProcess, err := sc.Evaluator.InProcessQuery(ctx, step)
	if !inProcess {
		return nil, false, nil
	}
	now := time.Now()
	if err != nil {
		return &StepResult{StepId: step.ID, Status: "failed", Error: err.Error(), StartedAt: now, CompletedAt: now}, true, err
	}
	return &StepResult{StepId: step.ID, Status: "success", StartedAt: now, CompletedAt: now, Result: v}, true, nil
}

// recordingStepRegistry is a minimal StepExecutorRegistry stub for the
// logic-runner end-to-end tests. An in-process expression step evaluates as
// the real query executor evaluates it (inProcessStep); every other step --
// one that would reach the engine -- is recorded and answered with a canned
// success result, so a Logic body's side-effect steps run without a live
// engine / DB. `dispatched` is therefore exactly the steps that would make
// an engine round trip.
type recordingStepRegistry struct {
	dispatched []string
}

func (r *recordingStepRegistry) Execute(ctx context.Context, step *Step, sc *StepContext) (*StepResult, error) {
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
	src := `@description("test")
logic doStuff {
  args {
    partitionId  string  @required
  }
  body {
    first := queryFoo( partitionId: args.partitionId )
    second := queryBar( id: first.first().id )
    return second.first() ?? first.first()
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
	src := `@description("test")
logic provisionThing {
  args {
    name  string  @required
  }
  body {
    existing := queryThing( name: args.name )
    created := if existing.empty() {
      mutationCreateThing( name: args.name )
    }
    return created ?? existing.first()
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
		"partitionId": "space-abc",
	}
	evaluator := r.newEvaluatorForLogic(context.Background(), args)

	// `args` custom variable
	val, err := evalV1(evaluator, `args.partitionId`)
	if err != nil {
		t.Fatalf("evaluate args.partitionId: %v", err)
	}
	if val != "space-abc" {
		t.Errorf("args.partitionId = %#v, want %q", val, "space-abc")
	}

	// `event` custom variable plumbed from args
	val, err = evalV1(evaluator, `event.payload.id`)
	if err != nil {
		t.Fatalf("evaluate event.payload.id: %v", err)
	}
	if val != "user-123" {
		t.Errorf("event.payload.id = %#v, want %q", val, "user-123")
	}

	// `ctx.input` mirrors args
	val, err = evalV1(evaluator, `ctx.input.partitionId`)
	if err != nil {
		t.Fatalf("evaluate ctx.input.partitionId: %v", err)
	}
	if val != "space-abc" {
		t.Errorf("ctx.input.partitionId = %#v, want %q", val, "space-abc")
	}
}

// TestLogicRunner_EventBindingIsFirstClass pins the memql#1706 first-class
// event-context binding: the triggering event is resolvable from step
// argument expressions via BOTH the author-facing `args.event.payload.X`
// path and the bare `event.payload.X` root, with the SAME object backing
// both spellings -- this is exactly what nested-step argument resolution
// reads when threading the event into a step's args.
func TestLogicRunner_EventBindingIsFirstClass(t *testing.T) {
	r := NewLogicRunner(nil, nil, nil)
	args := map[string]any{
		"event": map[string]any{
			"topic":   "node.created",
			"payload": map[string]any{"activePartitionId": "space-xyz", "id": "user-2"},
		},
	}
	ev := r.newEvaluatorForLogic(context.Background(), args)

	for _, expr := range []string{
		`args.event.payload.activePartitionId`, // author-facing form in the live logics
		`event.payload.activePartitionId`,      // the bare event root
	} {
		val, err := evalV1(ev, expr)
		if err != nil {
			t.Fatalf("evaluate %s: %v", expr, err)
		}
		if val != "space-xyz" {
			t.Errorf("%s = %#v, want %q (event must thread into step arg scope)", expr, val, "space-xyz")
		}
	}
}

// TestLogicRunner_EventBindingSeededWhenAbsent pins the defensive fallback:
// even when the caller passes NO `event` arg, the bare `event` root is
// seeded with a well-formed empty envelope so `event.payload.X` resolves to
// empty rather than failing on an unbound root. Before #1706 the `event`
// root was seeded only when an `event` arg was present, so this resolution
// had no root to walk.
func TestLogicRunner_EventBindingSeededWhenAbsent(t *testing.T) {
	r := NewLogicRunner(nil, nil, nil)
	ev := r.newEvaluatorForLogic(context.Background(), map[string]any{"partitionId": "space-abc"})

	// The envelope itself is a well-formed object (not nil).
	envelope, err := evalV1(ev, `event`)
	if err != nil {
		t.Fatalf("evaluate event: %v", err)
	}
	if _, ok := envelope.(map[string]any); !ok {
		t.Fatalf("event = %#v (%T), want a well-formed envelope object", envelope, envelope)
	}

	// A missing payload field resolves cleanly to absent -- no unbound-root error.
	val, err := evalV1(ev, `event.payload.id`)
	if err != nil {
		t.Fatalf("evaluate event.payload.id with no event arg: %v (must degrade to absent, not error)", err)
	}
	if !memql.IsAbsent(val) {
		t.Errorf("event.payload.id = %#v, want absent for the synthetic envelope", val)
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
// revokeExpiredDelegations, the cluster/identity sweeps, etc.
// compileBodyToAutomation re-attaches a synthetic Step at the end
// of the slice; this test guards that fix.
//
// The queryFoo argument is named "asOf". It read "now" until memql#3626 --
// a reserved engine name no args block may declare, so queryFoo could never
// have received it and the value was dropped. This fixture is about the
// _return step, so the argument name only has to be a legal one.
func TestLogicRunner_PreservesReturnStep(t *testing.T) {
	src := `@description("repro")
logic logicSweep {
  args {
    asOf string @required
  }
  body {
    rows := queryFoo( asOf: args.asOf )
    for item := range rows.nodes() {
      tick := mutationBar( id: item.id )
    }
    return rows.count()
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
// This is the path that lets revokeExpiredDelegations,
// purgeExpiredArchivedSpaces, and the rest of the
// `return X.count()` family run end-to-end.
func TestLogicRunner_ReturnPureStepMethod(t *testing.T) {
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

	// .count() -> count of nodes
	val, err := evalV1(evaluator, "expiredDelegations.count()")
	if err != nil {
		t.Fatalf("expiredDelegations.count(): %v", err)
	}
	if !numericEquals(val, 3) {
		t.Errorf(".count() = %#v, want 3", val)
	}

	// .empty() -> false (3 nodes)
	val, err = evalV1(evaluator, "expiredDelegations.empty()")
	if err != nil || val != false {
		t.Errorf(".empty() = %#v (err %v); want false", val, err)
	}

	// .first() -> the first node
	val, err = evalV1(evaluator, "expiredDelegations.first()")
	if err != nil {
		t.Fatalf("expiredDelegations.first(): %v", err)
	}
	if m, ok := val.(map[string]any); !ok || m["id"] != "d1" {
		t.Errorf(".first() = %#v, want first node {id: d1}", val)
	}
}

// TestLogicRunner_ReturnBareStepVariable pins the memql#363 regression
// fix: a `return nodeRecord` after a `nodeRecord := mutation...` step must
// resolve to the step's value, not surface as `unknown spec "nodeRecord"`
// when the engine's query parser treats the bare identifier as a spec name.
// Evaluated, a bare step name over a Bundle-backed result stands for its
// node list (run_scope.go).
func TestLogicRunner_ReturnBareStepVariable(t *testing.T) {
	evaluator := NewEvaluator()
	evaluator.SetStepResult("nodeRecord", &StepResult{
		Status: "success",
		Result: map[string]any{
			"Bundle": map[string]any{
				"nodes": []any{map[string]any{"id": "v1:cluster:node:bff-local"}},
			},
		},
	})

	val, err := evalV1(evaluator, "nodeRecord")
	if err != nil {
		t.Fatalf("nodeRecord: %v", err)
	}
	nodes, ok := val.([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("bare step-variable return: got %#v, want the one-node list", val)
	}
	if n, _ := nodes[0].(map[string]any); n["id"] != "v1:cluster:node:bff-local" {
		t.Errorf("bare step-variable return: node = %#v", nodes[0])
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

// TestLogicRunner_RunLogic_SeedThenLiteralReturn reproduces the exact
// staging-confirmed memql#1090 shape end-to-end through RunLogic: a Logic
// body with a side-effect step followed by `return 1` --
//
//	logic logicSeedKnowledgeDomains {
//	  body {
//	    seed := knowledgeSeedStandardDomains()
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
	src := `
@description("repro of logicSeedKnowledgeDomains shape")
logic logicSeedKnowledgeDomains {
  args {
    event object @required
  }
  body {
    seed := knowledgeSeedStandardDomains()
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

// TestLogicRunner_RunLogic_CollectionChainStepRHS pins gap 2 (#2317)
// END-TO-END: a multi-statement logic body whose intermediate step RHS is a
// collection-method / lambda chain over a caller arg --
//
//	logic logicProbe {
//	  args { members []object @required }
//	  body {
//	    active := args.members.where(m => m.active)
//	    return active.count()
//	  }
//	}
//
// must LOAD (the parser emits a query step carrying the chain's verbatim
// source instead of rejecting the *ast.MethodCallExpr RHS) AND EVALUATE (the
// LogicRunner's collection-chain branch resolves `args.members`, runs the
// `where(m => m.active)` filter in-memory, binds the filtered collection as
// the `active` step result, and the trailing `active.count()` counts it).
// Neither the chain step nor the count return reaches the step registry --
// both resolve locally -- so a zero-value engine + stub registry is enough.
func TestLogicRunner_RunLogic_CollectionChainStepRHS(t *testing.T) {
	src := `
@description("collection-chain step RHS probe (#2317)")
logic logicProbe {
  args {
    members []object @required
  }
  body {
    active := args.members.where(m => m.active)
    return active.count()
  }
}
`
	body := parseLogicBody(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	members := []any{
		map[string]any{"name": "alice", "active": true},
		map[string]any{"name": "bob", "active": false},
		map[string]any{"name": "carol", "active": true},
	}
	out, err := r.RunLogic(context.Background(), "logicProbe", body, map[string]any{
		"members": members,
	})
	if err != nil {
		t.Fatalf("RunLogic returned error (#2317 collection-chain step RHS must load + evaluate): %v", err)
	}
	if !numericEquals(out, 2) {
		t.Errorf("RunLogic return = %#v (%T), want 2 (the active member count)", out, out)
	}
	// Neither the chain step nor the count return should have hit the
	// registry -- both are resolved in-memory by the LogicRunner.
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

// emptyQueryStepRegistry is a step-registry stub whose every engine-bound
// step returns a result with zero rows, so a downstream `existing.empty()`
// guard evaluates true (the first-boot path where the seeded records do not
// yet exist and the conditional mutation steps must fire). An in-process
// expression step evaluates as the real query executor evaluates it
// (inProcessStep). It records each engine-bound step ID so a test can
// assert exactly which steps reached the engine.
type emptyQueryStepRegistry struct {
	dispatched []string
}

func (r *emptyQueryStepRegistry) Execute(ctx context.Context, step *Step, sc *StepContext) (*StepResult, error) {
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
		// Empty Bundle -> GetStepNodes returns zero nodes -> .empty() == true.
		Result: map[string]any{"Bundle": map[string]any{"nodes": []any{}}},
	}, nil
}

// TestLogicRunner_RunLogic_WelcomeCurriculumShape reproduces the second
// staging seed automation that failed at boot with the SAME filterless-query
// guard error -- `logicSeedWelcomeCurriculum` (memql#1129). Its body has a
// distinct shape from logicSeedKnowledgeDomains: a lookup query, then several
// CONDITIONAL mutation steps gated on `existing.empty()`, then a bare literal
// `return 1` --
//
//	logic logicSeedWelcomeCurriculum {
//	  body {
//	    existing := queryCurriculumBySlug( slug: "..." )
//	    insertCurriculum := if existing.empty() { mutationCreateCurriculum(...) }
//	    insertGreeting    := if existing.empty() { mutationCreateSegment(...) }
//	    return 1
//	  }
//	}
//
// memql#1090 fixed the literal-return guard but only regression-pinned the
// simpler logicSeedKnowledgeDomains shape (`seed := <builtin>; return 1`). The
// welcome curriculum -- query + conditional mutations + literal return -- was
// never covered, yet it tripped the identical "query must include at least one
// filter or relationship expression" ERROR on staging because the trailing
// `return 1` was dispatched through engine.Execute. This pins that the whole
// shape resolves cleanly: the conditionals fire on the empty-query first-boot
// path, and the literal return is resolved locally (never reaching the
// unbounded-query guard).
func TestLogicRunner_RunLogic_WelcomeCurriculumShape(t *testing.T) {
	src := `
@description("repro of logicSeedWelcomeCurriculum shape")
logic logicSeedWelcomeCurriculum {
  args {
    event object @required
  }
  body {
    existing := queryCurriculumBySlug( slug: "exampleapp.welcome.v1" )
    insertCurriculum := if existing.empty() {
      mutationCreateCurriculum(
        curriculumId: "v1:curriculum:curriculum:exampleapp-welcome-v1",
        slug: "exampleapp.welcome.v1",
        name: "Welcome to ExampleApp",
        version: 1,
        active: true
      )
    }
    insertGreeting := if existing.empty() {
      mutationCreateSegment(
        segmentId: "v1:curriculum:segment:exampleapp-welcome-v1-greeting",
        curriculumId: "v1:curriculum:curriculum:exampleapp-welcome-v1",
        slug: "greeting",
        recommendedSteps: "[{\"name\":\"uiHighlight\",\"arguments\":{\"target\":null}}]",
        orderHint: 1
      )
    }
    return 1
  }
}
`
	body := parseLogicBody(t, src)
	registry := &emptyQueryStepRegistry{}
	// A zero-value engine is enough: the lookup + conditional mutation steps
	// are served by the stub registry, and the literal `return 1` is resolved
	// locally before any engine.Execute call.
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "logicSeedWelcomeCurriculum", body, map[string]any{
		"event": map[string]any{"payload": map[string]any{"id": "u1"}},
	})
	if err != nil {
		t.Fatalf("RunLogic returned error (memql#1129 regression -- filterless-query guard must not trip on the welcome-curriculum shape): %v", err)
	}
	if out != int64(1) {
		t.Errorf("RunLogic return = %#v, want int64(1)", out)
	}
	// The lookup + BOTH conditional mutations must have reached the registry
	// (the empty-query first-boot path fires the guards); the literal `return
	// 1` must NOT have been dispatched -- it is resolved locally.
	want := []string{"existing", "insertCurriculum", "insertGreeting"}
	if len(registry.dispatched) != len(want) {
		t.Fatalf("dispatched steps = %v, want %v (literal return must bypass the engine; conditionals must fire on empty query)", registry.dispatched, want)
	}
	got := map[string]bool{}
	for _, id := range registry.dispatched {
		got[id] = true
		if id == "_return" {
			t.Errorf("the literal `return 1` step reached the engine (memql#1090/#1129 regression); it must resolve locally")
		}
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("expected step %q to be dispatched; got %v", id, registry.dispatched)
		}
	}
}

// TestLogicRunner_RunLogic_TerminalReturnArithmetic pins #2542 item 1
// END-TO-END: a multi-step logic whose terminal return is binary arithmetic
// over prior step results --
//
//	logic aov {
//	  body {
//	    r := args.revenue ?? 0
//	    o := args.orders ?? 1
//	    return r / o
//	  }
//	}
//
// must LOAD AND EVALUATE (both step operands resolve in process under the
// #2316 numeric rules). Nothing reaches the engine: the `??` steps and the
// return all evaluate in process.
func TestLogicRunner_RunLogic_TerminalReturnArithmetic(t *testing.T) {
	src := `
@description("aov-style terminal-return arithmetic (#2542)")
logic aov {
  args {
    revenue int @required
    orders int @required
  }
  body {
    r := args.revenue ?? 0
    o := args.orders ?? 1
    return r / o
  }
}
`
	body := parseLogicBody(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "aov", body, map[string]any{
		"revenue": 100,
		"orders":  4,
	})
	if err != nil {
		t.Fatalf("RunLogic returned error (#2542 terminal-return arithmetic must evaluate): %v", err)
	}
	if !numericEquals(out, 25) {
		t.Errorf("RunLogic return = %#v (%T), want 25 (100 / 4)", out, out)
	}
	if len(registry.dispatched) != 0 {
		t.Errorf("dispatched steps = %v, want none (coalesce + arithmetic resolve locally)", registry.dispatched)
	}
}

// Division by zero must surface as a clean logic error naming the failure,
// never a panic and never a silent nil return.
func TestLogicRunner_RunLogic_TerminalReturnArithmetic_DivisionByZero(t *testing.T) {
	src := `
@description("division by zero surfaces cleanly")
logic ratio {
  args {
    a int @required
    b int @required
  }
  body {
    x := args.a ?? 0
    y := args.b ?? 0
    return x / y
  }
}
`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	_, err := r.RunLogic(context.Background(), "ratio", body, map[string]any{"a": 10, "b": 0})
	if err == nil {
		t.Fatalf("RunLogic succeeded on x / 0; want a division-by-zero error")
	}
	if !strings.Contains(err.Error(), "division_by_zero") {
		t.Errorf("error = %q, want the division_by_zero refusal", err.Error())
	}
}

// Modulo requires whole-number operands: a fractional operand must surface
// the operand_type refusal, not compute a bogus value.
func TestLogicRunner_RunLogic_TerminalReturnArithmetic_FloatModulo(t *testing.T) {
	src := `
@description("float modulo surfaces cleanly")
logic remainder {
  args {
    a float @required
    b int @required
  }
  body {
    x := args.a ?? 0
    y := args.b ?? 1
    return x % y
  }
}
`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	_, err := r.RunLogic(context.Background(), "remainder", body, map[string]any{"a": 10.5, "b": 3})
	if err == nil {
		t.Fatalf("RunLogic succeeded on float %% int; want an integer-operands error")
	}
	if !strings.Contains(err.Error(), "needs whole numbers") {
		t.Errorf("error = %q, want the operand_type whole-numbers refusal", err.Error())
	}
}

// TestLogicRunner_RunLogic_TerminalReturnComparison pins #2542 item 5
// END-TO-END for the MULTI-STEP path: a logic whose terminal return is an
// expression-led comparison over a computed intermediate --
//
//	logic isProfitable {
//	  body {
//	    delta := args.revenue - args.cost
//	    return delta - 5 > 0
//	  }
//	}
//
// must EVALUATE to the boolean (the LogicRunner's comparison branch resolves
// both operands locally and applies the ordering operator) rather than falling
// through to the step registry -- where the engine rejects a
// BinaryComparisonExpression with the `only available in logic` scope error.
// Nothing reaches the registry: the arithmetic step and the comparison return
// both resolve locally. The engine single-return path already handles the
// single-return boolean shape (engine.go plan-root branch); this is its
// multi-step counterpart.
func TestLogicRunner_RunLogic_TerminalReturnComparison(t *testing.T) {
	src := `
@description("expression-led comparison terminal return (#2542 item 5)")
logic isProfitable {
  args {
    revenue int @required
    cost int @required
  }
  body {
    delta := args.revenue - args.cost
    return delta - 5 > 0
  }
}
`
	body := parseLogicBody(t, src)

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
			out, err := r.RunLogic(context.Background(), "isProfitable", body, map[string]any{
				"revenue": tc.revenue,
				"cost":    tc.cost,
			})
			if err != nil {
				t.Fatalf("RunLogic (#2542 comparison terminal return must evaluate): %v", err)
			}
			if out != tc.want {
				t.Errorf("RunLogic return = %#v (%T), want %v", out, out, tc.want)
			}
			if len(registry.dispatched) != 0 {
				t.Errorf("dispatched steps = %v, want none (arithmetic step + comparison return resolve locally)", registry.dispatched)
			}
		})
	}
}

// TestLogicRunner_RunLogic_TerminalReturnComparison_LiteralLed pins the
// literal-led shape (`0 < delta`) -- the other half of the expression-led
// comparison grammar (parser's non-identifier-led left side). It must evaluate
// through the same LogicRunner comparison branch, never dispatch to the
// registry.
func TestLogicRunner_RunLogic_TerminalReturnComparison_LiteralLed(t *testing.T) {
	src := `
@description("literal-led comparison terminal return (#2542 item 5)")
logic hasSurplus {
  args {
    revenue int @required
    cost int @required
  }
  body {
    delta := args.revenue - args.cost
    return 0 < delta
  }
}
`
	body := parseLogicBody(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "hasSurplus", body, map[string]any{"revenue": 10, "cost": 4})
	if err != nil {
		t.Fatalf("RunLogic (literal-led comparison must evaluate): %v", err)
	}
	if out != true {
		t.Errorf("RunLogic return = %#v (%T), want true (0 < 6)", out, out)
	}
	if len(registry.dispatched) != 0 {
		t.Errorf("dispatched steps = %v, want none", registry.dispatched)
	}
}

// TestLogicRunner_RunLogic_DateBuiltinStepValueAndReturn pins #2541
// END-TO-END: date builtins bound as step VALUES (`delta := daysBetween(...)`,
// `y := year(...)`) and read back through the terminal return. This is the
// day-delta half of the issue's minimal date surface (streak logic: "is today
// exactly one day after the last").
func TestLogicRunner_RunLogic_DateBuiltinStepValueAndReturn(t *testing.T) {
	src := `
@description("day-delta between two datetimes (#2541)")
logic streakDelta {
  args {
    prev string @required
    curr string @required
  }
  body {
    delta := daysBetween(args.prev, args.curr)
    return delta
  }
}
`
	body := parseLogicBody(t, src)
	registry := &recordingStepRegistry{}
	r := NewLogicRunner(&memql.MemQLEngine{}, registry, nil)

	out, err := r.RunLogic(context.Background(), "streakDelta", body, map[string]any{
		"prev": "2026-07-13T22:00:00Z",
		"curr": "2026-07-14T22:00:00Z",
	})
	if err != nil {
		t.Fatalf("RunLogic returned error (#2541 date-builtin step value must evaluate): %v", err)
	}
	if !numericEquals(out, 1) {
		t.Errorf("RunLogic return = %#v (%T), want 1 (one day apart)", out, out)
	}
	if len(registry.dispatched) != 0 {
		t.Errorf("dispatched steps = %v, want none (the date builtin resolves locally)", registry.dispatched)
	}
}

// A date builtin in the TERMINAL RETURN position, over a prior step result.
func TestLogicRunner_RunLogic_DateBuiltinTerminalReturn(t *testing.T) {
	src := `
@description("addDuration in terminal return (#2541)")
logic boundary {
  args {
    start string @required
  }
  body {
    seed := args.start ?? ""
    return addDuration(seed, "P1D")
  }
}
`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	out, err := r.RunLogic(context.Background(), "boundary", body, map[string]any{
		"start": "2026-03-10",
	})
	if err != nil {
		t.Fatalf("RunLogic returned error (#2541 date builtin in terminal return must evaluate): %v", err)
	}
	if out != "2026-03-11T00:00:00Z" {
		t.Errorf("RunLogic return = %#v, want 2026-03-11T00:00:00Z", out)
	}
}

// A date builtin nested INSIDE terminal-return arithmetic:
// `return daysBetween(a, b) / 7` (weeks between two dates). Exercises the
// arithmetic operand walker's date-builtin dispatch.
func TestLogicRunner_RunLogic_DateBuiltinInsideArithmeticReturn(t *testing.T) {
	src := `
@description("daysBetween inside terminal-return arithmetic (#2541/#2542)")
logic weeksBetween {
  args {
    a string @required
    b string @required
  }
  body {
    seed := args.a ?? ""
    return daysBetween(args.a, args.b) / 7
  }
}
`
	body := parseLogicBody(t, src)
	r := NewLogicRunner(&memql.MemQLEngine{}, &recordingStepRegistry{}, nil)

	out, err := r.RunLogic(context.Background(), "weeksBetween", body, map[string]any{
		"a": "2026-07-01",
		"b": "2026-07-15",
	})
	if err != nil {
		t.Fatalf("RunLogic returned error: %v", err)
	}
	if !numericEquals(out, 2) {
		t.Errorf("RunLogic return = %#v (%T), want 2 (14 days / 7)", out, out)
	}
}

// TestLogicRunner_CompileRoundTrip_ArithmeticAndDateBuiltins pins the
// serializer half of #2541/#2542: the compiled `_return` string carries
// canonical v1 source -- re-parseable, never the `<<unsupported expression
// %T>>` marker, and never rewritten into another dialect (no `$args`).
func TestLogicRunner_CompileRoundTrip_ArithmeticAndDateBuiltins(t *testing.T) {
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
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			src := `
@description("round-trip probe")
logic probe {
  args {
    a string @required
    b string @required
  }
  body {
    r := args.a ?? ""
    o := args.b ?? ""
    ` + tc.ret + `
  }
}
`
			body := parseLogicBody(t, src)
			r := NewLogicRunner(nil, nil, nil)
			auto, err := r.compileBodyToAutomation("probe", body)
			if err != nil {
				t.Fatalf("compileBodyToAutomation: %v", err)
			}
			var ret *Step
			for _, s := range auto.Steps {
				if s != nil && s.ID == "_return" {
					ret = s
				}
			}
			if ret == nil || ret.Query == nil {
				t.Fatalf("no _return step in compiled automation")
			}
			got := strings.TrimSpace(ret.Query.Query)
			if strings.Contains(got, "<<unsupported") {
				t.Fatalf("_return = %q still carries the unsupported-expression marker", got)
			}
			if got != tc.wantReturn {
				t.Errorf("_return = %q, want %q", got, tc.wantReturn)
			}
		})
	}
}
