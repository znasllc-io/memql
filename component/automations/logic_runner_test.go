package automations

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

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
