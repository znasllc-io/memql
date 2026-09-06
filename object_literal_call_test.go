package main

// object_literal_call_test.go -- the repo-wide gate for the object-literal
// call form (memql#5004, memql#5000).
//
// ===========================================================================
// THE DEFECT, FOUR TIMES
// ===========================================================================
// Go that hands the engine a call composes it as text, and there is exactly
// one accepted form: `name(k: v, ...)`. The wrapper `name({...})` -- a single
// positional object literal -- has been REFUSED at parse since Story 9 of
// memql#2335. Rendered from a marshalled map it looks like this, and it has
// been written this way in nineteen places across four separate rounds of
// fixing it:
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
// The eleven this test was written against were: the AI router's per-call
// BILLING record (component/router), both capability-graph edge writers
// (component/skills), mintSkill, the planner's semantic tasks and domain
// embedding, the agent's RAG lookup, a SECOND worker-invocation writer, the
// cockpit-app spend ledger, the router's API-key secret, and a taskState write
// that had never once succeeded.
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
