package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestDSLCallStringsDoNotUseGoQuoting refuses Go's %q inside a string that is
// composed into a MemQL statement (znasllc-io/memql#3611).
//
// # The defect
//
// Go's %q and the MemQL lexer do not agree on the escape set, and the
// disagreement is a hard error rather than a fallback. readString implements
// the JSON escapes and only those -- `" \ / b f n r t u` -- and returns
// `invalid escape character` for anything else. %q emits Go's set, which
// includes `\x00`, `\a`, `\v` and `\xNN`. So a single control byte, or one
// invalid UTF-8 byte, anywhere in an interpolated value makes the WHOLE
// statement fail to parse -- and the write never happens. Not a mangled value:
// no row at all, and on the read path no result.
//
// The correct renderer is langparser.QuoteString
// (component/language/parser/quote.go), which lives beside the lexer
// deliberately: the only correct definition of "a MemQL string literal" is
// "what readString accepts", and a renderer that lives anywhere else drifts
// from it the moment the escape set changes. That file's comment is the long
// version of everything above.
//
// # Why a gate and not just a fix
//
// Because the fix has now shipped narrower than the defect twice. memql#3035
// fixed component/outbound. memql#3192 swept a named list and fixed the SDK
// GENERATOR -- and left 129 hand-built call sites across 42 files, including
// the unauthenticated `POST /device/code`, still emitting %q. Nobody added a
// gate either time, so the third occurrence was found the same way as the
// first two: by someone going looking.
//
// A convention that has been re-broken twice is not a convention. This is the
// executable form of it.
//
// # What it recognises, and what it cannot
//
// Two literal shapes, both measured against the whole tree so that neither
// fires on ordinary prose:
//
//   - a DSL CALL -- `mutation createFoo(` / `query fooById(` / `builtin bar(`,
//     including the `mutation %s(` form where the function name is itself
//     interpolated;
//   - a DSL ARGUMENT-LIST CONTINUATION -- `, summary:%q` -- which is how the
//     strings.Builder call sites in integrations/{agent,workbench,library}
//     append optional arguments. These carry no keyword, so the first rule
//     cannot see them, and they were a third of the real sites.
//
// Both are deliberately narrow, and there are shapes they do NOT catch:
//
//   - a BARE named call (`createAgent(agentId: %q)`, no keyword). Every
//     occurrence in the tree is in a test; a rule wide enough to catch it
//     matches any Go format string of the form `foo(bar: %q)` and is worse
//     than no rule.
//   - a helper INDIRECTION -- a `quote()` of one's own that a DSL string then
//     interpolates with %s. That was component/identity/adminops, the bug as a
//     one-line function. quotingHelperRule below covers the specific shape it
//     had; a general version needs type information this sweep does not have.
//
// So a green result is "none of the known shapes is present", not "no site can
// possibly be wrong". The remaining assurance is a round-trip test at the call
// site -- see component/identity/devicecode/store_quoting_test.go, which runs
// the real parser over the real emitted statement.
//
// # Why it lives in the root package
//
// Same reason as area_graph_dag_test.go, product_neutrality_test.go and
// clients_allowlist_test.go, which is the convention this file follows: a
// `git ls-files` sweep reads files the Go test cache cannot know it depends
// on, so in any cacheable package it reports a stale green over a tree it
// never looked at. The root package runs uncached in both CI lanes.
func TestDSLCallStringsDoNotUseGoQuoting(t *testing.T) {
	// selfName is skipped because this file names the forbidden verb by
	// necessity, in prose and in the patterns below.
	const selfName = "dsl_call_quoting_test.go"

	fset := token.NewFileSet()
	var findings []string

	for _, rel := range trackedGoFiles(t) {
		if rel == selfName {
			continue
		}
		src, err := os.ReadFile(rel)
		if err != nil {
			continue // deleted-in-worktree etc.
		}
		f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			// A tracked .go file that does not parse is not this gate's
			// business -- go build and go vet own that -- but it must not
			// read as clean either.
			t.Logf("SKIPPED (does not parse): %s: %v", rel, err)
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !goQuoteVerb.MatchString(s) {
				return true
			}
			var why string
			switch {
			case dslCallLiteral.MatchString(s):
				why = "a MemQL call string"
			case isDSLArgFragment(s):
				why = "a MemQL argument-list continuation"
			default:
				return true
			}
			findings = append(findings, positionOf(fset, lit.Pos())+": "+why+" interpolates a value with %q\n      "+strconv.Quote(elide(s)))
			return true
		})

		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || !isGoQuoteHelper(fn) {
				return true
			}
			findings = append(findings, positionOf(fset, fn.Pos())+": func "+fn.Name.Name+
				" is a one-line %q quoting helper\n      "+
				"If it renders a MemQL literal it is this defect behind an indirection "+
				"(that is exactly what component/identity/adminops.quote was). "+
				"If it renders something else, strconv.Quote says so plainly.")
			return true
		})
	}

	if len(findings) == 0 {
		return
	}
	sort.Strings(findings)
	t.Errorf("Go's %%q escape grammar reached %d MemQL statement site(s).\n\n"+
		"%%q emits \\x00, \\a, \\v and \\xNN. The MemQL lexer implements the JSON escapes\n"+
		"and rejects all of those, so ONE control byte or invalid UTF-8 byte in the value\n"+
		"makes the whole statement unparseable -- the write does not happen, and nothing\n"+
		"reports that it did not.\n\n"+
		"Render the value with langparser.QuoteString instead and use %%s:\n\n"+
		"    -  fmt.Sprintf(`mutation createFoo(name:%%q)`, name)\n"+
		"    +  fmt.Sprintf(`mutation createFoo(name:%%s)`, langparser.QuoteString(name))\n\n"+
		"    import langparser \"github.com/znasllc-io/memql/component/language/parser\"\n\n"+
		"See component/language/parser/quote.go for why that is the only correct\n"+
		"definition, and memql#3035 / #3192 / #3611 for what it has cost so far.\n\n"+
		"  %s", len(findings), strings.Join(findings, "\n  "))
}

