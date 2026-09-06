package main

// object_literal_call_test.go -- the repo-wide gate for the object-literal
// call form (memql#5004, memql#5000).
//
// ===========================================================================
// THE DEFECT, FOUR TIMES
// ===========================================================================
// Go that hands the engine a MUTATION or a QUERY composes it as text, and
// there is exactly one accepted form: `name(k: v, ...)`. The wrapper
// `name({...})` -- a single positional object literal -- has been REFUSED at
// parse since Story 9 of memql#2335. Rendered from a marshalled map it looks
// like this, and it has been written this way in nineteen places across four
// separate rounds of fixing it:
//
//	payload, _ := json.Marshal(args)
//	engine.Execute(ctx, fmt.Sprintf("createThing(%s)", string(payload)))
//
// memql#4209 fixed component/deploycontrol. memql#4256 fixed the guest and
// auth-session handlers. memql#5004 fixed all eight writes in
// component/worker. Each round fixed the sites it could see and left the rest,
// because there was no way to see the rest -- and each round's gate was
// package-local, so the next occurrence landed somewhere it did not look.
//
// SEVENTEEN of the nineteen were genuinely failing, measured against the real
// engine rather than assumed: the AI router's per-call BILLING record and the
// cockpit-app spend ledger (`recordRouterCall` twice), both capability-graph
// edge writers, `mintSkill` twice, `createAgent`, `updateAgent` three times,
// `createSemanticTask`, a SECOND worker-invocation writer, `setPartitionSecret`,
// `createSkillChangeEvent` twice, `mutationCreateCanvasState` twice, and a
// `persistTaskState` write that had never once succeeded.
//
// THE OTHER TWO WERE FINE, and the difference is why this gate reports a SHAPE
// rather than claiming a breakage. `similarTo` and `embedDomainItems` are
// PRIMITIVE BUILTINS, and parseFunctionCallWithKind's own comment says so:
// "bare positional primitive args ... stay valid". Both accept the positional
// object AND the named form, and validate their required fields identically
// under each -- measured. Rewriting them was consistency, not repair.
//
// That distinction was got wrong first. The sweep's claim was "nineteen writes
// that never succeeded", which was true of seventeen; two of the probes that
// would have caught it were run and misread, because a builtin answering
// "requires 'concept' field" is an ARGUMENT error and reads at a glance like
// the parse refusal next to it.
//
// ===========================================================================
// WHY IT IS SILENT, WHICH IS WHY IT NEEDS A GATE RATHER THAN REVIEW
// ===========================================================================
// The Go compiles. The string is well-formed. The engine returns an error the
// caller usually logs at WARN and swallows, because these are all
// best-effort record-keeping writes -- which is exactly the code where a
// swallowed failure is invisible. And every suite that drives a recording
// engine stays green, because a recorder parses nothing.
//
// ===========================================================================
// WHAT THIS DETECTS
// ===========================================================================
// A `json.Marshal` result substituted WHOLE into a call's parentheses. That is
// the shape all nineteen had, and it is narrow enough to have no false
// positives in this tree: a correct renderer joins per-argument `k: v` pairs,
// so its Sprintf argument is a strings.Join or a Builder, never `string(x)`
// where x came out of json.Marshal.
//
// It reports the SHAPE and does not resolve the name, so it flags the two
// primitive-builtin sites too. That is the right trade in this direction: the
// shape is a reliable signal that somebody reached for the wrapper, the fix
// costs nothing where the wrapper was legal, and a gate that had to know which
// construct is a builtin would be a second copy of the registry.
//
// It does NOT detect an object literal written as a string constant -- the
// parser's own negative_grammar_test.go covers that -- nor a call assembled by
// a renderer that is itself wrong. The per-package gate for the latter is a
// test that drives the real methods and parses what they produced; see
// component/worker/render_parses_test.go.
//
// THE FIX is parser.RenderCall, which renders the named form from the same
// map.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// callWithOneSubstitution matches a format string that renders a call whose
// parentheses contain exactly one substitution and nothing else: `name(%s)`,
// `mutation name(%s)`, `%s(%s)`.
var callWithOneSubstitution = regexp.MustCompile(`\(%s\)\s*$`)

func TestNoGoCallSiteRendersTheObjectLiteralForm(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Fields(string(out))
	if len(files) == 0 {
		t.Fatal("git ls-files matched no Go files -- the walk is broken, not the tree")
	}

	fset := token.NewFileSet()
	var findings []string
	scanned := 0

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A file this walk cannot parse is not a pass. Say so rather
			// than skipping it silently.
			t.Errorf("parsing %s: %v", path, perr)
			continue
		}
		scanned++

		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			marshalled := marshalledIdents(fn.Body)
			if len(marshalled) == 0 {
				return true
			}
			for _, site := range objectLiteralSites(fn.Body, marshalled) {
				findings = append(findings, path+":"+strconv.Itoa(fset.Position(site.Pos()).Line))
			}
			return true
		})
	}

	if scanned == 0 {
		t.Fatal("scanned no files")
	}
	sort.Strings(findings)
	for _, f := range findings {
		t.Errorf("%s renders a MemQL call by substituting a json.Marshal result whole into its "+
			"parentheses -- `name({...})`, the object-literal form the parser has refused since "+
			"memql#2335. The write fails at parse, and on every path this shape appears the error "+
			"is logged and swallowed. Render it with parser.RenderCall(name, args) instead.", f)
	}
}

// marshalledIdents names the local variables assigned from json.Marshal in one
// function body.
func marshalledIdents(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, "json", "Marshal") {
			return true
		}
		// `v, err := json.Marshal(x)` -- the first result is the bytes.
		if len(assign.Lhs) > 0 {
			if ident, ok := assign.Lhs[0].(*ast.Ident); ok {
				out[ident.Name] = true
			}
		}
		return true
	})
	return out
}

// objectLiteralSites finds the Sprintf calls that put one of those variables
// whole inside a rendered call's parentheses.
func objectLiteralSites(body *ast.BlockStmt, marshalled map[string]bool) []ast.Node {
	var out []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, "fmt", "Sprintf") || len(call.Args) < 2 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		format, uerr := strconv.Unquote(lit.Value)
		if uerr != nil || !callWithOneSubstitution.MatchString(format) {
			return true
		}
		// The LAST argument fills the last verb, which is the one inside the
		// parentheses.
		last := call.Args[len(call.Args)-1]
		conv, ok := last.(*ast.CallExpr)
		if !ok || len(conv.Args) != 1 {
			return true
		}
		if fnIdent, ok := conv.Fun.(*ast.Ident); !ok || fnIdent.Name != "string" {
			return true
		}
		arg, ok := conv.Args[0].(*ast.Ident)
		if !ok || !marshalled[arg.Name] {
			return true
		}
		out = append(out, call)
		return true
	})
	return out
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}
