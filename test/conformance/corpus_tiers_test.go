package conformance

// corpus_tiers_test.go -- the corpus's completeness gate over the tier
// manifest (memql#5369; D23 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md:
// "two completeness gates, one over registry cells and one over tier-table
// entries").
//
// The manifest (component/language/tiers) says, for every expression
// position, which node kinds and catalog functions it admits. A manifest entry
// no case shows is a rule nothing holds the engine to, so this gate reads the
// corpus and asks it to show every one:
//
//   - every (position, node kind) the manifest admits -- Admitted or
//     PlanConstantOnly -- is used by a POSITIVE case at that position
//     (load_ok, lower or evaluate);
//   - every position holds a REFUSED case (refuse_parse or refuse_load);
//   - every catalog function and method is called by a positive case
//     somewhere.
//
// Coverage is computed from the cells themselves, never declared beside them:
// the gate PARSES each positive case at its position with the edition-2026
// parser and walks what it parsed. A lower or evaluate case holds the
// expression alone; a load_ok case is a whole file, and the gate reads the
// expression at the case's position out of it -- a filter's lambda, a spec's
// lambda, a trigger filter, a refine clause, and the literal of the three
// literal positions. A load_ok case at an in-process position is not read for
// coverage: a load there shows that the form parses and loads, not what it
// answers, so the in-process positions are credited by their evaluate cases,
// which run through the edition-2026 evaluator itself.
//
// It also refuses a positive case that uses what its position refuses, so the
// corpus cannot show a form as legal that the manifest says is not.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/functions"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/language/tiers"
)

// TestCorpusTierCompleteness fails naming each (position, kind) cell the tier
// manifest admits and no positive case uses, each position with no refused
// case, each catalog function no positive case calls, and each positive case
// that uses what its position refuses.
func TestCorpusTierCompleteness(t *testing.T) {
	cov := corpusTierCoverage(discoverCorpus(t))
	if cov.examined == 0 {
		t.Fatal("the gate examined no positive expression case -- it would pass by matching nothing")
	}

	var missingCells []string
	cells, covered := 0, 0
	fnKeys := map[string]bool{}
	for _, rule := range tiers.Rules() {
		if rule.Function != "" {
			fnKeys[rule.Function] = true
			continue
		}
		if rule.Admission == tiers.Refused {
			continue
		}
		cells++
		if cov.kinds[corpusTierCell{rule.Position, rule.Kind}] > 0 {
			covered++
			continue
		}
		missingCells = append(missingCells, fmt.Sprintf("(%s, %s) -- %s there, and no positive case at %s uses it",
			rule.Position, rule.Kind, rule.Admission, rule.Position))
	}

	var missingFns []string
	for key := range fnKeys {
		if cov.functions[key] == 0 {
			missingFns = append(missingFns, key)
		}
	}
	sort.Strings(missingFns)

	var noRefusal []string
	for _, p := range tiers.Positions() {
		if cov.refused[p] == 0 {
			noRefusal = append(noRefusal, string(p))
		}
	}

	t.Logf("tier completeness: %d of %d (position, kind) cells covered, by %d positive cases; %d of %d catalog functions called; %d of %d positions hold a refused case",
		covered, cells, cov.examined, len(fnKeys)-len(missingFns), len(fnKeys), len(tiers.Positions())-len(noRefusal), len(tiers.Positions()))
	for _, m := range missingCells {
		t.Errorf("uncovered tier cell %s", m)
	}
	for _, k := range missingFns {
		t.Errorf("catalog function %s is called by no positive case", k)
	}
	for _, p := range noRefusal {
		t.Errorf("position %s holds no refused case (refuse_parse or refuse_load): the corpus shows nothing it refuses", p)
	}
	for _, p := range cov.problems {
		t.Error(p)
	}
}

// corpusTierCell is one (position, node kind) entry of the manifest.
type corpusTierCell struct {
	pos  tiers.Position
	kind ast.NodeKind
}

// corpusCoverage is what the positive and refused cases show.
type corpusCoverage struct {
	kinds     map[corpusTierCell]int
	functions map[string]int
	refused   map[tiers.Position]int
	examined  int
	problems  []string
}