// goQuoteVerb matches the %q verb with any flag/width prefix. %%q is not a
// verb, and Unquote-then-match cannot tell the difference -- so a literal
// containing only `%%q` would be a false positive. None exists; if one ever
// does, the fix is to scan verbs rather than to widen this.
var goQuoteVerb = regexp.MustCompile(`%[-+# 0]*[0-9]*q`)

// dslCallLiteral matches a MemQL call: a construct keyword, the function name,
// and its open paren.
//
// The name may be interpolated (`mutation %s(`, as in
// component/identity/devicecode's transition + lookup helpers), and that
// alternative is deliberately tighter than the identifier one: it admits only
// %s / %v and requires the paren to follow IMMEDIATELY. Allowing %q there, or
// allowing a space, matches ordinary diagnostics like
//
//	"no finding for query %q (got %d findings)"
//	"%s: mutation %q (concept %q)%s"
//
// which are six real occurrences in this tree and are not this defect.
var dslCallLiteral = regexp.MustCompile(
	`(?:^|[^A-Za-z0-9_])(query|mutation|logic|builtin|seed)\s+` +
		`(?:[A-Za-z_][A-Za-z0-9_]*\s*\(|%[-+# 0]*[0-9]*[sv]\()`)

var dslArgPair = regexp.MustCompile(`^\s*[A-Za-z_][A-Za-z0-9_]*\s*:\s*[^\s].*$`)

// isDSLArgFragment reports whether s is a continuation of a MemQL argument
// list -- `, summary:%q` or `, attachmentId:%q, mimeType:%q`, optionally
// closing the call.
//
// The leading comma is what makes this precise. Without it the rule matches
// the extremely common `something: %q` tail of an error message ("no usable
// remedy: %q") and produces hundreds of false positives; with it, the whole
// tree yields nineteen hits and every one is a real DSL fragment. Every part
// must be a `key: value` pair, which is what keeps prose out.
func isDSLArgFragment(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, ",") {
		return false
	}
	t = strings.TrimSpace(strings.TrimPrefix(t, ","))
	t = strings.TrimSpace(strings.TrimSuffix(t, ")"))
	if t == "" {
		return false
	}
	for _, part := range strings.Split(t, ",") {
		if !dslArgPair.MatchString(part) {
			return false
		}
	}
	return true
}

// isGoQuoteHelper reports whether fn is exactly
// `func f(x string) string { return fmt.Sprintf("%q", x) }`.
//
// The shape is worth naming because it is how the defect hid from the two
// previous sweeps: adminops declared one and eleven call sites then rendered
// their values with a %s that greps clean. The narrowness is the point -- a
// single-statement body and the literal format `"%q"`, nothing else.
func isGoQuoteHelper(fn *ast.FuncDecl) bool {
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return false
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	call, ok := ret.Results[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Sprintf" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return false
	}
	format, ok := call.Args[0].(*ast.BasicLit)
	if !ok || format.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(format.Value)
	return err == nil && s == "%q"
}

// trackedGoFiles returns every git-tracked .go path. Tracked only: an
// untracked scratch file in someone's worktree is not repo content, the same
// rule product_neutrality_test.go applies.
func trackedGoFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel != "" {
			files = append(files, rel)
		}
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned no .go files -- the sweep would report a vacuous pass")
	}
	return files
}

func positionOf(fset *token.FileSet, pos token.Pos) string {
	p := fset.Position(pos)
	return p.Filename + ":" + strconv.Itoa(p.Line)
}

// elide keeps a long statement readable in the failure output while leaving
// enough of it to locate.
func elide(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= 160 {
		return s
	}
	return s[:160] + "..."
}
