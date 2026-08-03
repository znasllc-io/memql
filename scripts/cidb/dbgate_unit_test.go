package cidb

// dbgate_unit_test.go -- unit tests for the gate's own machinery (memql#2886).
//
// The gates in dbgate_test.go read the real ci.yml and the real tree, so they
// pass both when the invariant holds and when the scanner that checks it is
// broken. These tests remove that second possibility: every helper is exercised
// on synthetic input whose answer is known, including the cases that would make
// a broken gate report success -- a renamed job, a step whose `name:` mentions
// `go test`, a commented-out or heredoc'd invocation, a step-level env
// override, a -run selector, a file that merely names dbtest in a comment, and
// a build-tagged file.
//
// Each case below is chosen to red under a specific plausible MUTATION of the
// helper it covers, not merely to execute it.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func mustParse(t *testing.T, y string) laneSpec {
	t.Helper()
	spec, err := parseDBTestsJob([]byte(y))
	if err != nil {
		t.Fatalf("parseDBTestsJob: %v", err)
	}
	return spec
}

func TestParseDBTestsJob_ReadsEnvAndPackagesFromTheRightJob(t *testing.T) {
	spec := mustParse(t, laneYAML)

	if got := spec.jobEnv["MEMQL_REQUIRE_DB"]; got != "1" {
		t.Errorf("MEMQL_REQUIRE_DB = %#v, want \"1\"", got)
	}
	pkgs := spec.pkgs()
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

// TestParseDBTestsJob_OnlyDotSlashArgsArePackages pins the field-selection
// predicate. Loosening it to "contains a slash" -- an easy simplification --
// swallows flags carrying paths, and each one silently becomes a package
// argument that coversOne() then reports as matching nothing.
func TestParseDBTestsJob_OnlyDotSlashArgsArePackages(t *testing.T) {
	spec := mustParse(t, "jobs:\n  db-tests:\n    steps:\n"+
		"      - run: go test -count=1 -coverprofile=/tmp/c.out -timeout=300s ./component/memql/...\n")
	if len(spec.steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(spec.steps))
	}
	got := spec.pkgs()
	if len(got) != 1 || got[0] != "./component/memql/..." {
		t.Errorf("pkgs = %v, want [./component/memql/...] -- only ./-prefixed args are packages", got)
	}
	for _, f := range spec.steps[0].flags {
		if strings.HasPrefix(f, "./") {
			t.Errorf("flag %q looks like a package argument", f)
		}
	}
	if len(spec.steps[0].flags) != 3 {
		t.Errorf("flags = %v, want the three non-package args", spec.steps[0].flags)
	}
}

func TestParseDBTestsJob_UnquotedScalarIsStillFound(t *testing.T) {
	// Actions accepts `MEMQL_REQUIRE_DB: 1`. Unmarshalled into map[string]string
	// that fails and reads as "absent", which is the false negative the gate
	// exists to prevent.
	spec := mustParse(t, "jobs:\n  db-tests:\n    env:\n      MEMQL_REQUIRE_DB: 1\n    steps:\n      - run: go test ./x/\n")
	if _, ok := spec.jobEnv["MEMQL_REQUIRE_DB"]; !ok {
		t.Fatal("unquoted `MEMQL_REQUIRE_DB: 1` was not found; the gate would report the key absent")
	}
}

func TestParseDBTestsJob_MissingJobIsAnError(t *testing.T) {
	if _, err := parseDBTestsJob([]byte("jobs:\n  go-checks:\n    steps:\n      - run: go test ./...\n")); err == nil {
		t.Fatal("a workflow with no db-tests job must be an error, not a silent pass")
	}
}

func TestParseDBTestsJob_StepNameIsNotAStep(t *testing.T) {
	// scripts/citags records this exact defect: a gate satisfied by a LABEL.
	// Deleting every `run:` line must leave the gate with zero packages.
	spec := mustParse(t, "jobs:\n  db-tests:\n    steps:\n      - name: go test -count=1 ./component/memql/...\n        uses: actions/checkout@v7\n")
	if got := spec.pkgs(); len(got) != 0 {
		t.Errorf("pkgs = %v from a step `name:`; only steps[].run may satisfy this gate", got)
	}
}

// TestGoTestArgs_IgnoresCommandsThatDoNotRun is the same defect one layer down
// in the shell: the gate must not be satisfied by text that never executes.
// A `#`-commented invocation is the likeliest, left behind by someone debugging
// the lane.
func TestGoTestArgs_IgnoresCommandsThatDoNotRun(t *testing.T) {
	cases := []struct {
		name string
		run  string
	}{
		{"shell comment", "# go test -count=1 ./component/memql/..."},
		{"indented shell comment", "  #   go test ./component/memql/..."},
		{"echoed", "echo go test ./component/memql/..."},
		{"short-circuited", "false && go test ./component/memql/..."},
		{"heredoc body, quoted", "cat <<'EOF'\ngo test ./component/memql/...\nEOF"},
		{"heredoc body, bare", "cat <<EOF\ngo test ./component/memql/...\nEOF"},
		{"heredoc body, dash form", "cat <<-EOF\n\tgo test ./component/memql/...\n\tEOF"},
		// The exact shape the lane's own extensions step uses.
		{"heredoc body, psql SQL", "psql -v ON_ERROR_STOP=1 <<'SQL'\ngo test ./component/memql/...\nSQL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := goTestArgs(tc.run); len(got) != 0 {
				t.Errorf("goTestArgs(%q) = %v, want none -- that command does not run", tc.run, got)
			}
		})
	}

	// Positive controls, without which every case above could pass vacuously.
	for _, run := range []string{
		"go test -count=1 ./component/memql/...",
		"set -o pipefail\ngo test -count=1 ./component/memql/...",
		// A real invocation AFTER a closed heredoc must still be found --
		// otherwise heredoc handling could swallow the rest of the block.
		"psql <<'SQL'\nSELECT 1;\nSQL\ngo test -count=1 ./component/memql/...",
	} {
		if got := goTestArgs(run); len(got) != 1 {
			t.Errorf("goTestArgs(%q) = %v, want exactly one invocation", run, got)
		}
	}
}