// corpusTierCoverage reads every case at an expression position.
func corpusTierCoverage(runs []*corpusRun) *corpusCoverage {
	cov := &corpusCoverage{
		kinds:     map[corpusTierCell]int{},
		functions: map[string]int{},
		refused:   map[tiers.Position]int{},
	}
	fixtureFields := map[string]map[string]string{}
	for _, r := range runs {
		pos := tiers.Position(r.c.Position)
		if pos == "" {
			continue
		}
		if _, seen := fixtureFields[r.dir]; !seen {
			fixtureFields[r.dir] = corpusDeclaredFields(r.line.Edition, r.fixture)
		}
		typing := corpusTyping{fields: fixtureFields[r.dir], args: r.c.Args}
		switch r.c.Verdict {
		case verdictRefuseParse, verdictRefuseLoad:
			cov.refused[pos]++
		case verdictLower, verdictEvaluate:
			n, err := corpusParseAtPosition(pos, strings.TrimSpace(r.src))
			if err != nil {
				cov.problems = append(cov.problems, fmt.Sprintf("%s: a positive case the edition-2026 parser refuses at %s: %v", r.name(), pos, err))
				continue
			}
			cov.examined++
			cov.record(pos, n, typing, r.name())
		case verdictLoadOK:
			file, err := corpusParseFile(r.line.Edition, r.src)
			if err != nil {
				continue // the verdict runner fails the case, naming the parse error
			}
			nodes, readable := corpusExpressionsAt(pos, file)
			if !readable {
				continue
			}
			if len(nodes) == 0 {
				cov.problems = append(cov.problems, fmt.Sprintf("%s: a load_ok case at %s holds no edition-2026 expression at that position -- write the position's v1 form", r.name(), pos))
				continue
			}
			if own := corpusDeclaredFields(r.line.Edition, r.src); len(own) > 0 {
				merged := map[string]string{}
				for k, v := range typing.fields {
					merged[k] = v
				}
				for k, v := range own {
					merged[k] = v
				}
				typing.fields = merged
			}
			cov.examined++
			for _, n := range nodes {
				cov.record(pos, n, typing, r.name())
			}
		}
	}
	return cov
}

// record walks one positive expression at pos.
func (cov *corpusCoverage) record(pos tiers.Position, root ast.ExpressionNode, typing corpusTyping, ref string) {
	if lam, ok := root.(*ast.LambdaExpr); ok && lam != nil && len(lam.Params) == 1 {
		typing.param = lam.Params[0]
	}
	ast.WalkV1(root, func(n ast.ExpressionNode) bool {
		k := ast.KindOf(n)
		if k == ast.KindUnknown {
			cov.problems = append(cov.problems, fmt.Sprintf("%s: %T is not an edition-2026 expression node", ref, n))
			return false
		}
		cov.kinds[corpusTierCell{pos, k}]++
		if tiers.KindAdmission(pos, k) == tiers.Refused {
			cov.problems = append(cov.problems, fmt.Sprintf("%s: a positive case at %s uses a %s node (`%s`), which %s refuses",
				ref, pos, k, ast.FormatExpr(n), pos))
		}
		if call, ok := n.(*ast.CallExpr); ok && call.Kind == "" {
			cov.recordCall(pos, call, typing, ref)
		}
		return true
	})
}

// recordCall credits the catalog entry a call reaches. A method's entry
// depends on its receiver's type, which the gate reads from what it can see
// statically (a literal, a declared field, a case arg, a catalog result); a
// method named by more than one receiver on a receiver it cannot type is
// credited to neither, rather than to a guess. A bare call no catalog function
// answers is a predicate application.
func (cov *corpusCoverage) recordCall(pos tiers.Position, call *ast.CallExpr, typing corpusTyping, ref string) {
	var key string
	switch {
	case call.Receiver != nil:
		key = corpusMethodKey(call, typing)
		if key == "" {
			return
		}
	default:
		fn, ok := functions.Lookup(call.Name)
		if !ok {
			if tiers.PredicateAdmission(pos) == tiers.Refused {
				cov.problems = append(cov.problems, fmt.Sprintf("%s: a positive case at %s applies %s(), and %s applies no spec or trait",
					ref, pos, call.Name, pos))
			}
			return
		}
		key = fn.Key()
	}
	cov.functions[key]++
	if tiers.FunctionAdmission(pos, key) == tiers.Refused {
		cov.problems = append(cov.problems, fmt.Sprintf("%s: a positive case at %s calls %s, which %s refuses", ref, pos, key, pos))
	}
}

