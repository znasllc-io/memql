package dslconformance

// clustere2e_named_calls_test.go -- memql#4212.
//
// # The defect
//
// test/clustere2e is build-tagged, so nothing in CI ran it, and it rotted in
// two ways. The first -- it stopped compiling -- is caught by the compile/vet
// lane in ci.yml. The second cannot be: ten of its tests named a construct in
// a STRING (`qc.ExecuteNamed(ctx, "mutationCreateSpace", ...)`) that no longer
// existed on the cluster. A rename to its successor did not help either,
// because the successor (`createSpace`) is product-pack DSL: the `space`
// concept and its mutations are delivered at runtime through MEMQL_DSL_PATH,
// and the parity cluster this suite runs against (the engine-bff component,
// no bundle) has no pack at all. A string can name anything; the compiler
// checks none of it.
//
// # The rule
//
// Every literal name passed to QueryClient.ExecuteNamed inside test/clustere2e
// must be a construct the ENGINE tree declares -- a query, mutation, logic or
// builtin in dsl/. The generated SDK methods (qc.CreateNote, qc.PlansForSpace,
// ...) are already compile-checked against the tree and need no gate; this
// covers the escape hatch they do not.
//
// A name built at runtime (construct_training_test.go composes its trained
// construct's name from a per-run suffix) is not a literal and is not checked;
// those constructs are created by the test itself and cannot be declared here.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	languageAst "github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/dsl"
)

// engineCallableNames returns every construct name ExecuteNamed can resolve on
// an engine-only cluster, read off the parsed embedded tree.
func engineCallableNames(t *testing.T) map[string]string {
	t.Helper()
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatalf("load tree: %v", err)
	}
	names := map[string]string{}
	for _, file := range tree.Files {
		if file == nil {
			continue
		}
		for _, def := range file.Definitions {
			switch d := def.(type) {
			case *languageAst.FunctionDef:
				switch d.Type {
				case languageAst.FunctionTypeQuery, languageAst.FunctionTypeMutation,
					languageAst.FunctionTypeLogic, languageAst.FunctionTypeBuiltin:
					names[d.Name] = string(d.Type)
				}
			case *languageAst.BuiltinDecl:
				names[d.Name] = "builtin"
			}
		}
	}
	if len(names) < 100 {
		t.Fatalf("parsed only %d callable constructs from the embedded tree; the loader narrowed, this gate cannot pass vacuously", len(names))
	}
	return names
}

// executeNamedLiterals returns every literal construct name passed to an
// ExecuteNamed call under test/clustere2e, keyed by position.
func executeNamedLiterals(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "test", "clustere2e")
	fset := token.NewFileSet()
	// Build tags are irrelevant to parsing: every file in the directory is
	// read regardless of its //go:build line, which is the point -- the tag
	// is what hides these files from every other gate.
	pkgs, err := parser.ParseDir(fset, dir, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "ExecuteNamed" || len(call.Args) < 2 {
					return true
				}
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true // a name composed at runtime; not a literal
				}
				name, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				out[fset.Position(lit.Pos()).String()] = name
				return true
			})
		}
	}
	if len(out) == 0 {
		t.Fatal("found no literal ExecuteNamed call under test/clustere2e -- either the suite stopped using " +
			"the escape hatch (delete this gate with it) or the scanner broke")
	}
	return out
}

func TestClusterE2ENamedCallsExistOnTheEngine(t *testing.T) {
	declared := engineCallableNames(t)
	literals := executeNamedLiterals(t)

	positions := make([]string, 0, len(literals))
	for pos := range literals {
		positions = append(positions, pos)
	}
	sort.Strings(positions)
	for _, pos := range positions {
		name := literals[pos]
		if kind, ok := declared[name]; ok {
			t.Logf("%s: %s (%s)", pos, name, kind)
			continue
		}
		t.Errorf("%s: ExecuteNamed names %q, which no query / mutation / logic / builtin in the engine's "+
			"dsl/ tree declares. The parity cluster the suite runs against loads no product pack, so a "+
			"pack-owned construct (the space concept and its mutations, for one) cannot be called from "+
			"here; drive an engine-owned construct instead, through its generated SDK method where one "+
			"exists (memql#4212).", pos, name)
	}
}
