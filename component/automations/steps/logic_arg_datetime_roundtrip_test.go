package steps

// logic_arg_datetime_roundtrip_test.go -- the memql#2543 end-to-end
// regression: an automation step passes a query/step-result collection into
// a logic arg (`logic dayRollup(rows: steps.getRows.result)`); the
// FunctionExecutor resolves the reference to live Go values, stringifies
// them into query text, and engine.Execute re-parses that text before
// dispatching the logic. A row's createdAt (a live time.Time) rendered in
// Go's default format and the re-parse rejected it with `expected '}', got
// "-07"` -- a runtime crash memqllint could not see.
//
// The test drives every production stage of that boundary in order:
//
//  1. SERIALIZE -- resolveArgsRefs + renderFunctionArgs, exactly the two
//     calls FunctionExecutor.Execute makes to build the query text.
//  2. RE-PARSE -- MemQLEngine.Parse on the real engine (the same
//     parseWithFunctions path engine.Execute runs), with the receiving
//     logic registered so the call classifies as plan.LogicCall.
//  3. RUN -- LogicRunner.RunLogic with the re-parsed args, with the
//     receiving logic filtering ON the datetime field, proving the
//     round-tripped value is evaluable data, not just parseable text.
//
// Only engine.Execute's DB dispatch is stubbed (the embedded test engine
// has no database); parse and logic evaluation are the production code.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// parseLogicBodyForSteps parses a logic source and returns the multi-step
// body the function loader would store as fn.LogicSteps -- the same shape
// LogicRunner.RunLogic receives in production. SetSource matters: the
// collection-chain step RHS (#2317) is captured as a verbatim source slice.
func parseLogicBodyForSteps(t *testing.T, src string) *langparser.AutomationDef {
	t.Helper()
	normalised, err := langparser.NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	lexer := langparser.NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	p := langparser.NewParser(tokens)
	p.SetSource(normalised)
	ast, err := p.Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file, ok := ast.(*langparser.File)
	if !ok {
		t.Fatalf("expected *File, got %T", ast)
	}
	for _, def := range file.Definitions {
		fd, ok := def.(*langparser.FunctionDef)
		if !ok || fd.Type != langparser.FunctionTypeLogic {
			continue
		}
		body, ok := fd.Body.(*langparser.AutomationDef)
		if !ok {
			t.Fatalf("expected *AutomationDef body, got %T", fd.Body)
		}
		return body
	}
	t.Fatalf("no logic function found in source")
	return nil
}

// nullStepRegistry satisfies the LogicRunner's registry dependency for
// bodies whose steps all resolve locally (collection chains, literal
// returns). Any step that reaches it succeeds with an empty result.
type nullStepRegistry struct{}

func (r *nullStepRegistry) Execute(_ context.Context, step *automations.Step, _ *automations.StepContext) (*automations.StepResult, error) {
	now := time.Now()
	return &automations.StepResult{StepId: step.ID, Status: "success", StartedAt: now, CompletedAt: now}, nil
}

func TestLogicArgDatetimeRoundTrip_FullPath(t *testing.T) {
	eng := bootEmbeddedEngine(t)

	// The receiving logic: filters the arg-passed working set ON the
	// datetime field. Before the fix this logic was unreachable -- the
	// call text crashed at re-parse; a receiving side that then USES the
	// field proves the round-trip delivers data.
	logicSrc := `@enabled
@description("memql#2543 regression probe")
logic logicDayRollupProbe {
  args {
    rows []object @required
    day string @required
  }
  body {
    matching := args.rows.where(r => r.createdAt == "2026-07-14T08:30:00Z")
    return matching.count()
  }
}
`
	body := parseLogicBodyForSteps(t, logicSrc)
	if err := eng.Functions().Upsert(&memql.Function{
		Name:         "logicDayRollupProbe",
		FunctionKind: "logic",
		Enabled:      true,
		LogicSteps:   body,
	}); err != nil {
		t.Fatalf("register probe logic: %v", err)
	}

	// STAGE 1 -- serialize. Seed the sending automation's evaluator with a
	// step result whose rows carry a live time.Time createdAt (the DB-model
	// representation, memory-nodes/models.go), then resolve + render the
	// compiled `logic dayRollupProbe(rows: steps.getRows.result, day: ...)`
	// args exactly as FunctionExecutor.Execute does.
	cet := time.FixedZone("CET", 3600)
	evaluator := automations.NewEvaluator()
	evaluator.SetStepResult("getRows", &automations.StepResult{
		StepId: "getRows",
		Status: "success",
		Result: []map[string]any{
			{"id": "v1:ship:scan:1", "createdAt": time.Date(2026, 7, 14, 9, 30, 0, 0, cet), "count": 3},
			{"id": "v1:ship:scan:2", "createdAt": time.Date(2026, 7, 13, 9, 30, 0, 0, cet), "count": 30},
		},
	})
	compiledArgs := map[string]any{
		"rows": "steps.getRows.result",
		"day":  "2026-07-14",
	}
	resolved, err := resolveArgsRefs(compiledArgs, evaluator)
	if err != nil {
		t.Fatalf("resolveArgsRefs: %v", err)
	}
	query := "logicDayRollupProbe(" + renderFunctionArgs(resolved) + ")"
	for _, leak := range []string{" CET", " MST", "+0100", "seconds:"} {
		if strings.Contains(query, leak) {
			t.Fatalf("rendered query leaked Go-format datetime %q (the #2543 crash text):\n%s", leak, query)
		}
	}

	// STAGE 2 -- re-parse through the real engine parse path. Before the
	// fix this failed with `expected '}', got "-07"`.
	plan, err := eng.Parse(query)
	if err != nil {
		t.Fatalf("engine re-parse of the rendered logic call failed (memql#2543):\n%v\nquery: %s", err, query)
	}
	if plan.LogicCall == nil {
		t.Fatalf("expected the call to classify as plan.LogicCall; plan: %+v", plan)
	}
	rows, ok := plan.LogicCall.Args["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("rows arg did not survive the round trip: %#v", plan.LogicCall.Args["rows"])
	}
	row0, _ := rows[0].(map[string]any)
	if row0 == nil || row0["createdAt"] != "2026-07-14T08:30:00Z" {
		t.Fatalf("createdAt did not round-trip as the RFC3339 UTC string: %#v", rows[0])
	}

	// STAGE 3 -- run the logic on the re-parsed args (the dispatch
	// executeLogicFunctionCall performs). The where() filter reads the
	// datetime field, so a wrong representation cannot sneak through as an
	// ignored blob.
	runner := automations.NewLogicRunner(eng, &nullStepRegistry{}, nil)
	out, err := runner.RunLogic(context.Background(), "logicDayRollupProbe", body, plan.LogicCall.Args)
	if err != nil {
		t.Fatalf("RunLogic on round-tripped args: %v", err)
	}
	if !intEquals(out, 1) {
		t.Errorf("logic filtered on the round-tripped createdAt: got %#v (%T), want 1 matching row", out, out)
	}
}

// intEquals reports whether v is the integer n across the numeric types the
// collection-count path can surface.
func intEquals(v any, n int) bool {
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
