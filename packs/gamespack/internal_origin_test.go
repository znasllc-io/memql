package gamespack

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// internal_origin_test.go -- the source-level pin of this package's internal-origin
// stamps, the precedent being integrations/work/internal_origin_test.go
// (TestTheStampNeverEscapesItsCall).
//
// A write under INTERNAL ORIGIN is let through by every @serverOnly gate and by the
// owned tier's write guard, which is why the pack's three writers (createGamesPlanTrace,
// createGamesSkill, recordGamesSkillLadder) can only be reached by its own Go half. The
// rest rests on the shape of the stamp: it is applied INLINE, as the context of the one
// call that needs it, and the marked context is never stored or returned, so no later
// frame can inherit it and open a different @serverOnly construct (the memql#2879 and
// memql#2989 escalation). A fourth stamp, or a stamp held in a variable, is not an error
// in itself; what it costs is that guarantee, and these tests make adding one a deliberate
// act. The call_origin_conformance_test.go entry for packs/gamespack rests on them.

// authImport is the path of component/auth, whose ContextWithInternalOrigin is the stamp.
const authImport = "github.com/znasllc-io/memql/component/auth"

// stampSite is one stamp of internal origin: the function it is in and the construct whose
// rendered call it is on.
type stampSite struct {
	File, Func, Writer string
}

// packageSources reads the package's non-test Go files, name to text.
func packageSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(src)
	}
	if len(out) == 0 {
		t.Fatal("found no source files, so the scan asserts nothing")
	}
	return out
}

// parseAll parses the sources, name to text, with one file set.
func parseAll(t *testing.T, sources map[string]string) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	out := make(map[string]*ast.File, len(sources))
	for name, src := range sources {
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[name] = f
	}
	return out, fset
}

// authName is what a file calls component/auth, or "" when it does not import it.
func authName(f *ast.File) string {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != authImport {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "auth"
	}
	return ""
}

// authSelector is e as <local>.<name>, or nil.
func authSelector(e ast.Node, local, name string) *ast.SelectorExpr {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return nil
	}
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != local {
		return nil
	}
	return sel
}

// renderedConstruct is the name a call to RenderCall renders, when the call is one and the
// name is a string literal.
func renderedConstruct(call *ast.CallExpr) (name string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "RenderCall" || len(call.Args) == 0 {
		return "", false
	}
	if lit, isLit := call.Args[0].(*ast.BasicLit); isLit && lit.Kind == token.STRING {
		if s, err := strconv.Unquote(lit.Value); err == nil {
			return s, true
		}
	}
	return "(not a literal)", true
}

// inlineStamp is the one shape of stamp this package allows,
//
//	s.exec(auth.ContextWithInternalOrigin(ctx), "mutation "+call)
//
// the stamp made from the caller's own context, as the context of the call that runs a
// mutation. It returns the reference to the stamp.
func inlineStamp(call *ast.CallExpr, local string) *ast.SelectorExpr {
	exec, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || exec.Sel.Name != "exec" || len(call.Args) != 2 {
		return nil
	}
	inner, ok := call.Args[0].(*ast.CallExpr)
	if !ok || len(inner.Args) != 1 {
		return nil
	}
	stamp := authSelector(inner.Fun, local, "ContextWithInternalOrigin")
	if stamp == nil {
		return nil
	}
	if arg, ok := inner.Args[0].(*ast.Ident); !ok || arg.Name != "ctx" {
		return nil
	}
	if bin, ok := call.Args[1].(*ast.BinaryExpr); !ok || bin.Op != token.ADD {
		return nil
	} else if lit, ok := bin.X.(*ast.BasicLit); !ok || lit.Kind != token.STRING || lit.Value != `"mutation "` {
		return nil
	}
	return stamp
}

// scanStamps finds every reference to auth.ContextWithInternalOrigin in the files. It
// returns the stamps of the one allowed shape (inlineStamp) in a function that renders the
// call of exactly one construct, by a literal name, and a problem for every reference that
// is anything else: held in a variable, returned, made from another context, on a
// statement that is not the rendered mutation, or in a function that renders no construct
// or several.
func scanStamps(files map[string]*ast.File, fset *token.FileSet) (sites []stampSite, problems []string) {
	for name, f := range files {
		local := authName(f)
		if local == "" {
			continue
		}
		right := map[*ast.SelectorExpr]bool{}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var stamps []*ast.SelectorExpr
			var renders []string
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if construct, ok := renderedConstruct(call); ok {
					renders = append(renders, construct)
				}
				if stamp := inlineStamp(call, local); stamp != nil {
					stamps = append(stamps, stamp)
				}
				return true
			})
			if len(stamps) == 0 {
				continue
			}
			if len(renders) != 1 || renders[0] == "(not a literal)" {
				problems = append(problems, fmt.Sprintf("%s: %s stamps internal origin and renders %v: want exactly one construct, named by a literal", name, fd.Name.Name, renders))
				continue
			}
			for _, stamp := range stamps {
				right[stamp] = true
				sites = append(sites, stampSite{File: name, Func: fd.Name.Name, Writer: renders[0]})
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if stamp := authSelector(n, local, "ContextWithInternalOrigin"); stamp != nil && !right[stamp] {
				problems = append(problems, fmt.Sprintf("%s:%d references auth.ContextWithInternalOrigin other than inline as the context of s.exec(<ctx>, \"mutation \"+call) in a function that renders one construct",
					name, fset.Position(stamp.Pos()).Line))
			}
			return true
		})
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Writer+sites[i].Func < sites[j].Writer+sites[j].Func })
	sort.Strings(problems)
	return sites, problems
}

