package parser

import (
	"fmt"
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// clauseDispatch names, per construct, the switch its body parser dispatches
// clauses on: the file and function, and the switch's tag (empty: every switch
// in the function). beforeSwitch are clauses the parser consumes before the
// switch runs; refusalArms are arms that exist only to refuse a retired or
// foreign clause with a pointed message; notClauses are arms that are not body
// clauses (the action's one capability call).
var clauseDispatch = map[string]struct {
	file, fn, tag string
	beforeSwitch  []string
	refusalArms   []string
	notClauses    []string
}{
	// The query's `args { }` block is cut out by argsBlockHeader before the
	// line switch runs; its `concept` arm refuses the retired inline concept
	// line.
	"query":      {file: "rewriter.go", fn: "parseStructQueryBody", beforeSwitch: []string{"args"}, refusalArms: []string{"concept"}},
	"mutate":     {file: "rewriter.go", fn: "parseStructMutationBody"},
	"logic":      {file: "rewriter.go", fn: "logicBodyClause"},
	"automation": {file: "rewriter.go", fn: "automationBodyClause"},
	"action": {file: "parser.go", fn: "parseActionDecl", tag: "key",
		refusalArms: []string{"intent", "params", "argTemplate", "body"}, notClauses: []string{"capability"}},
	"capability": {file: "parser.go", fn: "parseCapabilityDecl", tag: "p.current.Literal", refusalArms: []string{"body"}},
	"provider":   {file: "provider_decl.go", fn: "parseProviderDecl", tag: "blockName"},
}

// dispatchClauses reads the clause names a body parser's dispatch compares
// against: the string literals in the case lists of fn's switches whose tag
// renders as tag (every switch when tag is empty).
func dispatchClauses(t *testing.T, file, fn, tag string) (map[string]bool, bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var decl *goast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*goast.FuncDecl); ok && fd.Name.Name == fn {
			decl = fd
		}
	}
	if decl == nil || decl.Body == nil {
		return nil, false
	}
	out := map[string]bool{}
	goast.Inspect(decl.Body, func(n goast.Node) bool {
		sw, ok := n.(*goast.SwitchStmt)
		if !ok || (tag != "" && renderGoExpr(sw.Tag) != tag) {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*goast.CaseClause)
			if !ok {
				continue
			}
			for _, e := range cc.List {
				goast.Inspect(e, func(n goast.Node) bool {
					if lit, ok := n.(*goast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							out[s] = true
						}
					}
					return true
				})
			}
		}
		return true
	})
	return out, true
}

// renderGoExpr renders a switch tag -- an identifier or a selector chain.
func renderGoExpr(e goast.Expr) string {
	switch x := e.(type) {
	case *goast.Ident:
		return x.Name
	case *goast.SelectorExpr:
		return renderGoExpr(x.X) + "." + x.Sel.Name
	}
	return ""
}

// TestBodyClausesMatchTheParserDispatch pins every construct's clause table to
// the switch its body parser dispatches on, so a clause added to or removed
// from a parser without the table fails here by name -- for every construct,
// not only the two whose case arms feed the grammar digest.
func TestBodyClausesMatchTheParserDispatch(t *testing.T) {
	for keyword := range bodyClauseTable {
		if _, ok := clauseDispatch[keyword]; !ok {
			t.Errorf("%s has a clause table but no dispatch entry in clauseDispatch", keyword)
		}
	}
	for keyword, d := range clauseDispatch {
		arms, ok := dispatchClauses(t, d.file, d.fn, d.tag)
		if !ok {
			t.Errorf("%s: no func %s in %s -- the construct's body clauses are dispatched nowhere, so this pin reads nothing", keyword, d.fn, d.file)
			continue
		}
		for _, c := range d.beforeSwitch {
			arms[c] = true
		}
		for _, list := range [][]string{d.refusalArms, d.notClauses} {
			for _, c := range list {
				if !arms[c] {
					t.Errorf("%s: clauseDispatch excepts the %q arm, which %s no longer has -- drop the exception", keyword, c, d.fn)
				}
				delete(arms, c)
			}
		}
		table := map[string]bool{}
		for _, c := range BodyClauses(keyword) {
			table[c] = true
		}
		if got, want := sortedKeySet(table), sortedKeySet(arms); got != want {
			t.Errorf("%s: BodyClauses(%q) = %s, but %s dispatches %s -- update bodyClauseTable (body_clauses.go) in the same change as the parser",
				keyword, keyword, got, d.fn, want)
		}
	}
}

