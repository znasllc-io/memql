package cidb

// Negative cases keep the native coverage scanner from silently narrowing.
import (
	"github.com/znasllc-io/memql/component/database/dbtest"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCovers(t *testing.T) {
	cases := []struct {
		name string
		pkgs []string
		dir  string
		want bool
	}{
		{"exact", []string{"./component/memql"}, "component/memql", true},
		{"recursive self", []string{"./component/memql/..."}, "component/memql", true},
		{"recursive child", []string{"./component/memql/..."}, "component/memql/sub", true},
		{"recursive is not a prefix match", []string{"./component/automations/steps/..."}, "component/automations", false},
		{"sibling prefix is not covered", []string{"./component/mem/..."}, "component/memql", false},
		{"unrelated", []string{"./examples/referencepack/..."}, "component/grpc", false},
		{"whole module", []string{"./..."}, "anything/at/all", true},
		{"empty selector", nil, "component/memql", false},

		// An EXACT argument must not behave as a prefix. Relaxing the default
		// arm to HasPrefix is the obvious-looking simplification and is wrong:
		// `./component/mem` would then "cover" component/memql.
		{"exact arg is not a prefix", []string{"./component/mem"}, "component/memql", false},
		{"exact arg does not reach children", []string{"./component/memql"}, "component/memql/sense", false},

		// Trailing slashes are how these are usually written by hand; dropping
		// the trim makes every such argument match nothing.
		{"trailing slash exact", []string{"./component/memql/"}, "component/memql", true},
		{"trailing slash is not a prefix", []string{"./component/mem/"}, "component/memql", false},

		// `./` and `.` are the NARROWEST selector -- the root package only.
		// Folding them in with `./...` read the narrowest argument as the
		// widest and made the whole coverage gate vacuous.
		{"dot slash is the root package only", []string{"./"}, "component/memql", false},
		{"dot slash covers the root", []string{"./"}, ".", true},
		{"bare dot is the root package only", []string{"."}, "component/memql", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := covers(tc.pkgs, tc.dir); got != tc.want {
				t.Errorf("covers(%v, %q) = %v, want %v", tc.pkgs, tc.dir, got, tc.want)
			}
		})
	}
}

// TestCovers_ParentIsNotCoveredByAChildSelector pins the live subtlety the
// audit on memql#2886 found: `./component/automations/steps/...` does NOT reach
// component/automations, so the parent's db-gated tests never run (memql#3030).
func TestCovers_ParentIsNotCoveredByAChildSelector(t *testing.T) {
	if covers([]string{"./component/automations/steps/..."}, "component/automations") {
		t.Fatal("a child selector must not be read as covering its parent package; " +
			"component/automations' db-gated tests genuinely do not run in the lane")
	}
}

func parseSrc(t *testing.T, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "x_test.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	return f
}

func TestImportsDBTest(t *testing.T) {
	withImport := `package x
import (
	"testing"
	"github.com/znasllc-io/memql/component/database/dbtest"
)
func TestA(t *testing.T) { _ = dbtest.DSN() }
`
	if !importsDBTest(parseSrc(t, withImport)) {
		t.Error("a file importing dbtest was not recognised as db-gated")
	}

	// A comment mentioning dbtest is not an import. Keying on prose is how the
	// hand-maintained ci.yml audit drifted in the first place.
	mentionOnly := `package x
import "testing"
// This suite is like the dbtest ones but needs no database.
// github.com/znasllc-io/memql/component/database/dbtest
func TestA(t *testing.T) {}
`
	if importsDBTest(parseSrc(t, mentionOnly)) {
		t.Error("a file merely naming dbtest in a comment must not count as db-gated")
	}

	// A different package with a confusable name must not match.
	lookalike := `package x
import "github.com/znasllc-io/memql/component/database/dbtesthelper"
func TestA() {}
`
	if importsDBTest(parseSrc(t, lookalike)) {
		t.Error("dbtesthelper is not dbtest; the match must be on the full import path")
	}
}

