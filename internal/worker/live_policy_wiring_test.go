package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// liveReaders are the Options fields that read the LIVE policy. Each is a
// function because a SIGHUP changes its answer, and each decides something a
// runner advertises -- or withholds -- on Register.
var liveReaders = []string{"PipelinesPolicy"}

// workerSources parses every non-test Go file of this package, build tags
// regardless: a file built only on darwin builds a Runner on darwin.
func workerSources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files[path] = f
	}
	if len(files) == 0 {
		t.Fatal("parsed no source files: the scan is not where the package is")
	}
	return fset, files
}

// TestEveryRunnerConstructionWiresTheLivePolicy (memql#5494). Three places
// build a Runner -- `worker run`'s single home, the fleet's runner per home,
// and the run that follows `worker pair` -- and a reader one of them forgets
// fails SILENTLY and closed: that runner never advertises the consent, while
// its dispatcher, reading the same policy, admits the work.
func TestEveryRunnerConstructionWiresTheLivePolicy(t *testing.T) {
	var funcs []string
	options := reflect.TypeOf(Options{})
	for i := 0; i < options.NumField(); i++ {
		if options.Field(i).Type.Kind() == reflect.Func {
			funcs = append(funcs, options.Field(i).Name)
		}
	}
	want := append([]string(nil), liveReaders...)
	sort.Strings(funcs)
	sort.Strings(want)
	if strings.Join(funcs, ",") != strings.Join(want, ",") {
		t.Fatalf("Options' function fields are %v but liveReaders names %v. A new reader of the live policy belongs in liveReaders, and so at every place a Runner is built; a function field that reads no policy needs this test taught why it is exempt.", funcs, want)
	}

	fset, files := workerSources(t)
	sites := map[string]int{}
	for path, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if ident, ok := lit.Type.(*ast.Ident); !ok || ident.Name != "Options" {
				return true
			}
			set := map[string]bool{}
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						set[key.Name] = true
					}
				}
			}
			pos := fset.Position(lit.Pos())
			sites[path]++
			for _, reader := range liveReaders {
				if !set[reader] {
					t.Errorf("%s:%d builds a Runner without %s: that runner never advertises what the live policy says, while its dispatcher admits the work", pos.Filename, pos.Line, reader)
				}
			}
			return true
		})
	}
	// The scan reports its own coverage: the places Runners are built today.
	// A site that moves needs this list moved with it; a scan that silently
	// found nothing would pass every assertion above.
	for _, site := range []string{"cli.go", "fleet.go", "pair.go"} {
		if sites[site] == 0 {
			t.Errorf("found no Options literal in %s (found %v): the scan is not looking where Runners are built", site, sites)
		}
	}
}

// TestEverySIGHUPReloadsThroughOneFunction (memql#5494). The run paths each
// handle SIGHUP, and the reload is where a changed consent is announced; a
// path that reloads by hand announces nothing, which is how pair.go came to
// omit the changed repository scope. Every SIGHUP case calls reloadPolicy.
func TestEverySIGHUPReloadsThroughOneFunction(t *testing.T) {
	fset, files := workerSources(t)
	found := 0
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok || !namesSIGHUP(clause.List) {
				return true
			}
			found++
			calls := false
			for _, stmt := range clause.Body {
				ast.Inspect(stmt, func(m ast.Node) bool {
					if call, ok := m.(*ast.CallExpr); ok {
						if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "reloadPolicy" {
							calls = true
						}
					}
					return true
				})
			}
			if !calls {
				pos := fset.Position(clause.Pos())
				t.Errorf("%s:%d handles SIGHUP without reloadPolicy: a changed consent would be reloaded and never announced", pos.Filename, pos.Line)
			}
			return true
		})
	}
	if found < 2 {
		t.Errorf("found %d SIGHUP case(s), want at least the two in cli.go and pair.go: the scan is not looking where signals are handled", found)
	}
}

func namesSIGHUP(exprs []ast.Expr) bool {
	for _, e := range exprs {
		if sel, ok := e.(*ast.SelectorExpr); ok && sel.Sel.Name == "SIGHUP" {
			return true
		}
	}
	return false
}

// A reload says which consent changed, the same way on every run path:
// repository scopes ride Register, so the cluster learns of a change by a
// re-registration the log should account for.
func TestAPolicyReloadAnnouncesAChangedConsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("shell:\n  allow: [ls]\n")
	policy, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}

	write("pipelines:\n  allow: true\n")
	logs := &logBuffer{}
	if !reloadPolicy(slogJSON(logs), policy) {
		t.Fatalf("a valid file did not reload:\n%s", logs)
	}
	for _, want := range []string{"pipeline repository policy changed"} {
		if logs.count(want) != 1 {
			t.Errorf("the reload did not say %q once:\n%s", want, logs)
		}
	}

	logs = &logBuffer{}
	if !reloadPolicy(slogJSON(logs), policy) {
		t.Fatal("an unchanged file did not reload")
	}
	if logs.count("changed") != 0 {
		t.Errorf("an unchanged reload announced a change:\n%s", logs)
	}

	write("inference:\n  serve: cluster\npipelines:\n  allow: true\n  repos: [o/a]\n")
	logs = &logBuffer{}
	if !reloadPolicy(slogJSON(logs), policy) || logs.count("pipeline repository policy changed") != 1 {
		t.Fatalf("repository-only reload was not announced: %s", logs)
	}

	write("pipelines: [not, a, mapping\n")
	logs = &logBuffer{}
	if reloadPolicy(slogJSON(logs), policy) {
		t.Error("a file that does not parse reported a reload")
	}
	if logs.count("policy reload failed") != 1 || !policy.PipelinesAllowed() {
		t.Errorf("a failed reload must say so and keep the policy in force:\n%s", logs)
	}
}
