package parser

// v1_positions_test.go -- the positions that parse the edition-2026
// expression grammar (task memql#5364, Task 3 of the DSL v1 expressions plan),
// in both modes of the transition: Options.ExpressionsV1 off (the legacy
// grammar, with the pushdown positions accepting their v1 spellings beside the
// legacy ones) and on (every position v1, the legacy predicate forms refused).
//
// memqlmigrate:keep-file -- the legacy spellings in this file are its cases:
// the off half loads them and the on half refuses them, so the fixture
// codemod must leave them as written.

import (
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

var (
	v1Off = Options{}
	v1On  = Options{ExpressionsV1: true}
)

// parseV1Authored runs authored source through the path the engine uses --
// the struct-form rewriter, then the parser -- under o.
func parseV1Authored(t *testing.T, src string, o Options) (*File, error) {
	t.Helper()
	normalised, err := NormaliseAll(src)
	if err != nil {
		return nil, err
	}
	// Lexed with the author's positions carried in it, as every parse site
	// that lowers does (position_markers.go): a refusal names src's line and
	// column.
	return ParseFileWithOptions(PositionLowering(src, normalised), o)
}

func mustParseV1Authored(t *testing.T, src string, o Options) *File {
	t.Helper()
	f, err := parseV1Authored(t, src, o)
	if err != nil {
		t.Fatalf("parse (ExpressionsV1=%v):\n%s\nerror: %v", o.ExpressionsV1, src, err)
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

// TestExpressionsV1MarksDefinitions: every definition parsed with the option
// on says so, on the FunctionDef and on an automation or logic body, because
// that flag is what the compiler and the automation runtime key their v1 path
// on; with the option off nothing is marked.
func TestExpressionsV1MarksDefinitions(t *testing.T) {
	sources := map[string]string{
		"logic": `logic probe {
  args {
    x string @required
  }
  body {
    return args.x
  }
}`,
		"automation": `@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  step first {
    logic other(x: 1)
  }
}`,
	}
	for name, src := range sources {
		t.Run(name, func(t *testing.T) {
			for _, o := range []Options{v1Off, v1On} {
				fn := onlyFunction(t, mustParseV1Authored(t, src, o))
				if fn.ExpressionsV1 != o.ExpressionsV1 {
					t.Errorf("ExpressionsV1=%v: FunctionDef.ExpressionsV1 = %v", o.ExpressionsV1, fn.ExpressionsV1)
				}
				auto, ok := fn.Body.(*ast.AutomationDef)
				if !ok {
					t.Fatalf("want an AutomationDef body, got %T", fn.Body)
				}
				if auto.ExpressionsV1 != o.ExpressionsV1 {
					t.Errorf("ExpressionsV1=%v: AutomationDef.ExpressionsV1 = %v", o.ExpressionsV1, auto.ExpressionsV1)
				}
			}
		})
	}
	// The tree is migrated (memql#5368), so ParseFile -- every loader, Sense,
	// memqllint -- parses edition 2026.
	if !DefaultOptions.ExpressionsV1 {
		t.Error("DefaultOptions.ExpressionsV1 is off, but the tree is migrated: ParseFile must parse edition 2026")
	}
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

// automationBody returns the file's one automation or logic body.
func automationBody(t *testing.T, f *File) *ast.AutomationDef {
	t.Helper()
	fn := onlyFunction(t, f)
	auto, ok := fn.Body.(*ast.AutomationDef)
	if !ok {
		t.Fatalf("want an AutomationDef body, got %T", fn.Body)
	}
	return auto
}

// stepByID finds a step by id, searching forEach bodies and switch cases.
func stepByID(t *testing.T, steps []ast.StepDef, id string) *ast.StepDef {
	t.Helper()
	var find func([]ast.StepDef) *ast.StepDef
	find = func(ss []ast.StepDef) *ast.StepDef {
		for i := range ss {
			if ss[i].ID == id {
				return &ss[i]
			}
			switch cfg := ss[i].Config.(type) {
			case *ast.ForEachStepConfig:
				if s := find(cfg.Do); s != nil {
					return s
				}
			case *ast.SwitchStepConfig:
				for _, c := range cfg.Cases {
					if s := find(c.Steps); s != nil {
						return s
					}
				}
				if cfg.Default != nil {
					if s := find(cfg.Default.Steps); s != nil {
						return s
					}
				}
			}
		}
		return nil
	}
	if s := find(steps); s != nil {
		return s
	}
	var ids []string
	for _, s := range steps {
		ids = append(ids, s.ID)
	}
	t.Fatalf("no step %q among %v", id, ids)
	return nil
}

// ---------------------------------------------------------------------------
// Pushdown positions: both spellings accepted with the option off; only the
// v1 spelling with it on.
// ---------------------------------------------------------------------------

const v1FilterQuery = `query thing probe {
  args {
    a string @required
  }
  filter row => row.a == args.a && isX(row)
  shape probeCard
}`

// TestV1FilterQueryParsesInBothModes: a filter that opens with a lambda header
// is a v1 filter, joined to the concept with `&&`, the lambda kept whole.
func TestV1FilterQueryParsesInBothModes(t *testing.T) {
	for _, o := range []Options{v1Off, v1On} {
		fn := onlyFunction(t, mustParseV1Authored(t, v1FilterQuery, o))
		body, ok := fn.Body.(ast.ExpressionNode)
		if !ok {
			t.Fatalf("query body is %T", fn.Body)
		}
		and, ok := queryBase(body).(*ast.LogicalExpr)
		if !ok || and.Op != ast.LogicalAnd {
			t.Fatalf("ExpressionsV1=%v: base is %T %+v, want LogicalExpr{AND, concept==..., lambda}", o.ExpressionsV1, queryBase(body), queryBase(body))
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
}

// TestLegacyFilterParsesOffAndRefusesOn: the legacy filter is joined the same
// way, parenthesised so its own `||` stays inside the concept's scope; with the
// option on it is the retired form.
func TestLegacyFilterParsesOffAndRefusesOn(t *testing.T) {
	src := `query thing probe {
  args {
    a string
    b string
  }
  filter a == args.a || b == args.b
}`
	fn := onlyFunction(t, mustParseV1Authored(t, src, v1Off))
	and, ok := queryBase(fn.Body.(ast.ExpressionNode)).(*ast.LogicalExpr)
	if !ok || and.Op != ast.LogicalAnd {
		t.Fatalf("base is %T, want LogicalExpr{AND}", queryBase(fn.Body.(ast.ExpressionNode)))
	}
	if or, ok := and.Right.(*ast.LogicalExpr); !ok || or.Op != ast.LogicalOr {
		t.Fatalf("the legacy filter's || escaped the concept scope: right of the join is %T %+v", and.Right, and.Right)
	}

	_, err := parseV1Authored(t, src, v1On)
	wantRetired(t, err, "retired_filter_without_lambda")

	// A query with no filter has nothing to refuse.
	noFilter := "query thing probe {\n  shape probeCard\n}"
	mustParseV1Authored(t, noFilter, v1On)
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
			for _, o := range []Options{v1Off, v1On} {
				fn := onlyFunction(t, mustParseV1Authored(t, src, o))
				and := queryBase(fn.Body.(ast.ExpressionNode)).(*ast.LogicalExpr)
				if _, ok := and.Right.(*ast.LambdaExpr); !ok {
					t.Fatalf("ExpressionsV1=%v: right of the join is %T", o.ExpressionsV1, and.Right)
				}
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
	for _, o := range []Options{v1Off, v1On} {
		fn := onlyFunction(t, mustParseV1Authored(t, src, o))
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
	}

	for name, c := range map[string]struct{ src, want string }{
		"without paginate": {"query thing probe {\n  filter row => row.a == 1\n  refine row => row.b == 2\n}", "`refine` requires `paginate`"},
		"with count":       {"query thing probe {\n  filter row => row.a == 1\n  refine row => row.b == 2\n  count\n}", "`refine` cannot be combined with `count`"},
		"not a lambda":     {"query thing probe {\n  filter row => row.a == 1\n  paginate 5\n  refine b == 2\n}", "parse error at line 4, column 10: refine takes a lambda of one parameter, as in row => <predicate>; got `b == 2`"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseV1Authored(t, c.src, v1Off)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}

// TestSpecAndTraitLambdaForm: `spec <bound> <name> = <lambda>` and
// `trait <name> = <lambda>` carry the lambda in SpecDecl.Lambda; the legacy
// `{ return ... }` body keeps parsing with the option off and is refused with
// it on.
func TestSpecAndTraitLambdaForm(t *testing.T) {
	v1Forms := map[string]string{
		"spec over a concept": `spec registration isRevoked = row => row.revoked == true`,
		"spec over an actor":  `spec actorEnvelope requiresAdmin = actor => actor.role == "admin"`,
		"trait":               `trait isActiveRecord = row => row.active == true`,
		"multi-line":          "spec registration isRevoked = row =>\n  row.revoked == true\n  && row.a != nil",
	}
	for name, src := range v1Forms {
		t.Run(name, func(t *testing.T) {
			for _, o := range []Options{v1Off, v1On} {
				f := mustParseV1Authored(t, src, o)
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
				if decl.Body != nil {
					t.Errorf("Body = %T, want nil for the lambda form", decl.Body)
				}
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
			f := mustParseV1Authored(t, c.src, v1Off)
			if decl := f.Definitions[0].(*ast.SpecDecl); decl.Body == nil || decl.Lambda != nil {
				t.Fatalf("off: Body=%v Lambda=%v", decl.Body, decl.Lambda)
			}
			_, err := parseV1Authored(t, c.src, v1On)
			wantRetired(t, err, c.rule)
		})
	}

	for _, bad := range []string{
		`spec registration isX = (a, b) => a == b`,
		`spec registration isX = row.a == 1`,
		`trait isX = (row => row.a)`,
		`spec registration isX =`,
	} {
		if _, err := parseV1Authored(t, bad, v1Off); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := parseV1Authored(t, `spec registration isX = (a, b) => a == b`, v1Off); err == nil || !strings.Contains(err.Error(), "one parameter") {
		t.Errorf("a two-parameter spec lambda should be refused naming the one parameter, got %v", err)
	}
}

// TestFilterAnnotationLambda: `@filter(<lambda>)` keeps its canonical text in
// TriggerDef.Filter and the lambda in TriggerDef.FilterLambda; the legacy
// raw-text @filter keeps working with the option off and is refused with it on.
func TestFilterAnnotationLambda(t *testing.T) {
	automation := func(filter string) string {
		return filter + "\n@trigger(event=\"node.created\", concept=\"v1:probe:thing\")\nautomation probe {\n  step first {\n    logic other(x: 1)\n  }\n}"
	}
	for _, o := range []Options{v1Off, v1On} {
		auto := automationBody(t, mustParseV1Authored(t, automation(`@filter(row => row.status == "x" && row.a != nil)`), o))
		if auto.Trigger == nil || auto.Trigger.FilterLambda == nil {
			t.Fatalf("ExpressionsV1=%v: no FilterLambda on %+v", o.ExpressionsV1, auto.Trigger)
		}
		if want := `row => row.status == "x" && row.a != nil`; auto.Trigger.Filter != want {
			t.Errorf("Filter = %q, want %q", auto.Trigger.Filter, want)
		}
	}

	// The legacy raw-text form, unchanged with the option off.
	legacy := automation(`@filter(payload.status == "x")`)
	auto := automationBody(t, mustParseV1Authored(t, legacy, v1Off))
	if auto.Trigger == nil || auto.Trigger.Filter != "payload.status==x" || auto.Trigger.FilterLambda != nil {
		t.Fatalf("the legacy @filter changed: %+v", auto.Trigger)
	}
	_, err := parseV1Authored(t, legacy, v1On)
	wantRetired(t, err, "retired_filter_annotation")
	_, err = parseV1Authored(t, automation(`@filter("payload.status == 1")`), v1On)
	wantRetired(t, err, "retired_filter_annotation")

	// The filter= argument of @trigger takes the same lambda.
	viaTrigger := "@trigger(event=\"node.created\", concept=\"v1:probe:thing\", filter=row => row.a == 1)\nautomation probe {\n  step first {\n    logic other(x: 1)\n  }\n}"
	auto = automationBody(t, mustParseV1Authored(t, viaTrigger, v1Off))
	if auto.Trigger == nil || auto.Trigger.FilterLambda == nil || auto.Trigger.Filter != "row => row.a == 1" {
		t.Fatalf("@trigger(filter=<lambda>): %+v", auto.Trigger)
	}
	legacyTrigger := "@trigger(event=\"node.created\", concept=\"v1:probe:thing\", filter=\"payload.a == 1\")\nautomation probe {\n  step first {\n    logic other(x: 1)\n  }\n}"
	_, err = parseV1Authored(t, legacyTrigger, v1On)
	wantRetired(t, err, "retired_filter_annotation")

	// A filter lambda has exactly one parameter.
	if _, err := parseV1Authored(t, automation(`@filter((a, b) => a == b)`), v1Off); err == nil {
		t.Error("a two-parameter @filter lambda was accepted")
	}
}

// TestTerseAutomationWithFilterLambda: the terse header's arrow is the LAST
// top-level `=>` after the annotations, so a lambda inside @filter -- on the
// header line or on its own line above it -- does not confuse the lowering.
// The second case is dsl/data/automations.memql migrated.
func TestTerseAutomationWithFilterLambda(t *testing.T) {
	cases := map[string]string{
		"inline": `automation conflictDetection @trigger(event="node.created", concept="v1:data:record", partition="*") @filter(row => row.naturalKeyValue != nil) => logic conflictDetection`,
		"dsl/data/automations.memql migrated": `/// Detects conflicts between new data records and existing confirmed records.
@filter(row => row.naturalKeyValue != nil)
automation conflictDetection @trigger(event="node.created", concept="v1:data:record", partition="*") => logic conflictDetection`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if !LooksLikeTerseAutomation(src) {
				t.Fatal("not recognised as a terse automation")
			}
			slices := ExtractTerseAutomationSlices(src)
			if len(slices) != 1 || slices[0].Name != "conflictDetection" {
				t.Fatalf("ExtractTerseAutomationSlices = %+v", slices)
			}
			for _, o := range []Options{v1Off, v1On} {
				auto := automationBody(t, mustParseV1Authored(t, src, o))
				if auto.Trigger == nil || auto.Trigger.FilterLambda == nil {
					t.Fatalf("ExpressionsV1=%v: trigger %+v", o.ExpressionsV1, auto.Trigger)
				}
				if auto.Trigger.Filter != "row => row.naturalKeyValue != nil" {
					t.Errorf("Filter = %q", auto.Trigger.Filter)
				}
				run := stepByID(t, auto.Steps, "run")
				if cfg, ok := run.Config.(*ast.FunctionStepConfig); !ok || cfg.Name != "conflictDetection" {
					t.Errorf("the lowered step is %+v", run.Config)
				}
			}
		})
	}
	// The pre-existing terse shape is unchanged.
	plain := `automation registerNode @trigger(event="system.startup") => logic registerNode`
	if !LooksLikeTerseAutomation(plain) {
		t.Fatal("the plain terse form is no longer recognised")
	}
	lowered, err := NormaliseTerseAutomationSource(plain)
	if err != nil {
		t.Fatal(err)
	}
	want := "@trigger(event=\"system.startup\")\nautomation registerNode {\n  step run {\n    logic registerNode { event: event }\n  }\n}"
	if lowered != want {
		t.Errorf("plain terse lowering changed:\n got %q\nwant %q", lowered, want)
	}
}

// ---------------------------------------------------------------------------
// In-process positions: v1 only with the option on.
// ---------------------------------------------------------------------------

const v1LogicSource = `logic probe {
  args {
    items []object
    mode string
  }
  body {
    total := query countThings(mode: args.mode)
    if args.mode == "a" && total != nil {
      marked := markThing(id: args.mode, n: total)
    } else {
      other := markOther(id: args.mode)
    }
    for item := range args.items if item.active == true {
      touched := touchThing(id: item.id)
    }
    switch args.mode {
    case "a":
      sa := stepA(x: 1)
    default:
      sd := stepD(x: 2)
    }
    return total + 1
  }
}`

// TestInProcessPositionsV1: each in-process position parses the v1 grammar
// with the option on -- the string field holds canonical v1 source and the
// *Expr field (or the value map) holds the node -- and is untouched with it
// off.
func TestInProcessPositionsV1(t *testing.T) {
	on := automationBody(t, mustParseV1Authored(t, v1LogicSource, v1On))

	// A construct call on a step RHS: named v1 nodes in the Args map.
	total := stepByID(t, on.Steps, "total")
	fcfg, ok := total.Config.(*ast.FunctionStepConfig)
	if !ok || fcfg.Name != "countThings" {
		t.Fatalf("total: %+v", total.Config)
	}
	if n, ok := fcfg.Args["mode"].(ast.ExpressionNode); !ok || ast.FormatExpr(n) != "args.mode" {
		t.Errorf("total's mode arg is %#v", fcfg.Args["mode"])
	}

	// if / else: the condition stamps, then negates, as v1 nodes.
	marked := stepByID(t, on.Steps, "marked")
	if want := `args.mode == "a" && total != nil`; marked.Condition != want || marked.ConditionExpr == nil || ast.FormatExpr(marked.ConditionExpr) != want {
		t.Errorf("marked condition %q / %v, want %q", marked.Condition, marked.ConditionExpr, want)
	}
	if n, ok := marked.Config.(*ast.FunctionStepConfig).Args["n"].(ast.ExpressionNode); !ok || ast.FormatExpr(n) != "total" {
		t.Errorf("marked's n arg is %#v", marked.Config.(*ast.FunctionStepConfig).Args["n"])
	}
	other := stepByID(t, on.Steps, "other")
	if want := `!(args.mode == "a" && total != nil)`; other.Condition != want || other.ConditionExpr == nil {
		t.Errorf("else-branch condition %q, want %q", other.Condition, want)
	}

	// for: source and filter.
	var loop *ast.ForEachStepConfig
	for _, s := range on.Steps {
		if cfg, ok := s.Config.(*ast.ForEachStepConfig); ok {
			loop = cfg
		}
	}
	if loop == nil {
		t.Fatal("no forEach step")
	}
	if loop.Source != "args.items" || loop.SourceExpr == nil || loop.Filter != "item.active == true" || loop.FilterExpr == nil {
		t.Errorf("forEach %+v", loop)
	}

	// switch: the subject.
	var sw *ast.SwitchStepConfig
	for _, s := range on.Steps {
		if cfg, ok := s.Config.(*ast.SwitchStepConfig); ok {
			sw = cfg
		}
	}
	if sw == nil || sw.Expression != "args.mode" || sw.ExpressionExpr == nil {
		t.Fatalf("switch %+v", sw)
	}

	// return: a v1 node.
	ret := stepByID(t, on.Steps, "_return")
	q, ok := ret.Config.(*ast.QueryStepConfig)
	if !ok || ast.KindOf(q.Query) != ast.KindArithmetic || ast.FormatExpr(q.Query) != "total + 1" {
		t.Errorf("return %+v", ret.Config)
	}

	// Off: today's shapes, no v1 fields.
	off := automationBody(t, mustParseV1Authored(t, v1LogicSource, v1Off))
	if m := stepByID(t, off.Steps, "marked"); m.ConditionExpr != nil || m.Condition != `args.mode == "a" && total != nil` {
		t.Errorf("off: marked %q / %v", m.Condition, m.ConditionExpr)
	}
	if o := stepByID(t, off.Steps, "other"); o.Condition != `not (args.mode == "a" && total != nil)` {
		t.Errorf("off: the legacy negation changed: %q", o.Condition)
	}
	for _, s := range off.Steps {
		if cfg, ok := s.Config.(*ast.ForEachStepConfig); ok && (cfg.SourceExpr != nil || cfg.FilterExpr != nil) {
			t.Errorf("off: forEach carries v1 nodes")
		}
		if cfg, ok := s.Config.(*ast.SwitchStepConfig); ok && cfg.ExpressionExpr != nil {
			t.Errorf("off: switch carries a v1 node")
		}
	}
	if _, isV1 := stepByID(t, off.Steps, "_return").Config.(*ast.QueryStepConfig).Query.(*ast.BinaryExpr); isV1 {
		t.Error("off: the return parsed as v1")
	}
}

// TestAutomationStepsV1: the struct-form automation's steps -- a construct
// call with a pun, a gated call, a forEach with a where filter, a switch -- in
// on-mode.
func TestAutomationStepsV1(t *testing.T) {
	src := `@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  step first {
    logic runIt(status, mode: "x")
  }
  step second {
    if steps.first.result == true && event.payload.y != nil {
      builtin doIt(id: event.payload.id)
    }
  }
  step loop {
    forEach t in first.nodes() where t.active == true {
      touch { id: t.id }
    }
  }
  step pick {
    switch event.payload.kind {
      case "a" {
        logic handleA(x: 1)
      }
      default {
        logic handleD(x: 2)
      }
    }
  }
}`
	auto := automationBody(t, mustParseV1Authored(t, src, v1On))
	first := stepByID(t, auto.Steps, "first").Config.(*ast.FunctionStepConfig)
	if first.Name != "runIt" || ast.FormatExpr(first.Args["status"].(ast.ExpressionNode)) != "status" || ast.FormatExpr(first.Args["mode"].(ast.ExpressionNode)) != `"x"` {
		t.Errorf("first %+v", first)
	}
	second := stepByID(t, auto.Steps, "second")
	if second.Condition != "steps.first.result == true && event.payload.y != nil" || second.ConditionExpr == nil {
		t.Errorf("second condition %q", second.Condition)
	}
	if cfg := second.Config.(*ast.FunctionStepConfig); cfg.Name != "doIt" {
		t.Errorf("second %+v", cfg)
	}
	var loop *ast.ForEachStepConfig
	for _, s := range auto.Steps {
		if cfg, ok := s.Config.(*ast.ForEachStepConfig); ok {
			loop = cfg
		}
	}
	if loop == nil || loop.Source != "first.nodes()" || loop.Filter != "item.active == true" || loop.SourceExpr == nil || loop.FilterExpr == nil {
		t.Fatalf("forEach %+v", loop)
	}
	touch := stepByID(t, auto.Steps, "loop_do1").Config.(*ast.FunctionStepConfig)
	if ast.FormatExpr(touch.Args["id"].(ast.ExpressionNode)) != "item.id" {
		t.Errorf("touch %+v", touch)
	}
	var sw *ast.SwitchStepConfig
	for _, s := range auto.Steps {
		if cfg, ok := s.Config.(*ast.SwitchStepConfig); ok {
			sw = cfg
		}
	}
	if sw == nil || sw.Expression != "event.payload.kind" || sw.ExpressionExpr == nil {
		t.Fatalf("switch %+v", sw)
	}

	// Off: the same automation parses exactly as before.
	off := automationBody(t, mustParseV1Authored(t, src, v1Off))
	if s := stepByID(t, off.Steps, "second"); s.ConditionExpr != nil {
		t.Error("off: a v1 condition node")
	}
	if _, isNode := stepByID(t, off.Steps, "first").Config.(*ast.FunctionStepConfig).Args["mode"].(ast.ExpressionNode); isNode {
		t.Error("off: step args became v1 nodes")
	}
}

// TestMutationValuesV1: insert/update values parse v1 with the option on.
func TestMutationValuesV1(t *testing.T) {
	src := `mutate thing probe {
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
	fn := onlyFunction(t, mustParseV1Authored(t, src, v1On))
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

	off := onlyFunction(t, mustParseV1Authored(t, src, v1Off)).Body.(*ast.MutationStmt)
	if off.PayloadExpr != nil {
		t.Error("off: a v1 payload node")
	}
	if _, isV1 := off.IDTemplate.(*ast.MemberExpr); isV1 {
		t.Error("off: the id parsed as v1")
	}
}

// TestRetiredSpellingsInProcessPositions: `cond(` and `concat(` parse in an
// in-process position with the option off and are refused with it on.
func TestRetiredSpellingsInProcessPositions(t *testing.T) {
	cases := map[string]struct{ src, rule string }{
		"cond in a return":             {"logic probe {\n  args {\n    a bool\n  }\n  body {\n    return cond(args.a, 1, 2)\n  }\n}", "retired_cond_call"},
		"concat on a step":             {"logic probe {\n  args {\n    a string\n  }\n  body {\n    x := concat(args.a, \"b\")\n    return x\n  }\n}", "retired_concat_call"},
		"exists in an if":              {"logic probe {\n  args {\n    a string\n  }\n  body {\n    if exists(args.a) {\n      x := f(a: args.a)\n    }\n    return args.a\n  }\n}", "retired_exists_call"},
		"coalesce in a mutation value": {"mutate thing probe {\n  args {\n    id string @required\n    a string\n  }\n  insert {\n    id: args.id\n    a: coalesce(args.a, \"x\")\n  }\n}", "retired_coalesce_call"},
		"cond in a step argument":      {"@trigger(event=\"node.created\", concept=\"v1:probe:thing\")\nautomation probe {\n  step first {\n    logic other(x: cond(true, 1, 2))\n  }\n}", "retired_cond_call"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			mustParseV1Authored(t, c.src, v1Off)
			_, err := parseV1Authored(t, c.src, v1On)
			wantRetired(t, err, c.rule)
		})
	}
}

// TestDefaultOptionsReachesEveryEntry: the engine's function and automation
// loaders build their parsers with NewParser, the spec loader calls
// ParseSpecDecl, and other callers use ParseFile, so flipping DefaultOptions
// must reach all of them -- that is what makes the flip one line. An
// expression-only parse (the internal query form an SDK sends to Execute) must
// read no option at all.
func TestDefaultOptionsReachesEveryEntry(t *testing.T) {
	saved := DefaultOptions
	t.Cleanup(func() { DefaultOptions = saved })
	DefaultOptions = Options{ExpressionsV1: true}

	legacySpec := "spec thing isX {\n  return a == 1\n}"
	_, err := ParseSpecDecl(legacySpec)
	wantRetired(t, err, "retired_spec_return_body")
	_, err = ParseFile(legacySpec)
	wantRetired(t, err, "retired_spec_return_body")

	logic, err := NormaliseAll("logic probe {\n  args {\n    a bool\n  }\n  body {\n    return cond(args.a, 1, 2)\n  }\n}")
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
		t.Fatalf("the internal query form must keep the legacy grammar under any option: %v", err)
	}
}
