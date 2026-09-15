package parser

// v1_positions_test.go -- the positions that parse the edition-2026
// expression grammar (task memql#5364, Task 3 of the DSL v1 expressions plan):
// every authoring position parses v1, and the legacy predicate forms are
// refused naming their replacement.
//
// memqlmigrate:keep-file -- the legacy spellings in this file are its cases:
// each is asserted refused, so the fixture codemod must leave them as written.

import (
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// parseV1Authored runs authored source through the path the engine uses --
// the struct-form rewriter, then the parser.
func parseV1Authored(t *testing.T, src string) (*File, error) {
	t.Helper()
	normalised, err := NormaliseAll(src)
	if err != nil {
		return nil, err
	}
	// Lexed with the author's positions carried in it, as every parse site
	// that lowers does (position_markers.go): a refusal names src's line and
	// column.
	return ParseFile(PositionLowering(src, normalised))
}

func mustParseV1Authored(t *testing.T, src string) *File {
	t.Helper()
	f, err := parseV1Authored(t, src)
	if err != nil {
		t.Fatalf("parse:\n%s\nerror: %v", src, err)
	}
	return f
}

// onlyFunction returns the file's one FunctionDef.
func onlyFunction(t *testing.T, f *File) *FunctionDef {
	t.Helper()
	var fns []*FunctionDef
	for _, d := range f.Definitions {
		if fn, ok := d.(*FunctionDef); ok {
			fns = append(fns, fn)
		}
	}
	if len(fns) != 1 {
		t.Fatalf("want one function definition, got %d (%d definitions)", len(fns), len(f.Definitions))
	}
	return fns[0]
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// wantRetired asserts err is the edition-2026 refusal of rule.
func wantRetired(t *testing.T, err error, rule string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted a form retired under %s", rule)
	}
	var rf *RetiredFormError
	if !errors.As(err, &rf) {
		t.Fatalf("want the %s refusal, got %T: %v", rule, err, err)
	}
	if rf.Form.Rule != rule {
		t.Fatalf("refused under %s, want %s: %v", rf.Form.Rule, rule, err)
	}
	if !strings.Contains(err.Error(), "memqlmigrate --rewrite=expressions") {
		t.Fatalf("the refusal does not name the migrator: %v", err)
	}
}

// wantRetiredAt is wantRetired, with the refusal placed at the nth needle of
// src: the token the author wrote, in the author's line and column.
func wantRetiredAt(t *testing.T, err error, rule, src, needle string, nth int) {
	t.Helper()
	wantRetired(t, err, rule)
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("the %s refusal carries no position: %v", rule, err)
	}
	wantLine, wantCol := authoredAt(t, src, needle, nth)
	if line, col := pe.Position(); line != wantLine || col != wantCol {
		t.Fatalf("the %s refusal is at %d:%d, want %d:%d (the %q the author wrote): %v", rule, line, col, wantLine, wantCol, needle, err)
	}
}

// queryBase unwraps a query body's directive wrappers (shape, count,
// paginate, sort, asOf, withDepth, select, refine) to its base expression.
func queryBase(n ast.ExpressionNode) ast.ExpressionNode {
	for {
		switch e := n.(type) {
		case *ast.ShapeExpr:
			n = e.Target
		case *ast.CountExpr:
			n = e.Target
		case *ast.PaginateExpr:
			n = e.Target
		case *ast.SortExpr:
			n = e.Target
		case *ast.TimestampExpr:
			n = e.Target
		case *ast.DepthExpr:
			n = e.Target
		case *ast.SelectExpr:
			n = e.Target
		case *ast.RefineExpr:
			n = e.Target
		default:
			return n
		}
	}
}

// automationBody returns the file's one automation.
func automationBody(t *testing.T, f *File) *ast.AutomationDef {
	t.Helper()
	fn := onlyFunction(t, f)
	auto, ok := fn.Body.(*ast.AutomationDef)
	if !ok {
		t.Fatalf("want an AutomationDef body, got %T", fn.Body)
	}
	return auto
}

// ---------------------------------------------------------------------------
// Pushdown positions: the v1 spelling, and the legacy one refused.
// ---------------------------------------------------------------------------

const v1FilterQuery = `query thing probe {
  args {
    a string @required
  }
  filter row => row.a == args.a && isX(row)
  shape probeCard
}`