// corpusTyping is what the gate knows statically about the names a case reads.
type corpusTyping struct {
	param  string            // the position lambda's parameter: the row
	fields map[string]string // the fixture's declared fields: name to type word
	args   map[string]any    // the case's args
}

// corpusMethodKey returns the catalog key a method call reaches, or "".
func corpusMethodKey(call *ast.CallExpr, typing corpusTyping) string {
	var owners []string
	for _, recv := range []string{functions.TypeString, functions.TypeList} {
		if fn, ok := functions.Method(recv, call.Name); ok {
			owners = append(owners, fn.Key())
		}
	}
	switch len(owners) {
	case 0:
		return ""
	case 1:
		return owners[0]
	}
	recv := corpusTypeOf(call.Receiver, typing)
	if recv == "" {
		return ""
	}
	if fn, ok := functions.Method(recv, call.Name); ok {
		return fn.Key()
	}
	return ""
}

// corpusTypeOf is the static type word of an expression, or "" when the gate
// cannot tell.
func corpusTypeOf(n ast.ExpressionNode, typing corpusTyping) string {
	switch e := ast.Unparen(n).(type) {
	case *ast.LiteralExpr:
		if _, ok := e.Value.(string); ok {
			return functions.TypeString
		}
	case *ast.ListExpr:
		return functions.TypeList
	case *ast.BinaryExpr:
		if e.Op == "+" {
			l, r := corpusTypeOf(e.Left, typing), corpusTypeOf(e.Right, typing)
			switch {
			case l == functions.TypeString || r == functions.TypeString:
				return functions.TypeString
			case l == functions.TypeList && r == functions.TypeList:
				return functions.TypeList
			}
		}
	case *ast.CallExpr:
		if e.Kind != "" {
			return ""
		}
		var fn functions.Function
		var ok bool
		if e.Receiver != nil {
			if key := corpusMethodKey(e, typing); key != "" {
				fn, ok = corpusCatalogEntry(key)
			}
		} else {
			fn, ok = functions.Lookup(e.Name)
		}
		if ok && (fn.Returns == functions.TypeString || fn.Returns == functions.TypeList) {
			return fn.Returns
		}
	case *ast.MemberExpr:
		if id, ok := e.Object.(*ast.IdentExpr); ok && id != nil {
			switch {
			case typing.param != "" && id.Name == typing.param:
				return typing.fields[e.Field]
			case id.Name == "args":
				return corpusJSONType(typing.args[e.Field])
			}
		}
	}
	return ""
}

func corpusCatalogEntry(key string) (functions.Function, bool) {
	for _, f := range functions.Catalog() {
		if f.Key() == key {
			return f, true
		}
	}
	return functions.Function{}, false
}

func corpusJSONType(v any) string {
	switch v.(type) {
	case string:
		return functions.TypeString
	case []any:
		return functions.TypeList
	}
	return ""
}

// corpusDeclaredFields reads the fields the concepts in src declare, to the
// type word a method receiver is typed by: a string-valued field is a string,
// an array a list. Other types are left out -- no method is ambiguous over
// them.
func corpusDeclaredFields(edition, src string) map[string]string {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	file, err := corpusParseFile(edition, src)
	if err != nil || file == nil {
		return nil
	}
	out := map[string]string{}
	for _, def := range file.Definitions {
		c, ok := def.(*ast.ConceptDecl)
		if !ok {
			continue
		}
		for _, p := range c.Properties {
			if p == nil || p.Type == nil {
				continue
			}
			switch p.Type.Kind {
			case "string", "enum", "datetime":
				out[p.Name] = functions.TypeString
			case "array":
				out[p.Name] = functions.TypeList
			}
		}
	}
	return out
}

// corpusLambdaPosition reports the positions whose expression is a lambda over
// the row: a query filter, a spec or trait body, a trigger filter, a refine
// clause.
func corpusLambdaPosition(pos tiers.Position) bool {
	switch pos {
	case tiers.PositionQueryFilter, tiers.PositionSpecBody, tiers.PositionTriggerFilter, tiers.PositionQueryRefine:
		return true
	}
	return false
}