// returnsContext reports a function declaration that hands a context back.
func returnsContext(fd *ast.FuncDecl) bool {
	if fd.Type.Results == nil {
		return false
	}
	for _, r := range fd.Type.Results.List {
		if sel, ok := r.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Context" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "context" {
				return true
			}
		}
	}
	return false
}

// serverOnlyMutations are the names of the mutations mutations.memql marks @serverOnly.
func serverOnlyMutations(t *testing.T) []string {
	t.Helper()
	raw, err := fs.ReadFile(Tree(), "mutations.memql")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	marked := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "@serverOnly":
			marked = true
		case strings.HasPrefix(line, "mutation "):
			if fields := strings.Fields(line); marked && len(fields) >= 3 {
				out = append(out, fields[2])
			}
			marked = false
		case line == "":
			marked = false
		}
	}
	sort.Strings(out)
	return out
}

// theThreeStamps are the stamps the package makes: one on each of the pack's @serverOnly
// writers, in the method of engineStore that renders it, in the order scanStamps lists them
// (by the writer's name).
var theThreeStamps = []stampSite{
	{"store_internal.go", "createTrace", "createGamesPlanTrace"},
	{"store_internal.go", "createSkill", "createGamesSkill"},
	{"store_internal.go", "recordLadder", "recordGamesSkillLadder"},
}

// Exactly three stamps, one on each @serverOnly writer of the pack, each inline as the
// context of the one call that needs it. The three are named here on purpose: a fourth
// writer, or a second stamp for one of them, fails this until it is added deliberately,
// and the call-origin allowlist entry for this package is reviewed with it.
func TestTheStampNeverEscapesItsCall(t *testing.T) {
	files, fset := parseAll(t, packageSources(t))
	sites, problems := scanStamps(files, fset)
	for _, p := range problems {
		t.Error(p)
	}
	if fmt.Sprint(sites) != fmt.Sprint(theThreeStamps) {
		t.Errorf("the stamp sites are %v, want exactly %v -- one seam for each writer is what keeps the escalation reasoning true of the whole package", sites, theThreeStamps)
	}
	// ...and they are the pack's @serverOnly mutations, no more and no fewer: a writer
	// that is @serverOnly and has no stamp can never be written, and a stamp on anything
	// else opens a construct the pack never meant to.
	var writers []string
	for _, s := range theThreeStamps {
		writers = append(writers, s.Writer)
	}
	sort.Strings(writers)
	if got := serverOnlyMutations(t); fmt.Sprint(got) != fmt.Sprint(writers) {
		t.Errorf("mutations.memql marks %v @serverOnly, the package stamps %v: each @serverOnly writer has exactly one stamp", got, writers)
	}
}

// The structural half: nothing in the package hands a context back, so a context that was
// marked can only be the argument of the call it was made for.
func TestNoFunctionReturnsAContext(t *testing.T) {
	files, fset := parseAll(t, packageSources(t))
	for name, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && returnsContext(fd) {
				t.Errorf("%s:%d: %s returns a context. A context this package builds must die inside the call it was built for.", name, fset.Position(fd.Pos()).Line, fd.Name.Name)
			}
		}
	}
}

// The actor is never touched, which is what lets the owned tier decide every read: the
// only things the package takes from component/auth are the stamp and the reading of the
// caller. A context built with another actor or another access would write as somebody
// else.
func TestTheActorIsNeverTouched(t *testing.T) {
	allowed := map[string]bool{"ContextWithInternalOrigin": true, "AccessFromContext": true}
	files, fset := parseAll(t, packageSources(t))
	used := map[string]bool{}
	for name, f := range files {
		local := authName(f)
		if local == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == local {
				used[sel.Sel.Name] = true
				if !allowed[sel.Sel.Name] {
					t.Errorf("%s:%d uses auth.%s: the pack takes nothing from component/auth but the stamp and the reading of the caller", name, fset.Position(sel.Pos()).Line, sel.Sel.Name)
				}
			}
			return true
		})
	}
	for name := range allowed {
		if !used[name] {
			t.Errorf("auth.%s is no longer used: this test's list of what the pack may take is stale", name)
		}
	}
}