// bodyClauseFixtures is, per construct and clause, the smallest construct
// that uses the clause.
var bodyClauseFixtures = map[string]map[string]string{
	"query": {
		"args":     "query thing probe {\n  args {\n    id string\n  }\n  filter row => row.id == args.id\n}",
		"filter":   "query thing probe {\n  filter row => row.id != \"\"\n}",
		"refine":   "query thing probe {\n  filter row => row.id != \"\"\n  refine row => row.id != \"\"\n  paginate 25\n}",
		"shape":    "query thing probe {\n  filter row => row.id != \"\"\n  shape probeCard\n}",
		"sort":     "query thing probe {\n  filter row => row.id != \"\"\n  sort \"row.createdAt\", \"desc\"\n}",
		"paginate": "query thing probe {\n  filter row => row.id != \"\"\n  paginate 25\n}",
		"asOf":     "query thing probe {\n  filter row => row.id != \"\"\n  asOf latest\n}",
		"count":    "query thing probe {\n  filter row => row.id != \"\"\n  count\n}",
	},
	"mutate": {
		"args":   "mutate thing probe {\n  args {\n    id string!\n  }\n  insert {\n    id: args.id\n  }\n}",
		"insert": "mutate thing probe {\n  insert {\n    id: \"x\"\n  }\n}",
		"update": "mutate thing probe {\n  update {\n    id: \"x\"\n  }\n}",
		"accept": "mutate thing probe {\n  args {\n    name string!\n  }\n  accept { name }\n}",
		"stamp":  "mutate thing probe {\n  args {\n    name string!\n  }\n  accept { name }\n  stamp { createdAt: now }\n}",
	},
	"logic": {
		"args": "logic probe {\n  args {\n    x string\n  }\n  body {\n    return args.x\n  }\n}",
		"body": "logic probe {\n  body {\n    return 1\n  }\n}",
	},
	"automation": {
		"args": "automation probe {\n  args {\n    x any\n  }\n  step run {\n    mutation createThing (id: \"x\")\n  }\n}",
		"step": "automation probe {\n  step run {\n    mutation createThing (id: \"x\")\n  }\n}",
		// component/automations extracts a precondition before the rewriter
		// runs; one that reaches the rewriter (an editor lowering the file) is
		// a known clause it skips.
		"precondition": "automation probe {\n  precondition ready {\n    check: 1 == 1\n  }\n  step run {\n    mutation createThing (id: \"x\")\n  }\n}",
	},
	"action": {
		"args": "action probe {\n  args {\n    x string\n  }\n  capability script(script: args.x)\n}",
	},
	"capability": {
		"args": "capability integration.probe.run {\n  args {\n    x string\n  }\n}",
	},
	"provider": {
		"params": "provider probe {\n  params {\n    contextWindow 1\n  }\n}",
		"auth":   "provider probe {\n  auth {\n    key \"x\"\n  }\n}",
	},
}