// corpusParseAtPosition parses a lower / evaluate case's source the way its
// position is written: the lambda at a lambda position, one expression
// elsewhere.
func corpusParseAtPosition(pos tiers.Position, src string) (ast.ExpressionNode, error) {
	if corpusLambdaPosition(pos) {
		lam, err := langparser.ParseV1Lambda(src)
		if err != nil {
			return nil, err
		}
		return lam, nil
	}
	return langparser.ParseV1Expression(src)
}

// corpusExpressionsAt reads the expressions a parsed load_ok file writes at
// pos. readable is false for a position the gate does not read out of a whole
// file (the in-process positions; see the file comment).
func corpusExpressionsAt(pos tiers.Position, file *ast.File) (nodes []ast.ExpressionNode, readable bool) {
	literal := func(v any) ast.ExpressionNode { return &ast.LiteralExpr{Value: v} }
	for _, def := range file.Definitions {
		switch d := def.(type) {
		case *ast.FunctionDef:
			if auto, ok := d.Body.(*ast.AutomationDef); ok && auto != nil {
				if pos == tiers.PositionBeforeWriteValue && auto.Body != nil {
					ast.WalkBody(auto.Body.Statements, func(s ast.BodyStatement) bool {
						if f, ok := s.(*ast.FieldWriteStatement); ok {
							nodes = append(nodes, f.Value)
						}
						return true
					})
				}
				if pos == tiers.PositionTriggerFilter && auto.Trigger != nil && auto.Trigger.FilterLambda != nil {
					nodes = append(nodes, auto.Trigger.FilterLambda)
				}
				continue
			}
			if d.Type != ast.FunctionTypeQuery {
				continue
			}
			// A struct-form query reaches the parser as
			// `[directives](concept==<id> [&& (<filter lambda>)])`: the
			// directive wrappers are peeled one at a time down to the join.
			body, _ := d.Body.(ast.ExpressionNode)
			for body != nil {
				var next ast.ExpressionNode
				switch e := body.(type) {
				case *ast.RefineExpr:
					if pos == tiers.PositionQueryRefine && e.Lambda != nil {
						nodes = append(nodes, e.Lambda)
					}
					next = e.Target
				case *ast.SortExpr:
					if pos == tiers.PositionSort {
						for _, f := range e.Fields {
							nodes = append(nodes, literal(f.Field))
						}
					}
					next = e.Target
				case *ast.ShapeExpr:
					next = e.Target
				case *ast.CountExpr:
					next = e.Target
				case *ast.PaginateExpr:
					next = e.Target
				case *ast.TimestampExpr:
					next = e.Target
				case *ast.DepthExpr:
					next = e.Target
				case *ast.SelectExpr:
					next = e.Target
				case *ast.LogicalExpr:
					if lam, ok := e.Right.(*ast.LambdaExpr); ok && e.Op == ast.LogicalAnd && pos == tiers.PositionQueryFilter && lam != nil {
						nodes = append(nodes, lam)
					}
				}
				body = next
			}
		case *ast.SpecDecl:
			if pos == tiers.PositionSpecBody && d.Lambda != nil {
				nodes = append(nodes, d.Lambda)
			}
		case *ast.ConceptDecl:
			if pos != tiers.PositionRowAuthzArgument {
				continue
			}
			for _, a := range d.Attributes {
				if a == nil || a.Name != "rowAuthz" {
					continue
				}
				if a.Value != nil {
					nodes = append(nodes, literal(a.Value))
				}
				keys := make([]string, 0, len(a.Args))
				for k := range a.Args {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					nodes = append(nodes, literal(a.Args[k]))
				}
			}
		case *ast.ToolDecl:
			if pos != tiers.PositionToolDefault {
				continue
			}
			for _, f := range d.Fields {
				if f.Default != "" {
					nodes = append(nodes, literal(f.Default))
				}
			}
		}
	}
	switch pos {
	case tiers.PositionBeforeWriteValue, tiers.PositionQueryFilter, tiers.PositionQueryRefine, tiers.PositionSort, tiers.PositionSpecBody,
		tiers.PositionTriggerFilter, tiers.PositionRowAuthzArgument, tiers.PositionToolDefault:
		return nodes, true
	}
	return nil, false
}