// TestGoTestArgs_CommentedHeredocDoesNotSwallowTheRealCommand is what makes the
// comment-skip load-bearing rather than merely redundant with the `^` anchor.
//
// The anchor alone already rejects `# go test ...`. But a COMMENTED-OUT heredoc
// opener still matches heredocStart, so without the comment-skip it would arm
// the heredoc state and consume every following line -- including the real
// invocation -- leaving the lane looking like it runs no packages at all.
func TestGoTestArgs_CommentedHeredocDoesNotSwallowTheRealCommand(t *testing.T) {
	run := "# psql <<'SQL'\ngo test -count=1 ./component/memql/..."
	got := goTestArgs(run)
	if len(got) != 1 {
		t.Fatalf("goTestArgs(%q) = %v, want exactly one invocation -- a commented-out heredoc "+
			"opener must not arm heredoc tracking and eat the command below it", run, got)
	}
	if !strings.Contains(got[0], "./component/memql/...") {
		t.Errorf("goTestArgs returned %q, want the real invocation's arguments", got[0])
	}
}

// --- the lane cannot pass without running ------------------------------------

func TestEffectiveEnv_StepOverridesJob(t *testing.T) {
	spec := mustParse(t, "jobs:\n  db-tests:\n    env:\n      MEMQL_REQUIRE_DB: '1'\n    steps:\n"+
		"      - name: db-gated suites\n        env:\n          MEMQL_REQUIRE_DB: '0'\n        run: go test ./component/memql/...\n")
	if len(spec.steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(spec.steps))
	}
	got, present := spec.effectiveEnv(spec.steps[0], "MEMQL_REQUIRE_DB")
	if !present {
		t.Fatal("MEMQL_REQUIRE_DB resolved as absent")
	}
	if fmt.Sprintf("%v", got) != "0" {
		t.Errorf("effective MEMQL_REQUIRE_DB = %v, want 0 -- Actions gives step env precedence, "+
			"and reading only the job level lets a step silently revert the lane to green-by-skip", got)
	}
	if truthy(t, got) {
		t.Error("a step-level '0' must resolve falsy")
	}
}

func TestEffectiveEnv_FallsBackToJob(t *testing.T) {
	spec := mustParse(t, "jobs:\n  db-tests:\n    env:\n      MEMQL_REQUIRE_DB: '1'\n    steps:\n"+
		"      - run: go test ./component/memql/...\n")
	got, present := spec.effectiveEnv(spec.steps[0], "MEMQL_REQUIRE_DB")
	if !present || fmt.Sprintf("%v", got) != "1" {
		t.Errorf("effective MEMQL_REQUIRE_DB = %v (present=%v), want 1 from the job env", got, present)
	}
}

func TestParseDBTestsJob_CapturesStepGuardsAndJobContinueOnError(t *testing.T) {
	spec := mustParse(t, "jobs:\n  db-tests:\n    continue-on-error: true\n    steps:\n"+
		"      - if: ${{ false }}\n        continue-on-error: true\n        run: go test -run 'TestNope' ./component/memql/...\n")
	if isFalsy(spec.continueOnError) {
		t.Error("job continue-on-error: true was not captured; the lane could fail without failing the build")
	}
	if len(spec.steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(spec.steps))
	}
	s := spec.steps[0]
	if strings.TrimSpace(s.ifCond) == "" {
		t.Error("step `if:` was not captured; a step condition can skip the suite while the job succeeds")
	}
	if isFalsy(s.continueOnError) {
		t.Error("step continue-on-error: true was not captured")
	}
	if !s.hasRunFilter() {
		t.Error("a -run selector was not detected; -run matching nothing executes zero tests and reports ok")
	}
}

func TestHasRunFilter(t *testing.T) {
	cases := []struct {
		flags []string
		want  bool
	}{
		{[]string{"-run", "TestX"}, true},
		{[]string{"-run=TestX"}, true},
		{[]string{"--run=TestX"}, true},
		{[]string{"-count=1", "-timeout=300s"}, false},
		{[]string{"-runtime"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := (goTestStep{flags: tc.flags}).hasRunFilter(); got != tc.want {
			t.Errorf("hasRunFilter(%v) = %v, want %v", tc.flags, got, tc.want)
		}
	}
}

func TestIsFalsy(t *testing.T) {
	cases := []struct {
		v    any
		want bool
	}{
		{nil, true},
		{false, true},
		{"", true},
		{"false", true},
		{" False ", true},
		{true, false},
		{"true", false},
		{"${{ github.event_name == 'push' }}", false},
	}
	for _, tc := range cases {
		if got := isFalsy(tc.v); got != tc.want {
			t.Errorf("isFalsy(%#v) = %v, want %v", tc.v, got, tc.want)
		}
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

// --- scanDBGatedTests --------------------------------------------------------

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