// The pin is not blind: each way of letting the stamp escape, made in a copy of the real
// file, is found. A scan that stopped matching would report a clean tree.
func TestTheStampPinCatchesEveryWayTheStampCouldEscape(t *testing.T) {
	orig, ok := packageSources(t)["store_internal.go"]
	if !ok {
		t.Fatal("store_internal.go is not in the package")
	}
	const (
		stmt   = `if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {`
		render = `call, err := langparser.RenderCall("createGamesSkill", compact(args))`
	)
	if strings.Count(orig, stmt) != 3 || strings.Count(orig, render) != 1 {
		t.Fatalf("store_internal.go does not have the shapes this test mutates: %d inline stamps (want 3), %d renders of createGamesSkill (want 1)",
			strings.Count(orig, stmt), strings.Count(orig, render))
	}
	scan := func(src string) ([]stampSite, []string, bool) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "store_internal.go", src, 0)
		if err != nil {
			return nil, nil, false
		}
		sites, problems := scanStamps(map[string]*ast.File{"store_internal.go": f}, fset)
		return sites, problems, true
	}
	// The control: the real file is three sites and no problem.
	if sites, problems, _ := scan(orig); fmt.Sprint(sites) != fmt.Sprint(theThreeStamps) || len(problems) != 0 {
		t.Fatalf("the control: sites %v, problems %v, want %v and none", sites, problems, theThreeStamps)
	}

	for name, mutant := range map[string]string{
		"a stamp held in a variable":                strings.Replace(orig, stmt, "stamped := auth.ContextWithInternalOrigin(ctx)\n\tif _, err := s.exec(stamped, \"mutation \"+call); err != nil {", 1),
		"a stamp made from another context":         strings.Replace(orig, stmt, `if _, err := s.exec(auth.ContextWithInternalOrigin(context.Background()), "mutation "+call); err != nil {`, 1),
		"a stamp on a query":                        strings.Replace(orig, stmt, `if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "query "+call); err != nil {`, 1),
		"a stamp on a statement of its own":         strings.Replace(orig, stmt, `if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "mutation createGamesSkill()"); err != nil {`, 1),
		"a stamp on another receiver's exec":        strings.Replace(orig, stmt, `if _, err := s.run(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {`, 1),
		"a stamp handed back":                       orig + "\nfunc stampedFor(ctx context.Context) context.Context { return auth.ContextWithInternalOrigin(ctx) }\n",
		"a stamp as a function value":               orig + "\nvar stamp = auth.ContextWithInternalOrigin\n",
		"a function that renders two constructs":    strings.Replace(orig, render, render+"\n\t_, _ = langparser.RenderCall(\"second\", nil)", 1),
		"a function that renders no literal name":   strings.Replace(orig, render, `call, err := langparser.RenderCall(name, compact(args))`, 1),
		"a second stamp in a function that has one": strings.Replace(orig, stmt, stmt+"\n\t\treturn err\n\t}\n\tif _, err := s.exec(auth.ContextWithInternalOrigin(ctx), \"mutation \"+call); err != nil {", 1),
		"a fourth writer": orig + `
func (s engineStore) another(ctx context.Context, args map[string]any) error {
	call, err := langparser.RenderCall("anotherGamesWriter", compact(args))
	if err != nil {
		return err
	}
	if _, err := s.exec(auth.ContextWithInternalOrigin(ctx), "mutation "+call); err != nil {
		return err
	}
	return nil
}
`,
	} {
		t.Run(name, func(t *testing.T) {
			if mutant == orig {
				t.Fatal("the mutant did not change the file")
			}
			sites, problems, parsed := scan(mutant)
			if !parsed {
				t.Fatal("the mutant does not parse, so it tests nothing")
			}
			if len(problems) == 0 && fmt.Sprint(sites) == fmt.Sprint(theThreeStamps) {
				t.Fatalf("the scan found nothing wrong with %s: sites %v", name, sites)
			}
		})
	}
}

// returnsContext sees a function that hands a context back, in either spelling of its
// result, and no other.
func TestReturnsContextRecognisesAFunctionThatHandsOneBack(t *testing.T) {
	files, _ := parseAll(t, map[string]string{"a.go": `package a

import "context"

func handsBack(ctx context.Context) context.Context { return ctx }
func handsBackWithAnError(ctx context.Context) (context.Context, error) { return ctx, nil }
func handsBackNothing(ctx context.Context) error { return ctx.Err() }
func handsBackAString(ctx context.Context) string { return "" }
func handsBackNoResult(ctx context.Context) {}
`})
	got := map[string]bool{}
	for _, d := range files["a.go"].Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			got[fd.Name.Name] = returnsContext(fd)
		}
	}
	want := map[string]bool{"handsBack": true, "handsBackWithAnError": true, "handsBackNothing": false, "handsBackAString": false, "handsBackNoResult": false}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("returnsContext = %v, want %v", got, want)
	}
}
