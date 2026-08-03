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
	if s.zeroExecutionFlag() == "" {
		t.Error("a -run selector was not detected; -run matching nothing executes zero tests and reports ok")
	}
}

// TestZeroExecutionFlag covers every flag that makes `go test` exit 0 having
// run nothing. Guarding only -run read as though the category were closed; the
// lane already carries -count=1, so -count=0 is a one-character bypass.
func TestZeroExecutionFlag(t *testing.T) {
	cases := []struct {
		flags []string
		want  bool
	}{
		{[]string{"-run", "TestX"}, true},
		{[]string{"-run=TestX"}, true},
		{[]string{"--run=TestX"}, true},
		{[]string{"-test.run=TestX"}, true},
		{[]string{"-skip=.*"}, true},
		{[]string{"-test.skip=.*"}, true},
		{[]string{"-count=0"}, true},
		{[]string{"-test.count=0"}, true},
		{[]string{"-count=1", "-timeout=300s"}, false},
		{[]string{"-runtime"}, false},
		{[]string{"-skipper"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		got := (goTestStep{flags: tc.flags}).zeroExecutionFlag() != ""
		if got != tc.want {
			t.Errorf("zeroExecutionFlag(%v) = %v, want %v", tc.flags, got, tc.want)
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

// --- runBlockIsPlain: the fail-closed half ----------------------------------

// TestRunBlockIsPlain_RefusesShellConstructs is what makes the line-oriented
// scan trustworthy. `go test` as the first word of a line inside a conditional
// never executes, and no regex over lines can tell -- so the gate refuses the
// block instead of guessing. Each case below was CONFIRMED to leave the gate
// green before this existed.
func TestRunBlockIsPlain_RefusesShellConstructs(t *testing.T) {
	cases := []struct{ name, run string }{
		{"if/then/fi", "if [ \"$X\" = 1 ]; then\ngo test ./component/memql/...\nfi"},
		{"while false", "while false; do\ngo test ./component/memql/...\ndone"},
		{"for loop", "for x in a b; do\ngo test ./component/memql/...\ndone"},
		{"case arm", "case $X in\na)\ngo test ./component/memql/...\n;;\nesac"},
		{"exit above", "exit 0\ngo test ./component/memql/..."},
		{"function body", "run_it() {\ngo test ./component/memql/...\n}"},
		{"function keyword", "function run_it {\ngo test ./component/memql/...\n}"},
		{"eval", "eval go test ./component/memql/..."},
		{"cd changes what ./... means", "cd examples/referencepack\ngo test ./..."},
		{"exit-status suppression: || true", "go test ./component/memql/... || true"},
		{"exit-status suppression: backgrounded", "go test ./component/memql/... &\nwait"},
		{"exit-status suppression: piped", "go test ./component/memql/... | tee out.log"},
		{"trailing || continuation", "true ||\ngo test ./component/memql/..."},
		{"trailing && continuation", "false &&\ngo test ./component/memql/..."},
		{"no-space function shadowing go", "go(){ echo skip; }\ngo test ./component/memql/..."},
		{"multi-line subshell", "X=$(\ngo test ./component/memql/...\n)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := runBlockIsPlain(tc.run); ok {
				t.Errorf("runBlockIsPlain accepted a block containing a shell construct:\n%s\n\n"+
					"`go test` there may never execute, and the gate would report the lane's "+
					"packages as covered while it runs nothing", tc.run)
			}
		})
	}

	// Positive controls: the real lane's shapes must remain acceptable, or the
	// fail-closed check would red the lane it is meant to protect.
	for _, run := range []string{
		"go test -count=1 -timeout=300s ./component/memql/... ./examples/referencepack/...",
		"set -o pipefail\ngo test -count=1 ./component/memql/...",
		"# a comment\ngo test -count=1 ./component/memql/...",
		"psql -v ON_ERROR_STOP=1 <<'SQL'\nCREATE EXTENSION vector;\nSQL",
	} {
		if line, ok := runBlockIsPlain(run); !ok {
			t.Errorf("runBlockIsPlain rejected a legitimate block at %q:\n%s", line, run)
		}
	}
}

// TestHeredocDelimiter pins the hand-parse. A regex that merely looks for `<<`
// fires on `echo "a << b"`, captures `b`, and swallows every following line --
// silently hiding the real invocation below it.
func TestHeredocDelimiter(t *testing.T) {
	cases := []struct{ line, want string }{
		{"cat <<'SQL'", "SQL"},
		{"cat <<\"SQL\"", "SQL"},
		{"cat <<SQL", "SQL"},
		{"cat <<-SQL", "SQL"},
		{`cat <<\SQL`, "SQL"},
		{"psql -v ON_ERROR_STOP=1 <<'SQL'", "SQL"},
		{"cat <<'1SQL'", "1SQL"},          // digit-leading is valid bash
		{`echo "redirect is a << b"`, ""}, // not a heredoc
		{"echo a << b c", ""},             // trailing text
		{"go test ./x/...", ""},
	}
	for _, tc := range cases {
		if got := heredocDelimiter(tc.line); got != tc.want {
			t.Errorf("heredocDelimiter(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// TestParseDBTestsJob_ValueTakingFlagIsNotAPackage pins F5: a space-separated
// flag value starting with "./" is the flag's value, not a package argument.
func TestParseDBTestsJob_ValueTakingFlagIsNotAPackage(t *testing.T) {
	spec := mustParse(t, "jobs:\n  db-tests:\n    steps:\n"+
		"      - run: go test -coverprofile ./cover.out ./component/memql/...\n")
	got := spec.pkgs()
	if len(got) != 1 || got[0] != "./component/memql/..." {
		t.Errorf("pkgs = %v, want [./component/memql/...] -- ./cover.out is -coverprofile's value", got)
	}
}

// TestParseDBTestsJob_NonPlainRunIsRecordedNotScanned pins that a refused block
// contributes no packages AND is reported, rather than silently vanishing.
func TestParseDBTestsJob_NonPlainRunIsRecordedNotScanned(t *testing.T) {
	spec := mustParse(t, "jobs:\n  db-tests:\n    steps:\n"+
		"      - run: |\n          if [ \"$X\" = 1 ]; then\n          go test ./component/memql/...\n          fi\n")
	if len(spec.pkgs()) != 0 {
		t.Errorf("pkgs = %v from a conditional block; it must contribute nothing", spec.pkgs())
	}
	if len(spec.nonPlainRun) != 1 {
		t.Fatalf("nonPlainRun = %v, want exactly one recorded offending line", spec.nonPlainRun)
	}
}

// TestParseDBTestsJob_NonGoTestStepsAreExcluded pins that steps without a `go
// test` never enter spec.steps -- the per-step if:/continue-on-error/flag
// assertions are scoped by that, so including them would red the lane for the
// checkout and extension steps.
func TestParseDBTestsJob_NonGoTestStepsAreExcluded(t *testing.T) {
	spec := mustParse(t, laneYAML)
	if len(spec.steps) != 1 {
		t.Errorf("steps = %d, want 1 -- only the `go test` step, not checkout or psql", len(spec.steps))
	}
}

// TestLaneSpecPkgs_SpansEveryStep pins that a second `go test` step is not
// dropped; taking only the first step's packages would hide a whole suite.
func TestLaneSpecPkgs_SpansEveryStep(t *testing.T) {
	spec := mustParse(t, "jobs:\n  db-tests:\n    steps:\n"+
		"      - run: go test ./component/memql/...\n"+
		"      - run: go test ./examples/referencepack/...\n")
	if got := spec.pkgs(); len(got) != 2 {
		t.Errorf("pkgs = %v, want both steps' packages", got)
	}
}

// TestParseDBTestsJob_JobLevelIfIsRead pins F3: the job's `if:` decides whether
// the lane runs at all, and ci-required treats a skipped job as a pass.
func TestParseDBTestsJob_JobLevelIfIsRead(t *testing.T) {
	spec := mustParse(t, "jobs:\n  db-tests:\n    if: ${{ false }}\n    steps:\n      - run: go test ./x/...\n")
	if strings.TrimSpace(spec.jobIf) == "" {
		t.Error("the job-level `if:` was not captured; a constant-false condition would disable " +
			"the whole lane with no visible failure")
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

// TestParseDBTestsJob_UnparsedRunIsDistinguished pins the diagnosis quality.
// `set -e; go test …` and `GOFLAGS=… go test …` are legitimate edits the
// scanner deliberately does not parse around; reporting them as "no go test
// step" sends a maintainer looking for a step that is plainly right there.
func TestParseDBTestsJob_UnparsedRunIsDistinguished(t *testing.T) {
	for _, run := range []string{
		"set -e; go test -count=1 ./component/memql/...",
		"GOFLAGS=-mod=mod go test -count=1 ./component/memql/...",
	} {
		spec := mustParse(t, "jobs:\n  db-tests:\n    steps:\n      - run: "+run+"\n")
		if len(spec.steps) != 0 {
			t.Errorf("run %q yielded a parsed step; the scanner cannot read its packages", run)
		}
		if len(spec.unparsedRun) != 1 {
			t.Errorf("run %q: unparsedRun = %v, want it recorded so the gate can say WHY",
				run, spec.unparsedRun)
		}
		if len(spec.nonPlainRun) != 0 {
			t.Errorf("run %q was recorded as non-plain; it has no shell construct", run)
		}
	}
}

// --- remaining mutation survivors from the second review ---------------------

// TestIsFalsy_UnknownTypeIsNotFalsy exercises the default arm, which no
// nil/bool/string case reaches. A YAML value of an unexpected type must not be
// read as "continue-on-error is off".
func TestIsFalsy_UnknownTypeIsNotFalsy(t *testing.T) {
	for _, v := range []any{1, 0, []any{"x"}, map[string]any{"a": 1}} {
		if isFalsy(v) {
			t.Errorf("isFalsy(%#v) = true; an unrecognised value must not read as off", v)
		}
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

// TestGoTestCmd_WordBoundary pins the \b: `go testify` is not `go test`.
func TestGoTestCmd_WordBoundary(t *testing.T) {
	if got := goTestArgs("go testify ./component/memql/..."); len(got) != 0 {
		t.Errorf("goTestArgs counted `go testify` as `go test`: %v", got)
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

// TestJoinContinuations_FoldsWrappedInvocations pins the fix for the likeliest
// future edit to this lane: the selector grows and someone wraps the line.
// Joining first makes the wrapped form simply work, which beats any error
// message -- and it also defeats `echo hi \` + `go test …`, which folds into an
// `echo` line and is therefore correctly not counted rather than refused.
func TestJoinContinuations_FoldsWrappedInvocations(t *testing.T) {
	wrapped := "go test -count=1 -timeout=300s \\\n  ./component/memql/... \\\n  ./examples/referencepack/..."
	if line, ok := runBlockIsPlain(wrapped); !ok {
		t.Errorf("a wrapped invocation was refused at %q; wrapping is a legitimate edit", line)
	}
	got := goTestArgs(wrapped)
	if len(got) != 1 {
		t.Fatalf("goTestArgs = %v, want one joined invocation", got)
	}
	for _, want := range []string{"./component/memql/...", "./examples/referencepack/..."} {
		if !strings.Contains(got[0], want) {
			t.Errorf("joined invocation %q lost %q", got[0], want)
		}
	}

	// A continuation whose FIRST word is not `go test` must stay uncounted.
	if got := goTestArgs("echo hi \\\ngo test ./component/memql/..."); len(got) != 0 {
		t.Errorf("goTestArgs = %v; `echo hi \\` folds the next line into the echo command", got)
	}
}

// TestMentionsGoTest_IgnoresComments pins that the plainness probe is not keyed
// on prose. A comment saying "before go test runs" above the extensions step's
// legitimate `for` loop must not arm the refusal on a block that runs no tests.
func TestMentionsGoTest_IgnoresComments(t *testing.T) {
	if mentionsGoTest("# wait for postgres before go test runs\nfor i in $(seq 1 30); do\n  pg_isready\ndone") {
		t.Error("a COMMENT mentioning `go test` armed the probe; keying a gate on prose is the " +
			"exact defect this package criticises elsewhere")
	}
	if !mentionsGoTest("# a note\ngo test ./x/...") {
		t.Error("a real invocation below a comment was missed")
	}
}

// TestHeredocDelimiter_HereStringAndTrailingRedirect covers two shapes that
// each swallowed the rest of a block.
func TestHeredocDelimiter_HereStringAndTrailingRedirect(t *testing.T) {
	if got := heredocDelimiter("grep -q x <<<$out"); got != "" {
		t.Errorf("heredocDelimiter(here-string) = %q, want \"\" -- `<<<` has no terminator, so "+
			"treating it as a heredoc skips every line below it", got)
	}
	if got := heredocDelimiter("psql <<'SQL' >/dev/null"); got != "SQL" {
		t.Errorf("heredocDelimiter with a trailing redirect = %q, want SQL -- bash permits "+
			"redirections after the delimiter word", got)
	}
}

// TestZeroExecutionFlag_SpaceSeparatedCount pins the form the `=`-only regex
// missed. `-count 1` -> `-count 0` is the same one-character edit the comment
// warns about, and `go test -count 0 ./...` exits 0 with "[no tests to run]".
func TestZeroExecutionFlag_SpaceSeparatedCount(t *testing.T) {
	cases := []struct {
		flags []string
		want  bool
	}{
		{[]string{"-count", "0"}, true},
		{[]string{"-test.count", "0"}, true},
		{[]string{"-count", "1"}, false},
		{[]string{"-count"}, false},
	}
	for _, tc := range cases {
		got := (goTestStep{flags: tc.flags}).zeroExecutionFlag() != ""
		if got != tc.want {
			t.Errorf("zeroExecutionFlag(%v) = %v, want %v", tc.flags, got, tc.want)
		}
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

// TestParseDBTestsJob_WorkingDirectoryIsCaptured pins the YAML route to the
// same vacuity `cd` produces, with no shell involved at all.
func TestParseDBTestsJob_WorkingDirectoryIsCaptured(t *testing.T) {
	step := mustParse(t, "jobs:\n  db-tests:\n    steps:\n"+
		"      - working-directory: examples/referencepack\n        run: go test ./...\n")
	if step.workingDir != "examples/referencepack" {
		t.Errorf("step working-directory = %q, want it captured", step.workingDir)
	}
	job := mustParse(t, "jobs:\n  db-tests:\n    defaults:\n      run:\n        working-directory: examples/referencepack\n"+
		"    steps:\n      - run: go test ./...\n")
	if job.workingDir != "examples/referencepack" {
		t.Errorf("job defaults.run.working-directory = %q, want it captured", job.workingDir)
	}
}

// TestJobIfIsPathRouting pins the job-level `if:` predicate directly. The gate
// assertion that uses it had no unit test at all: weakening it, or disabling it
// outright, was caught by nothing.
func TestJobIfIsPathRouting(t *testing.T) {
	cases := []struct {
		name string
		cond string
		want bool
	}{
		{"absent", "", true},
		{"the real routing expression",
			"${{ github.event_name != 'pull_request' || needs.changes.outputs.ci == 'true' || needs.changes.outputs.go == 'true' }}", true},
		{"constant false", "${{ false }}", false},
		{"false ANDed with real routing", "${{ false && needs.changes.outputs.go == 'true' }}", false},
		{"no routing reference at all", "${{ github.event_name == 'push' }}", false},
		{"unrelated condition", "${{ success() }}", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobIfIsPathRouting(tc.cond); got != tc.want {
				t.Errorf("jobIfIsPathRouting(%q) = %v, want %v -- a job `if:` that can never be "+
					"true disables the lane, and ci-required reads the skip as a pass", tc.cond, got, tc.want)
			}
		})
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

// --- laneRunFindings: every check, table-tested ------------------------------

// TestLaneRunFindings covers each way the lane can report a non-failure while
// executing nothing. These previously lived inline in the gate test and were
// only ever evaluated against the real ci.yml, so deleting any one of them
// reded nothing.
func TestLaneRunFindings(t *testing.T) {
	okStep := goTestStep{pkgs: []string{"./component/memql/..."}, flags: []string{"-count=1"}}
	base := laneSpec{
		jobEnv: map[string]any{"MEMQL_REQUIRE_DB": "1"},
		jobIf:  "${{ needs.changes.outputs.go == 'true' }}",
		steps:  []goTestStep{okStep},
	}

	if got := laneRunFindings(base); len(got) != 0 {
		t.Fatalf("a healthy lane produced findings: %v", got)
	}

	cases := []struct {
		name   string
		mutate func(l *laneSpec)
	}{
		{"job if: constant false", func(l *laneSpec) { l.jobIf = "${{ false }}" }},
		{"job if: no routing reference", func(l *laneSpec) { l.jobIf = "${{ success() }}" }},
		{"job continue-on-error", func(l *laneSpec) { l.continueOnError = true }},
		{"working-directory set", func(l *laneSpec) { l.workingDir = "examples/referencepack" }},
		{"a refused run block", func(l *laneSpec) { l.nonPlainRun = []string{"if [ x ]; then"} }},
		{"step if:", func(l *laneSpec) { l.steps[0].ifCond = "${{ false }}" }},
		{"step continue-on-error", func(l *laneSpec) { l.steps[0].continueOnError = true }},
		{"zero-execution flag", func(l *laneSpec) { l.steps[0].flags = []string{"-count=0"} }},
		{"zero-execution flag, space form", func(l *laneSpec) { l.steps[0].flags = []string{"-count", "0"} }},
		{"GOFLAGS disables tests", func(l *laneSpec) { l.jobEnv["GOFLAGS"] = "-count=0" }},
		{"GOFLAGS via step env", func(l *laneSpec) { l.steps[0].env = map[string]any{"GOFLAGS": "-run=NONE"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := laneSpec{
				jobEnv: map[string]any{"MEMQL_REQUIRE_DB": "1"},
				jobIf:  base.jobIf,
				steps:  []goTestStep{{pkgs: okStep.pkgs, flags: []string{"-count=1"}}},
			}
			tc.mutate(&l)
			if got := laneRunFindings(l); len(got) == 0 {
				t.Errorf("no finding reported; this lane can report a non-failure having run nothing")
			}
		})
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

// TestParseDBTestsJob_CommentMentioningGoTestDoesNotArmRefusal pins the CALL
// SITE, not just mentionsGoTest itself: the extensions step legitimately loops
// over pg_isready, and a comment above it must not make the gate refuse it.
func TestParseDBTestsJob_CommentMentioningGoTestDoesNotArmRefusal(t *testing.T) {
	spec := mustParse(t, "jobs:\n  db-tests:\n    steps:\n"+
		"      - name: create required Postgres extensions\n        run: |\n"+
		"          # wait for postgres before go test runs\n"+
		"          for i in $(seq 1 30); do\n            pg_isready\n          done\n"+
		"      - run: go test -count=1 ./component/memql/...\n")
	if len(spec.nonPlainRun) != 0 {
		t.Errorf("nonPlainRun = %v; a COMMENT mentioning `go test` armed the refusal on a block "+
			"that runs no tests -- keying a gate on prose is the defect this package criticises "+
			"elsewhere", spec.nonPlainRun)
	}
	if len(spec.steps) != 1 {
		t.Errorf("steps = %d, want 1 (the real invocation)", len(spec.steps))
	}
}