// TestV1FilterQuery: a filter that opens with a lambda header is a v1 filter,
// joined to the concept with `&&`, the lambda kept whole.
func TestV1FilterQuery(t *testing.T) {
	fn := onlyFunction(t, mustParseV1Authored(t, v1FilterQuery))
	body, ok := fn.Body.(ast.ExpressionNode)
	if !ok {
		t.Fatalf("query body is %T", fn.Body)
	}
	and, ok := queryBase(body).(*ast.LogicalExpr)
	if !ok || and.Op != ast.LogicalAnd {
		t.Fatalf("base is %T %+v, want LogicalExpr{AND, concept==..., lambda}", queryBase(body), queryBase(body))
	}
	cmp, ok := and.Left.(*ast.ComparisonExpr)
	if !ok || cmp.Field.Raw != "concept" {
		t.Fatalf("left of the join is %T %+v, want the concept comparison", and.Left, and.Left)
	}
	lam, ok := and.Right.(*ast.LambdaExpr)
	if !ok {
		t.Fatalf("right of the join is %T, want *ast.LambdaExpr", and.Right)
	}
	if got := ast.FormatExpr(lam); got != `row => row.a == args.a && isX(row)` {
		t.Errorf("the lambda prints as %s", got)
	}
	if shape, ok := body.(*ast.ShapeExpr); !ok || shape.TemplateName != "probeCard" {
		t.Errorf("the shape wrapper is %T %+v", body, body)
	}
}

// TestLegacyFilterRefused: a filter with no lambda header is the retired form.
func TestLegacyFilterRefused(t *testing.T) {
	src := `query thing probe {
  args {
    a string
    b string
  }
  filter a == args.a || b == args.b
}`
	_, err := parseV1Authored(t, src)
	wantRetired(t, err, "retired_filter_without_lambda")

	// A query with no filter has nothing to refuse.
	noFilter := "query thing probe {\n  shape probeCard\n}"
	mustParseV1Authored(t, noFilter)
}

// TestV1FilterContinuationLines: a v1 filter may break across lines at a
// binary operator, the conditional's `?` and `:`, or the lambda's `=>`.
func TestV1FilterContinuationLines(t *testing.T) {
	cases := map[string]string{
		"leading &&":    "filter row => row.a == 1\n    && row.b == 2",
		"leading ||":    "filter row => row.a == 1\n    || row.b == 2",
		"trailing ?":    "filter row => row.kind == \"x\" ?\n    row.a == 1 : row.b == 2",
		"leading ? :":   "filter row => row.kind == \"x\"\n    ? row.a == 1\n    : row.b == 2",
		"trailing =>":   "filter row =>\n    row.a == 1",
		"leading =>":    "filter row\n    => row.a == 1",
		"leading ??":    "filter row => row.a\n    ?? \"x\" == \"y\"",
		"method chains": "filter row => row.tags\n    .any(t => t == \"x\")",
	}
	for name, clause := range cases {
		t.Run(name, func(t *testing.T) {
			src := "query thing probe {\n  " + clause + "\n  shape probeCard\n}"
			fn := onlyFunction(t, mustParseV1Authored(t, src))
			and := queryBase(fn.Body.(ast.ExpressionNode)).(*ast.LogicalExpr)
			if _, ok := and.Right.(*ast.LambdaExpr); !ok {
				t.Fatalf("right of the join is %T", and.Right)
			}
		})
	}
}