// TestBodyClausesMatchWhatTheParsersAccept: every clause in the table parses
// on the authored path.
func TestBodyClausesMatchWhatTheParsersAccept(t *testing.T) {
	for keyword, clauses := range bodyClauseTable {
		for _, clause := range clauses {
			src, ok := bodyClauseFixtures[keyword][clause]
			if !ok {
				t.Errorf("%s: no fixture for the %q clause -- add one to bodyClauseFixtures", keyword, clause)
				continue
			}
			if !grammarSurfaceOutcome(src) {
				normalised, nerr := NormaliseAll(src)
				_, perr := ParseFile(normalised)
				t.Errorf("%s: the %q clause the table lists is refused:\n%s\nrewrite: %v\nparse: %v", keyword, clause, src, nerr, perr)
			}
		}
	}
	for keyword := range bodyClauseFixtures {
		if _, ok := bodyClauseTable[keyword]; !ok {
			t.Errorf("fixture for %q, which the table does not list", keyword)
		}
	}
}

// bodyProbeFixtures is, per construct, a minimal body that parses, with %s
// where a probed clause goes.
var bodyProbeFixtures = map[string]string{
	"query":      "query thing probe {\n  filter row => row.id != \"\"\n  %s\n}",
	"mutate":     "mutate thing probe {\n  insert {\n    id: \"x\"\n  }\n  %s\n}",
	"logic":      "logic probe {\n  %s\n  body {\n    return 1\n  }\n}",
	"automation": "automation probe {\n  %s\n  step run {\n    mutation createThing (id: \"x\")\n  }\n}",
	"action":     "action probe {\n  %s\n  capability script(script: \"x\")\n}",
	"capability": "capability integration.probe.run {\n  %s\n}",
	"provider":   "provider probe {\n  %s\n}",
}

// TestUnlistedClausesAreRefused: a clause word the construct's table does not
// list -- any clause some other construct takes, the retired ones, and a word
// no construct knows -- is refused in the construct's body in every shape a
// clause takes (a block, a named block, a line). A body parser that keeps the
// clauses it recognises and drops the rest accepts all of them: the logic and
// automation emitters did, until memql#5359, so a `filter` line in an
// automation loaded and filtered nothing.
func TestUnlistedClausesAreRefused(t *testing.T) {
	vocabulary := map[string]bool{"junk": true}
	for _, clauses := range bodyClauseTable {
		for _, c := range clauses {
			vocabulary[c] = true
		}
	}
	for _, d := range clauseDispatch {
		for _, c := range d.refusalArms {
			vocabulary[c] = true
		}
	}
	shapes := []string{"%s {\n  }", "%s probe {\n  }", "%s x"}
	for keyword := range bodyClauseTable {
		fixture, ok := bodyProbeFixtures[keyword]
		if !ok {
			t.Errorf("%s: no probe fixture in bodyProbeFixtures", keyword)
			continue
		}
		if !grammarSurfaceOutcome(fmt.Sprintf(fixture, "")) {
			t.Errorf("%s: the probe fixture itself does not parse", keyword)
			continue
		}
		listed := map[string]bool{}
		for _, c := range BodyClauses(keyword) {
			listed[c] = true
		}
		for _, c := range clauseDispatch[keyword].notClauses {
			listed[c] = true
		}
		for word := range vocabulary {
			if listed[word] {
				continue
			}
			for _, shape := range shapes {
				clause := fmt.Sprintf(shape, word)
				if src := fmt.Sprintf(fixture, clause); grammarSurfaceOutcome(src) {
					t.Errorf("%s: the unlisted clause `%s` parses:\n%s", keyword, strings.ReplaceAll(clause, "\n", " "), src)
				}
			}
		}
	}
}

// TestLineClausesAreBodyClauses: every line clause is a clause some body
// accepts, so the editor never inserts a clause no construct takes.
func TestLineClausesAreBodyClauses(t *testing.T) {
	all := map[string]bool{}
	for _, clauses := range bodyClauseTable {
		for _, c := range clauses {
			all[c] = true
		}
	}
	for c := range lineClauses {
		if !all[c] {
			t.Errorf("line clause %q is in no construct's body", c)
		}
	}
	if IsLineClause("args") || !IsLineClause("filter") {
		t.Error("args opens a block and filter takes its line")
	}
}

func sortedKeySet(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
