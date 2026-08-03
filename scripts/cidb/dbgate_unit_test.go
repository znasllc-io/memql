package cidb

// dbgate_unit_test.go -- unit tests for the gate's own machinery (memql#2886).
//
// The gates in dbgate_test.go read the real ci.yml and the real tree, so they
// pass both when the invariant holds and when the scanner that checks it is
// broken. These tests remove that second possibility: every helper is exercised
// on synthetic input whose answer is known, including the cases that would make
// a broken gate report success -- a renamed job, a step whose `name:` mentions
// `go test`, a file that merely names dbtest in a comment, a build-tagged file.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// --- parseDBTestsJob ---------------------------------------------------------

const laneYAML = `
jobs:
  go-checks:
    env:
      RUN_GO: 'true'
    steps:
      - name: go test
        run: go test -count=1 ./...
  db-tests:
    env:
      MEMQL_DATABASE_DSN: postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable
      MEMQL_REQUIRE_DB: '1'
    steps:
      - uses: actions/checkout@v7
      - name: create required Postgres extensions
        run: psql -c 'CREATE EXTENSION vector;'
      - name: db-gated suites (seeded test DB)
        run: go test -count=1 -timeout=300s ./component/memql/... ./examples/referencepack/...
`

func TestParseDBTestsJob_ReadsEnvAndPackagesFromTheRightJob(t *testing.T) {
	env, pkgs, err := parseDBTestsJob([]byte(laneYAML))
	if err != nil {
		t.Fatalf("parseDBTestsJob: %v", err)
	}
	if got := env["MEMQL_REQUIRE_DB"]; got != "1" {
		t.Errorf("MEMQL_REQUIRE_DB = %#v, want \"1\"", got)
	}
	want := []string{"./component/memql/...", "./examples/referencepack/..."}
	if len(pkgs) != len(want) {
		t.Fatalf("pkgs = %v, want %v", pkgs, want)
	}
	for i := range want {
		if pkgs[i] != want[i] {
			t.Errorf("pkgs[%d] = %q, want %q", i, pkgs[i], want[i])
		}
	}
	// The go-checks `./...` must NOT leak in: it would make covers() return
	// true for everything and the coverage gate vacuous.
	for _, p := range pkgs {
		if p == "./..." {
			t.Error("picked up ./... from a different job -- the gate would then be satisfied by any package")
		}
	}
}

func TestParseDBTestsJob_UnquotedScalarIsStillFound(t *testing.T) {
	// Actions accepts `MEMQL_REQUIRE_DB: 1`. Unmarshalled into map[string]string
	// that fails and reads as "absent", which is the false negative the gate
	// exists to prevent.
	env, _, err := parseDBTestsJob([]byte("jobs:\n  db-tests:\n    env:\n      MEMQL_REQUIRE_DB: 1\n    steps:\n      - run: go test ./x/\n"))
	if err != nil {
		t.Fatalf("parseDBTestsJob: %v", err)
	}
	if _, ok := env["MEMQL_REQUIRE_DB"]; !ok {
		t.Fatal("unquoted `MEMQL_REQUIRE_DB: 1` was not found; the gate would report the key absent")
	}
}

func TestParseDBTestsJob_MissingJobIsAnError(t *testing.T) {
	if _, _, err := parseDBTestsJob([]byte("jobs:\n  go-checks:\n    steps:\n      - run: go test ./...\n")); err == nil {
		t.Fatal("a workflow with no db-tests job must be an error, not a silent pass")
	}
}

func TestParseDBTestsJob_StepNameIsNotAStep(t *testing.T) {
	// scripts/citags records this exact defect: a gate satisfied by a LABEL.
	// Deleting every `run:` line must leave the gate with zero packages.
	_, pkgs, err := parseDBTestsJob([]byte("jobs:\n  db-tests:\n    steps:\n      - name: go test -count=1 ./component/memql/...\n        uses: actions/checkout@v7\n"))
	if err != nil {
		t.Fatalf("parseDBTestsJob: %v", err)
	}
	if len(pkgs) != 0 {
		t.Errorf("pkgs = %v from a step `name:`; only steps[].run may satisfy this gate", pkgs)
	}
}

// --- covers ------------------------------------------------------------------

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
// jmendivilznas audit found: `./component/automations/steps/...` does NOT reach
// component/automations, so the parent's db-gated tests never run.
func TestCovers_ParentIsNotCoveredByAChildSelector(t *testing.T) {
	if covers([]string{"./component/automations/steps/..."}, "component/automations") {
		t.Fatal("a child selector must not be read as covering its parent package; " +
			"component/automations' db-gated tests genuinely do not run in the lane")
	}
}

// --- importsDBTest / testFuncNames -------------------------------------------

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

	// A different package with a confusable suffix must not match.
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

// --- scanDBGatedTests --------------------------------------------------------

// TestScanDBGatedTests_SkipsBuildTaggedFiles pins that the scan honours build
// constraints. The db-tests lane passes no -tags, so a tagged file does not run
// there and must not be counted as coverage.
func TestScanDBGatedTests_SkipsBuildTaggedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
// the test above: same fixture without the constraint must be found, so a zero
// result there proves the tag was honoured rather than the scanner being inert.
func TestScanDBGatedTests_FindsAnUntaggedDBGatedTest(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "plain")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	src := `package plain

import (
	"testing"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

func TestPlain(t *testing.T) { _ = dbtest.DSN() }
`
	if err := os.WriteFile(filepath.Join(pkg, "plain_db_test.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := scanDBGatedTests(t, root)
	if len(got) != 1 {
		t.Fatalf("scanDBGatedTests = %v, want exactly one entry", got)
	}
	if got[0].name != "TestPlain" || got[0].dir != "plain" {
		t.Errorf("got %+v, want {dir:plain name:TestPlain}", got[0])
	}
}

// --- the truthiness contract this gate leans on ------------------------------

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