// TestRefineClause: `refine <lambda>` needs `paginate`, never takes `count`,
// and wraps the page -- inside shape, outside paginate.
func TestRefineClause(t *testing.T) {
	src := `query thing probe {
  filter row => row.a == 1
  sort "row.createdAt", "desc"
  paginate 25
  refine row => row.title.includes("x")
  shape probeCard
}`
	fn := onlyFunction(t, mustParseV1Authored(t, src))
	shape, ok := fn.Body.(*ast.ShapeExpr)
	if !ok {
		t.Fatalf("body is %T, want the shape wrapper outermost", fn.Body)
	}
	refine, ok := shape.Target.(*ast.RefineExpr)
	if !ok {
		t.Fatalf("inside shape is %T, want *ast.RefineExpr", shape.Target)
	}
	if _, ok := refine.Target.(*ast.PaginateExpr); !ok {
		t.Fatalf("refine wraps %T, want the paginate wrapper", refine.Target)
	}
	if refine.Lambda == nil || ast.FormatExpr(refine.Lambda) != `row => row.title.includes("x")` {
		t.Fatalf("refine lambda is %v", refine.Lambda)
	}

	for name, c := range map[string]struct{ src, want string }{
		"without paginate": {"query thing probe {\n  filter row => row.a == 1\n  refine row => row.b == 2\n}", "`refine` requires `paginate`"},
		"with count":       {"query thing probe {\n  filter row => row.a == 1\n  refine row => row.b == 2\n  count\n}", "`refine` cannot be combined with `count`"},
		"not a lambda":     {"query thing probe {\n  filter row => row.a == 1\n  paginate 5\n  refine b == 2\n}", "parse error at line 4, column 10: refine takes a lambda of one parameter, as in row => <predicate>; got `b == 2`"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseV1Authored(t, c.src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

// TestSpecAndTraitLambdaForm: `spec <bound> <name> = <lambda>` and
// `trait <name> = <lambda>` carry the lambda in SpecDecl.Lambda; the legacy
// `{ return ... }` body is refused.
func TestSpecAndTraitLambdaForm(t *testing.T) {
	v1Forms := map[string]string{
		"spec over a concept": `spec registration isRevoked = row => row.revoked == true`,
		"spec over an actor":  `spec actorEnvelope requiresAdmin = actor => actor.role == "admin"`,
		"trait":               `trait isActiveRecord = row => row.active == true`,
		"multi-line":          "spec registration isRevoked = row =>\n  row.revoked == true\n  && row.a != nil",
	}
	for name, src := range v1Forms {
		t.Run(name, func(t *testing.T) {
			f := mustParseV1Authored(t, src)
			if len(f.Definitions) != 1 {
				t.Fatalf("got %d definitions", len(f.Definitions))
			}
			decl, ok := f.Definitions[0].(*ast.SpecDecl)
			if !ok {
				t.Fatalf("definition is %T", f.Definitions[0])
			}
			if decl.Lambda == nil || len(decl.Lambda.Params) != 1 {
				t.Fatalf("Lambda = %+v, want a one-parameter lambda", decl.Lambda)
			}
			// The per-slice entry the spec loader uses agrees.
			decl, err := ParseSpecDecl(src)
			if err != nil {
				t.Fatalf("ParseSpecDecl: %v", err)
			}
			if decl.Lambda == nil {
				t.Fatal("ParseSpecDecl dropped the lambda")
			}
		})
	}

	legacy := map[string]struct{ src, rule string }{
		"spec":  {"spec registration isRevoked {\n  return revoked == true\n}", "retired_spec_return_body"},
		"trait": {"trait isActiveRecord {\n  return active == true\n}", "retired_trait_return_body"},
	}
	for name, c := range legacy {
		t.Run("legacy "+name, func(t *testing.T) {
			_, err := parseV1Authored(t, c.src)
			wantRetired(t, err, c.rule)
		})
	}

	for _, bad := range []string{
		`spec registration isX = (a, b) => a == b`,
		`spec registration isX = row.a == 1`,
		`trait isX = (row => row.a)`,
		`spec registration isX =`,
	} {
		if _, err := parseV1Authored(t, bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := parseV1Authored(t, `spec registration isX = (a, b) => a == b`); err == nil || !strings.Contains(err.Error(), "one parameter") {
		t.Errorf("a two-parameter spec lambda should be refused naming the one parameter, got %v", err)
	}
}

// TestFilterAnnotationLambda: `@filter(<lambda>)` keeps its canonical text in
// TriggerDef.Filter and the lambda in TriggerDef.FilterLambda; the legacy
// raw-text @filter is refused.
func TestFilterAnnotationLambda(t *testing.T) {
	automation := func(filter string) string {
		return filter + "\n@trigger(event=\"node.created\", concept=\"v1:probe:thing\")\nautomation probe {\n  logic other(x: 1)\n}"
	}
	auto := automationBody(t, mustParseV1Authored(t, automation(`@filter(row => row.status == "x" && row.a != nil)`)))
	if auto.Trigger == nil || auto.Trigger.FilterLambda == nil {
		t.Fatalf("no FilterLambda on %+v", auto.Trigger)
	}
	if want := `row => row.status == "x" && row.a != nil`; auto.Trigger.Filter != want {
		t.Errorf("Filter = %q, want %q", auto.Trigger.Filter, want)
	}

	// The legacy raw-text form, bare or quoted.
	_, err := parseV1Authored(t, automation(`@filter(payload.status == "x")`))
	wantRetired(t, err, "retired_filter_annotation")
	_, err = parseV1Authored(t, automation(`@filter("payload.status == 1")`))
	wantRetired(t, err, "retired_filter_annotation")

	// The filter= argument of @trigger takes the same lambda.
	viaTrigger := "@trigger(event=\"node.created\", concept=\"v1:probe:thing\", filter=row => row.a == 1)\nautomation probe {\n  logic other(x: 1)\n}"
	auto = automationBody(t, mustParseV1Authored(t, viaTrigger))
	if auto.Trigger == nil || auto.Trigger.FilterLambda == nil || auto.Trigger.Filter != "row => row.a == 1" {
		t.Fatalf("@trigger(filter=<lambda>): %+v", auto.Trigger)
	}
	legacyTrigger := "@trigger(event=\"node.created\", concept=\"v1:probe:thing\", filter=\"payload.a == 1\")\nautomation probe {\n  logic other(x: 1)\n}"
	_, err = parseV1Authored(t, legacyTrigger)
	wantRetired(t, err, "retired_filter_annotation")

	// A filter lambda has exactly one parameter.
	if _, err := parseV1Authored(t, automation(`@filter((a, b) => a == b)`)); err == nil {
		t.Error("a two-parameter @filter lambda was accepted")
	}
}

// ---------------------------------------------------------------------------
// In-process positions. A logic's and an automation's statements are the
// statement parser's (v1_body_test.go); a mutation value is here.
// ---------------------------------------------------------------------------

// TestMutationValuesV1: insert/update values parse v1.
func TestMutationValuesV1(t *testing.T) {
	src := `mutation thing probe {
  args {
    id string @required
    name string
  }
  insert {
    id: args.id
    name: args.name ?? "unnamed"
    tags: ["a", "b"]
    meta: {source: "import", at: now}
  }
}`
	fn := onlyFunction(t, mustParseV1Authored(t, src))
	m, ok := fn.Body.(*ast.MutationStmt)
	if !ok {
		t.Fatalf("body %T", fn.Body)
	}
	if id, ok := m.IDTemplate.(ast.ExpressionNode); !ok || ast.FormatExpr(id) != "args.id" {
		t.Errorf("IDTemplate %#v", m.IDTemplate)
	}
	payload, ok := m.PayloadExpr.(*ast.MapExpr)
	if !ok {
		t.Fatalf("PayloadExpr %#v", m.PayloadExpr)
	}
	want := `{name: args.name ?? "unnamed", tags: ["a", "b"], meta: {source: "import", at: now}}`
	if got := ast.FormatExpr(payload); got != want || m.PayloadRaw != want {
		t.Errorf("payload %s / raw %s, want %s", got, m.PayloadRaw, want)
	}
}

// TestRetiredSpellingsInProcessPositions: `cond(`, `concat(`, `exists(` and
// `coalesce(` are refused in every in-process position: a statement's
// expression, a call argument and a mutation value.
func TestRetiredSpellingsInProcessPositions(t *testing.T) {
	cases := map[string]struct{ src, rule string }{
		"cond in a return":             {"logic probe {\n  args {\n    a bool\n  }\n  return cond(args.a, 1, 2)\n}", "retired_cond_call"},
		"concat in an assignment":      {"logic probe {\n  args {\n    a string\n  }\n  x := concat(args.a, \"b\")\n  return x\n}", "retired_concat_call"},
		"exists in an if":              {"logic probe {\n  args {\n    a string\n  }\n  if exists(args.a) {\n    x := logic f(a: args.a)\n  }\n  return args.a\n}", "retired_exists_call"},
		"coalesce in a mutation value": {"mutation thing probe {\n  args {\n    id string @required\n    a string\n  }\n  insert {\n    id: args.id\n    a: coalesce(args.a, \"x\")\n  }\n}", "retired_coalesce_call"},
		"cond in a call argument":      {"@trigger(event=\"node.created\", concept=\"v1:probe:thing\")\nautomation probe {\n  logic other(x: cond(true, 1, 2))\n}", "retired_cond_call"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseV1Authored(t, c.src)
			wantRetired(t, err, c.rule)
		})
	}
}

// TestEveryEntryParsesEdition2026: the engine's function and automation
// loaders build their parsers with NewParser, the spec loader calls
// ParseSpecDecl, and other callers use ParseFile; each refuses the retired
// forms. An expression-only parse (the internal query form an SDK sends to
// Execute) keeps its own grammar.
func TestEveryEntryParsesEdition2026(t *testing.T) {
	legacySpec := "spec thing isX {\n  return a == 1\n}"
	_, err := ParseSpecDecl(legacySpec)
	wantRetired(t, err, "retired_spec_return_body")
	_, err = ParseFile(legacySpec)
	wantRetired(t, err, "retired_spec_return_body")

	logic, err := NormaliseAll("logic probe {\n  args {\n    a bool\n  }\n  return cond(args.a, 1, 2)\n}")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := NewLexer(logic).Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewParser(tokens).Parse()
	wantRetired(t, err, "retired_cond_call")

	if _, err := ParseExpression(`concept==v1:a:b; status==active`); err != nil {
		t.Fatalf("the internal query form must keep its grammar: %v", err)
	}
}