// ---- the retired forms ------------------------------------------------------------

// corpusRetiredMethodRules maps each retired METHOD (functions.RetiredMethods,
// by receiver.name) to the parser rule that refuses it. A retired function's
// rule is derived (retired_<name>_call); a method's is not, so a newly retired
// method fails TestCorpusRefusesEveryRetiredForm until it is named here.
var corpusRetiredMethodRules = map[string]string{
	"list.contains": "retired_contains_method",
}

// corpusWrittenOutReplacements holds the retired forms whose refusal writes
// the replacement out for the author's own text instead of quoting the
// placeholder form (parser.V1RetiredForms says which): the message names the
// rewrite of the case's entry, so the placeholder text never appears in it.
// Each is matched by the shape of that rewrite.
var corpusWrittenOutReplacements = map[string]func(message string) bool{
	// `{ args.x.y }` -> `{ y: args.x.y }`: the entry's key is the last segment
	// of its path.
	"retired_keyless_map_entry": func(message string) bool {
		for _, m := range corpusKeylessRewrite.FindAllStringSubmatch(message, -1) {
			if m[1] == m[3] {
				return true
			}
		}
		return false
	},
}

var corpusKeylessRewrite = regexp.MustCompile(`write ([A-Za-z_][A-Za-z0-9_]*): ((?:[A-Za-z_][A-Za-z0-9_]*\.)+)([A-Za-z_][A-Za-z0-9_]*)`)

// TestCorpusRefusesEveryRetiredForm holds the corpus to showing every spelling
// edition 2026 retires, refused: each parser.V1RetiredForms() rule is the code
// of a refused case whose message names the replacement, and every name the
// function catalog retires (functions.RetiredFunctions, RetiredMethods) is one
// of those rules. A refusal is judged by the parser every loader uses
// (corpusParse), which reads only edition 2026, so the four predicate-position
// forms -- a filter with no lambda header, a spec or trait `{ return ... }`
// body, a raw-text @filter -- are shown refused like the rest.
func TestCorpusRefusesEveryRetiredForm(t *testing.T) {
	runs := discoverCorpus(t)
	byCode := map[string][]*corpusRun{}
	for _, r := range runs {
		if (r.c.Verdict == verdictRefuseParse || r.c.Verdict == verdictRefuseLoad) && r.c.Code != "" {
			byCode[r.c.Code] = append(byCode[r.c.Code], r)
		}
	}

	rules := map[string]langparser.RetiredForm{}
	shown := 0
	for _, f := range langparser.V1RetiredForms() {
		rules[f.Rule] = f
		cases := byCode[f.Rule]
		if len(cases) == 0 {
			t.Errorf("retired form %q has no refused case: write one whose code is %s", f.Spelling, f.Rule)
			continue
		}
		shown++
		for _, r := range cases {
			names := strings.Contains(r.c.Message, f.Replacement)
			if writtenOut, ok := corpusWrittenOutReplacements[f.Rule]; ok {
				names = writtenOut(r.c.Message)
			}
			if !names {
				t.Errorf("%s pins rule %s, but its message does not name the replacement %q", r.rel, f.Rule, f.Replacement)
			}
		}
	}
	t.Logf("retired forms: %d of %d shown refused", shown, len(rules))

	for name := range functions.RetiredFunctions() {
		rule := "retired_" + strings.ToLower(name) + "_call"
		if _, ok := rules[rule]; !ok {
			t.Errorf("the catalog retires %s(), and the parser has no rule %s to refuse it", name, rule)
		} else if len(byCode[rule]) == 0 {
			t.Errorf("the catalog retires %s(), and no refused case shows it (code %s)", name, rule)
		}
	}
	for key := range functions.RetiredMethods() {
		rule, ok := corpusRetiredMethodRules[key]
		if !ok {
			t.Errorf("the catalog retires the method %s; name the parser rule that refuses it in corpusRetiredMethodRules", key)
			continue
		}
		if _, ok := rules[rule]; !ok {
			t.Errorf("the catalog retires the method %s, and the parser has no rule %s", key, rule)
		} else if len(byCode[rule]) == 0 {
			t.Errorf("the catalog retires the method %s, and no refused case shows it (code %s)", key, rule)
		}
	}
}