func TestTestFuncNames(t *testing.T) {
	src := `package x
import "testing"
func TestMain(m *testing.M) {}
func TestAlpha(t *testing.T) {}
func TestBeta(t *testing.T) {}
func Testify(t *testing.T) {}
func BenchmarkGamma(b *testing.B) {}
func helper() {}
type S struct{}
func (S) TestMethod(t *testing.T) {}
`
	got := testFuncNames(parseSrc(t, src))
	want := []string{"TestAlpha", "TestBeta"}
	if len(got) != len(want) {
		t.Fatalf("testFuncNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("testFuncNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestTestFuncNames_ExcludesTestMain pins the reason TestMain is not counted:
// it is the harness that migrates the shared schema, not an assertion, so a
// package whose only db-gated function were TestMain still executes zero DB
// tests -- exactly the state the coverage gate must call empty.
func TestTestFuncNames_ExcludesTestMain(t *testing.T) {
	src := `package x
import (
	"testing"
	"github.com/znasllc-io/memql/component/database/dbtest"
)
func TestMain(m *testing.M) { _, _ = dbtest.EnsureSchema(nil) }
`
	if got := testFuncNames(parseSrc(t, src)); len(got) != 0 {
		t.Errorf("testFuncNames = %v, want none -- TestMain is a harness, not an assertion", got)
	}
}

// writeFixturePkg writes one db-gated test file into root/dir.
func writeFixturePkg(t *testing.T, root, dir, pkgName string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(dir))
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package " + pkgName + `

import (
	"testing"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

func TestSomething(t *testing.T) { _ = dbtest.DSN() }
`
	if err := os.WriteFile(filepath.Join(full, "x_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestScanDBGatedTests_SkipsBuildTaggedFiles pins that the scan honours build
// constraints. The db-tests lane passes no -tags, so a tagged file does not run
// there and must not be counted as coverage.
func TestScanDBGatedTests_SkipsBuildTaggedFiles(t *testing.T) {
	root := fixtureRoot(t)
	pkg := filepath.Join(root, "tagged")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	tagged := `//go:build clustere2e

package tagged

import (
	"testing"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

func TestTagged(t *testing.T) { _ = dbtest.DSN() }
`
	if err := os.WriteFile(filepath.Join(pkg, "tagged_db_test.go"), []byte(tagged), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scanDBGatedTests(t, root); len(got) != 0 {
		t.Errorf("scanDBGatedTests found %v in a build-tagged file; the lane passes no -tags so it never runs there", got)
	}
}

// TestScanDBGatedTests_FindsAnUntaggedDBGatedTest is the positive control for
// the test above: the same fixture without the constraint must be found, so a
// zero result there proves the tag was honoured rather than the scanner being
// inert.
func TestScanDBGatedTests_FindsAnUntaggedDBGatedTest(t *testing.T) {
	root := fixtureRoot(t)
	writeFixturePkg(t, root, "plain", "plain")

	got := scanDBGatedTests(t, root)
	if len(got) != 1 {
		t.Fatalf("scanDBGatedTests = %v, want exactly one entry", got)
	}
	if got[0].name != "TestSomething" || got[0].dir != "plain" {
		t.Errorf("got %+v, want {dir:plain name:TestSomething}", got[0])
	}
}

// TestSelfPkgNamesThisPackage pins selfPkg to the directory this file actually
// lives in.
//
// Without it, every assertion about the exclusion is written in terms of the
// constant and therefore MOVES WITH IT: widening selfPkg to "scripts" leaves a
// fixture-based test green while the gate silently stops excluding itself and
// starts excluding whatever sits directly under scripts/. A test that reads the
// value it is meant to be checking cannot catch that value changing -- which is
// the same shape as the bug this whole package exists to prevent.
func TestSelfPkgNamesThisPackage(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	rel, err := filepath.Rel(repoRoot(t), filepath.Dir(file))
	if err != nil {
		t.Fatalf("locating this package: %v", err)
	}
	if got := filepath.ToSlash(rel); got != selfPkg {
		t.Errorf("selfPkg = %q but this package lives at %q.\n\n"+
			"selfPkg must name EXACTLY this directory. Point it elsewhere and the gate counts "+
			"itself as a db-gated suite the lane fails to run; widen it and it hides sibling "+
			"packages from the coverage assertion (memql#2886).", selfPkg, got)
	}
}

// TestScanDBGatedTests_ExcludesOnlyTheGatesOwnPackage pins the exclusion as an
// EXACT directory match, using LITERAL paths rather than the selfPkg constant
// so the assertions cannot drift along with it (see TestSelfPkgNamesThisPackage).
func TestScanDBGatedTests_ExcludesOnlyTheGatesOwnPackage(t *testing.T) {
	root := fixtureRoot(t)
	writeFixturePkg(t, root, "scripts/cidb", "cidb")
	writeFixturePkg(t, root, "scripts/cidbx", "cidbx")
	writeFixturePkg(t, root, "scripts", "scripts")
	writeFixturePkg(t, root, "component/thing", "thing")

	got := map[string]bool{}
	for _, dbt := range scanDBGatedTests(t, root) {
		got[dbt.dir] = true
	}
	if got["scripts/cidb"] {
		t.Error("scripts/cidb was scanned; it imports dbtest for the predicate, not to reach a " +
			"database, so counting it reports the gate as a db-gated suite the lane fails to run")
	}
	if !got["scripts/cidbx"] {
		t.Error("scripts/cidbx was excluded; the exclusion must be an EXACT directory match, " +
			"not a prefix, or it hides packages it was never meant to")
	}
	if !got["scripts"] {
		t.Error("scripts/ itself was excluded; only the gate's own package may be")
	}
	if !got["component/thing"] {
		t.Error("an ordinary db-gated package was not scanned -- the scanner is inert")
	}
}

// TestRequireDBTruthiness pins dbtest.RequireDB's answer for the values a
// workflow author might plausibly write. The first gate calls RequireDB rather
// than comparing to "1" so the two cannot drift; this is what makes that
// delegation safe to rely on.
func TestRequireDBTruthiness(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"true", true},
		{"yes", true},
		{"0", false},
		{"false", false},
		{"no", false},
		{"", false},
		{"  ", false},
	}
	for _, tc := range cases {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(dbtest.RequireDBEnv, tc.value)
			if got := dbtest.RequireDB(); got != tc.want {
				t.Errorf("RequireDB() with %s=%q = %v, want %v", dbtest.RequireDBEnv, tc.value, got, tc.want)
			}
		})
	}
}

// TestProvisionedPkgs_FindsEnsureSchemaTestMains pins the structural tie: a
// package earns an EnsureSchema TestMain precisely so it can join this lane.
func TestProvisionedPkgs_FindsEnsureSchemaTestMains(t *testing.T) {
	root := fixtureRoot(t)

	withMain := `package p

import (
	"os"
	"testing"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

func TestMain(m *testing.M) {
	_, _ = dbtest.EnsureSchema(nil)
	os.Exit(m.Run())
}

func TestThing(t *testing.T) { _ = dbtest.DSN() }
`
	if err := os.MkdirAll(filepath.Join(root, "provisioned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "provisioned", "main_test.go"), []byte(withMain), 0o644); err != nil {
		t.Fatal(err)
	}
	// A db-gated package WITHOUT a TestMain is not provisioned for the lane.
	writeFixturePkg(t, root, "unprovisioned", "unprovisioned")

	got := provisionedPkgs(t, root)
	if len(got) != 1 || got[0] != "provisioned" {
		t.Errorf("provisionedPkgs = %v, want [provisioned] -- only packages with an "+
			"EnsureSchema TestMain are set up to share the lane's database", got)
	}
}

// TestImportsDBTest_SuffixIsNotEnough pins the full-path match. A suffix test
// passes the `dbtesthelper` fixture too, so only an exact comparison
// distinguishes a genuinely different package that merely ends in "dbtest".
func TestImportsDBTest_SuffixIsNotEnough(t *testing.T) {
	src := `package x
import "github.com/someone/else/internal/dbtest"
func TestA() {}
`
	if importsDBTest(parseSrc(t, src)) {
		t.Error("a DIFFERENT package whose path ends in \"dbtest\" must not count; " +
			"the match is on the full import path")
	}
}

// TestScanDBGatedTests_SkipsToolchainIgnoredDirs pins the walk exclusions.
// testdata/ and _-prefixed directories are not compiled by the Go toolchain, so
// a db-gated file inside one runs nowhere and must not count as coverage.
func TestScanDBGatedTests_SkipsToolchainIgnoredDirs(t *testing.T) {
	root := fixtureRoot(t)
	for _, dir := range []string{"pkg/testdata", "pkg/_ignored", "pkg/.hidden"} {
		writeFixturePkg(t, root, dir, "ignored")
	}
	writeFixturePkg(t, root, "pkg/real", "real")

	var dirs []string
	for _, dbt := range scanDBGatedTests(t, root) {
		dirs = append(dirs, dbt.dir)
	}
	if len(dirs) != 1 || dirs[0] != "pkg/real" {
		t.Errorf("scanDBGatedTests = %v, want only [pkg/real] -- testdata/, _-prefixed and "+
			"dot-prefixed directories are invisible to the Go toolchain", dirs)
	}
}

// TestGoflagsDisablesTests pins the environment route. GOFLAGS never appears on
// the command line, so no command-line check would ever see it.
func TestGoflagsDisablesTests(t *testing.T) {
	for _, v := range []any{"-count=0", "-run=NONE", "-mod=mod -skip=.*"} {
		if goflagsDisablesTests(v) == "" {
			t.Errorf("goflagsDisablesTests(%q) found nothing; that GOFLAGS disables the suite", v)
		}
	}
	for _, v := range []any{"-mod=mod", "", "-count=1"} {
		if f := goflagsDisablesTests(v); f != "" {
			t.Errorf("goflagsDisablesTests(%q) flagged %q; that value is harmless", v, f)
		}
	}
}

// TestDeclaresEnsureSchemaTestMain_Negatives gives the invariant's matcher the
// negative coverage it lacked: every case below would otherwise let a mutation
// through (a method named TestMain, any Test* function, any X.EnsureSchema).
func TestDeclaresEnsureSchemaTestMain_Negatives(t *testing.T) {
	imports := "import (\n\t\"testing\"\n\t\"github.com/znasllc-io/memql/component/database/dbtest\"\n)\n"
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"plain TestMain", "package x\n" + imports + "func TestMain(m *testing.M) { dbtest.EnsureSchema(nil) }", true},
		{"a METHOD named TestMain", "package x\n" + imports + "type S struct{}\nfunc (S) TestMain(m *testing.M) { dbtest.EnsureSchema(nil) }", false},
		{"an ordinary test calling EnsureSchema", "package x\n" + imports + "func TestThing(t *testing.T) { dbtest.EnsureSchema(nil) }", false},
		{"a DIFFERENT package's EnsureSchema", "package x\n" + imports + "func TestMain(m *testing.M) { other.EnsureSchema(nil) }", false},
		{"TestMain without EnsureSchema", "package x\n" + imports + "func TestMain(m *testing.M) { m.Run() }", false},
		{"EnsureSchema in a string literal", "package x\n" + imports + "func TestMain(m *testing.M) { _ = \"dbtest.EnsureSchema\" }", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := declaresEnsureSchemaTestMain(parseSrc(t, tc.src)); got != tc.want {
				t.Errorf("declaresEnsureSchemaTestMain = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDeclaresEnsureSchemaTestMain_AliasedImport pins alias resolution: a
// package using `dbt "…/dbtest"` is just as provisioned for the lane, and
// hardcoding the identifier "dbtest" silently dropped it from the invariant.
func TestDeclaresEnsureSchemaTestMain_AliasedImport(t *testing.T) {
	src := `package x
import (
	"testing"
	dbt "github.com/znasllc-io/memql/component/database/dbtest"
)
func TestMain(m *testing.M) { dbt.EnsureSchema(nil) }
`
	if !declaresEnsureSchemaTestMain(parseSrc(t, src)) {
		t.Error("an aliased dbtest import was not recognised; the package is provisioned for the " +
			"lane just the same, and missing it drops a real package out of the invariant")
	}
}

// TestWholeModuleWildcard pins the vacuity check: `./...` covers every package,
// so every coverage assertion passes regardless of what the lane runs.
func TestWholeModuleWildcard(t *testing.T) {
	for _, pkgs := range [][]string{{"./..."}, {"./component/memql/...", "./..."}, {"..."}, {"./"}} {
		_, found := wholeModuleWildcard(pkgs)
		want := pkgs[len(pkgs)-1] == "./..." || pkgs[len(pkgs)-1] == "..."
		if found != want {
			t.Errorf("wholeModuleWildcard(%v) = %v, want %v", pkgs, found, want)
		}
	}
}

// TestCoverageFindings covers each way the selector can fail to tie the job to
// the packages that carry db-gated tests. Inline, these ran only against the
// real tree, so deleting one changed nothing observable.
func TestCoverageFindings(t *testing.T) {
	all := []dbGatedTest{
		{dir: "component/memql", name: "TestA"},
		{dir: "examples/referencepack", name: "TestB"},
	}
	pkgs := []string{"./component/memql/...", "./examples/referencepack/..."}
	provisioned := []string{"component/memql", "examples/referencepack"}

	if got := coverageFindings(pkgs, all, provisioned); len(got) != 0 {
		t.Fatalf("a healthy selector produced findings: %v", got)
	}

	cases := []struct {
		name        string
		pkgs        []string
		provisioned []string
	}{
		{"empty selector", nil, provisioned},
		{"whole-module wildcard", []string{"./..."}, provisioned},
		{"covers nothing", []string{"./core/..."}, nil},
		{"an argument matching no db-gated test", append(append([]string{}, pkgs...), "./core/..."), provisioned},
		{"a provisioned package left out", []string{"./component/memql/..."}, provisioned},
		// memql#3095, the inverse direction: the selector still lists both
		// packages, but one lost its EnsureSchema TestMain. This is the case
		// that stayed silent while ALL of memql#3091's main_dbschema_test.go
		// files were deleted.
		{"a selector-covered db-gated package with no EnsureSchema TestMain",
			pkgs, []string{"component/memql"}},
		{"every EnsureSchema TestMain deleted", pkgs, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coverageFindings(tc.pkgs, all, tc.provisioned); len(got) == 0 {
				t.Error("no finding reported; the lane's selector does not tie it to the " +
					"packages that actually carry db-gated tests")
			}
		})
	}
}

// TestCoverageFindings_InSelectorImpliesProvisioned pins the memql#3095 rule
// precisely rather than just "some finding appeared": which package it names,
// that it carries the remedy, and -- the part a coarse assertion would miss --
// that it stays quiet for the two shapes it must NOT fire on.
func TestCoverageFindings_InSelectorImpliesProvisioned(t *testing.T) {
	all := []dbGatedTest{
		{dir: "component/memql", name: "TestA"},
		{dir: "examples/referencepack", name: "TestB"},
	}
	pkgs := []string{"./component/memql/...", "./examples/referencepack/..."}

	got := coverageFindings(pkgs, all, []string{"component/memql"})
	if len(got) != 1 {
		t.Fatalf("want exactly one finding for the one unprovisioned package, got %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], `"examples/referencepack"`) {
		t.Errorf("finding does not name the offending package: %s", got[0])
	}
	if strings.Contains(got[0], `"component/memql"`) &&
		!strings.Contains(got[0], "selector") {
		t.Errorf("finding names the healthy package as the subject: %s", got[0])
	}
	// A finding a reader cannot act on sends them to git blame. The remedy is
	// part of the assertion, not decoration.
	for _, want := range []string{"main_dbschema_test.go", "dbtest.EnsureSchema", "memql#2551"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("finding omits %q, so it names a problem without a remedy: %s", want, got[0])
		}
	}

	// Must NOT fire: a db-gated package OUTSIDE the selector. That is not this
	// rule's business -- it is caught, if at all, by the uncovered-set
	// assertion in dbgate_test.go, and reporting it here would demand a
	// migration TestMain from a package this lane never runs.
	outside := append(append([]dbGatedTest{}, all...), dbGatedTest{dir: "test/conformance", name: "TestC"})
	for _, f := range coverageFindings(pkgs, outside, []string{"component/memql", "examples/referencepack"}) {
		if strings.Contains(f, "no TestMain calling dbtest.EnsureSchema") {
			t.Errorf("fired for a package the selector does not cover: %s", f)
		}
	}

	// Must NOT fire: selfPkg. scripts/cidb imports dbtest for RequireDB and the
	// env-var name, never to reach a database. scanDBGatedTests drops it
	// upstream, but this function is pure and gets synthetic input, so the
	// exemption has to hold here too (dbgate_test.go's selfPkg).
	self := []dbGatedTest{{dir: selfPkg, name: "TestD"}}
	if f := coverageFindings([]string{"./" + selfPkg + "/..."}, self, nil); len(f) != 0 {
		t.Errorf("demanded a migration TestMain from the gate's own package %q: %v", selfPkg, f)
	}
}

// TestNamesSharedTestDB pins the narrow rule. The negatives are not invented:
// every one is a real postgres literal in this tree that the gate must leave
// alone, because none of them can drift with the shared database.
func TestNamesSharedTestDB(t *testing.T) {
	positives := []string{
		"postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable",
		"postgres://memql:memql_dev@127.0.0.1:1/memql?sslmode=disable",
		"postgres://memql@localhost:5432/memql",
		"postgresql://memql:memql_dev@localhost:5432/memql",
		// Named by database even under another user -- a copy is a copy.
		"postgres://postgres:pw@localhost:5432/memql",
	}
	for _, dsn := range positives {
		if !namesSharedTestDB(dsn) {
			t.Errorf("%q names the shared test database but was not flagged -- a copy of it "+
				"can drift and nothing would notice (memql#3149)", dsn)
		}
	}

	negatives := []string{
		"postgres://user:pass@localhost:5432/db?sslmode=disable",    // component/database/close_pool_test.go
		"postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable", // integrations/cognition, a deliberate dead probe
		"postgres://localhost:5432/main?sslmode=disable",            // component/database/direct_dsn_test.go
		"postgres://host/db",                           // ditto
		"postgres://main",                              // ditto
		"postgres://direct",                            // ditto
		"postgres://tiger-cloud-prod",                  // a DSN-shaped literal in a test fixture
		"postgres://local-dev",                         // ditto
		"postgres://x",                                 // component/envregistry/bootvalidate_test.go
		"postgres://legacy",                            // component/envregistry/legacyalias_test.go
		"postgres://u:sup3rs3cret@h:5432/d",            // a SafeDSN redaction fixture
		"mysql://memql:memql_dev@localhost:3306/memql", // not postgres at all
		"", // not a URI
	}
	for _, dsn := range negatives {
		if namesSharedTestDB(dsn) {
			t.Errorf("%q was flagged as a copy of the shared test DSN. It is a fixture or a "+
				"deliberate fake; redding on those makes the gate noise and gets it deleted", dsn)
		}
	}
}

// TestSharedDSNsIn_FindsOneEmbeddedInADocument is why the scan looks for
// OCCURRENCES rather than parsing each literal whole: this package's own ci.yml
// fixture carries the DSN as one line of a multi-line YAML string.
func TestSharedDSNsIn_FindsOneEmbeddedInADocument(t *testing.T) {
	doc := "jobs:\n  db-tests:\n    env:\n      MEMQL_DATABASE_DSN: " +
		"postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable\n      MEMQL_REQUIRE_DB: '1'\n"
	got := sharedDSNsIn(doc)
	if len(got) != 1 {
		t.Fatalf("want 1 shared DSN in the YAML document, got %d: %v", len(got), got)
	}
	if got[0] != "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable" {
		t.Errorf("candidate not cut at the line end: %q", got[0])
	}

	// A document with only fakes must yield nothing.
	if got := sharedDSNsIn("dsn: postgres://nobody:nobody@127.0.0.1:1/none\nalt: postgres://x\n"); len(got) != 0 {
		t.Errorf("flagged fakes inside a document: %v", got)
	}
	// And a bare literal is still found -- the common case.
	if got := sharedDSNsIn("postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"); len(got) != 1 {
		t.Errorf("missed a bare shared DSN: %v", got)
	}
}

// TestDSNLiteralFindings covers the three dispositions the gate can reach, and
// -- the part that matters -- proves the exemptions are load-bearing rather
// than decorative: remove one and the finding appears.
func TestDSNLiteralFindings(t *testing.T) {
	const shared = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"

	inResolver := []dsnLiteral{{file: dsnResolverDir + "/dbtest_hygiene_test.go", line: 15, dsn: shared}}
	if got := dsnLiteralFindings(inResolver, nil); len(got) != 0 {
		t.Errorf("flagged a literal inside %s, which is where the resolution lives: %v", dsnResolverDir, got)
	}

	elsewhere := []dsnLiteral{{file: "component/memql/some_db_test.go", line: 47, dsn: shared}}
	got := dsnLiteralFindings(elsewhere, nil)
	if len(got) != 1 {
		t.Fatalf("want exactly one finding for the fifteenth copy, got %d: %v", len(got), got)
	}
	for _, want := range []string{"component/memql/some_db_test.go:47", "dbtest.DSN()", "memql#3149", "REMEDY"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("finding omits %q, so it names a problem without a remedy: %s", want, got[0])
		}
	}

	exempt := map[string]string{"component/memql/some_db_test.go": "a reason"}
	if got := dsnLiteralFindings(elsewhere, exempt); len(got) != 0 {
		t.Errorf("an exempt file was still flagged: %v", got)
	}
}
