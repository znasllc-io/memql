package cidb

// Native database coverage and shared-schema provisioning must agree.
import (
	"fmt"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/scripts/ci/pyyaml"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const dbTestsJob = "db-tests"
const dbtestImport = "github.com/znasllc-io/memql/component/database/dbtest"

// This guard imports dbtest only to check its require-DB predicate.
const selfPkg = "scripts/cidb"

var zeroExecutionFlags = regexp.MustCompile(`^-{1,2}(test\.)?(run|skip|count=0)(=|$)`)

type dbGatedTest struct {
	dir  string
	file string
	name string
}

// goflagsDisablesTests reports whether a GOFLAGS value smuggles a
// zero-execution flag in through the environment, where none of the
// command-line checks would ever see it.
func goflagsDisablesTests(v any) string {
	for _, f := range strings.Fields(fmt.Sprintf("%v", v)) {
		if zeroExecutionFlags.MatchString(f) {
			return f
		}
	}
	return ""
}

// truthy reports whether a YAML scalar reads as on, using the SAME predicate
// the production code uses, so the gate cannot drift from the parser it guards.
func truthy(t *testing.T, raw any) bool {
	t.Helper()
	t.Setenv(dbtest.RequireDBEnv, fmt.Sprintf("%v", raw))
	return dbtest.RequireDB()
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod walking up from %s", filepath.Dir(file))
		}
		dir = parent
	}
}

// coversOne reports whether a single package argument covers dir, a
// repo-relative directory such as "component/memql". The root package is ".".
func coversOne(pkg, dir string) bool {
	p := strings.TrimSuffix(strings.TrimPrefix(pkg, "./"), "/")
	switch {
	case p == "...":
		// `./...` -- the whole module.
		return true
	case p == "":
		// `./` or `.` -- the ROOT PACKAGE ONLY, and the narrowest selector
		// there is. Folding it in with `./...` read the narrowest argument as
		// the widest and made the coverage gate vacuous.
		return dir == "."
	case strings.HasSuffix(p, "/..."):
		base := strings.TrimSuffix(p, "/...")
		return dir == base || strings.HasPrefix(dir, base+"/")
	default:
		// Exact directory match. NOT a prefix match: `./component/mem` must
		// not be read as covering `component/memql`.
		return dir == p
	}
}

// covers reports whether any of the lane's package arguments covers dir.
func covers(pkgs []string, dir string) bool {
	for _, p := range pkgs {
		if coversOne(p, dir) {
			return true
		}
	}
	return false
}

// coverageFindings returns one message per way the lane's SELECTOR fails to tie
// the job to the packages that actually carry db-gated tests.
//
// The tie is asserted in BOTH directions (memql#3095):
//
//	provisioned -> in selector   a package that earned an EnsureSchema TestMain
//	                             and is not in the selector runs nowhere.
//	in selector -> provisioned   a selector-covered package carrying db-gated
//	                             tests and no EnsureSchema TestMain races the
//	                             shared migration.
//
// Only the first existed originally, and doc.go was explicit that the rule was
// one-way -- a gap, honestly documented, not a false claim. It was still a gap
// wide enough to drive the change through: deleting ALL of the
// main_dbschema_test.go files while leaving the selector listing their packages
// left this gate fully green. The central artifact of memql#3091 could be
// removed wholesale and the gate that tightens it said nothing.
//
// What the second direction actually protects is the memql#2551 invariant. The
// lane runs per-package test binaries as PARALLEL PROCESSES against ONE
// database; without a TestMain serializing the migration behind an advisory
// lock they race it, and the lane fails INTERMITTENTLY with
// `relation "MemoryNodes" does not exist`. An intermittent red is the worst
// shape of failure this lane can produce, so the invariant is worth an
// assertion rather than a convention.
//
// Pure, for the same reason laneRunFindings is: inline, these were evaluated
// only against the real tree, so deleting one changed nothing observable.
func coverageFindings(pkgs []string, all []dbGatedTest, provisioned []string) []string {
	var out []string

	if len(pkgs) == 0 {
		return append(out, fmt.Sprintf("parsed no `go test ./...` package arguments from the %q "+
			"job. Either the test step was removed, or this gate's parsing broke -- neither "+
			"leaves anything running the db-gated suites (memql#2886).", dbTestsJob))
	}
	if p, found := wholeModuleWildcard(pkgs); found {
		return append(out, fmt.Sprintf("the %q job's selector contains %q. A whole-module "+
			"wildcard makes every coverage assertion vacuous -- each db-gated package reports "+
			"as covered regardless of what the lane runs (memql#2886).", dbTestsJob, p))
	}

	dirs := map[string]bool{}
	for _, dbt := range all {
		dirs[dbt.dir] = true
	}
	known := make([]string, 0, len(dirs))
	for d := range dirs {
		known = append(known, d)
	}
	sort.Strings(known)

	// No separate "covers nothing at all" check: it is strictly implied by the
	// per-argument one below. If every argument matches at least one db-gated
	// test then the total cannot be zero, and the empty-selector case returned
	// above. A redundant assertion is one no test can show to be necessary.
	for _, p := range pkgs {
		n := 0
		for _, dbt := range all {
			if coversOne(p, dbt.dir) {
				n++
			}
		}
		if n == 0 {
			out = append(out, fmt.Sprintf("the %q job's package argument %q matches NO db-gated "+
				"test -- it contributes nothing to the lane. Packages that do: %v (memql#2886).",
				dbTestsJob, p, known))
		}
	}
	for _, dir := range provisioned {
		if !covers(pkgs, dir) {
			out = append(out, fmt.Sprintf("package %q has a TestMain calling dbtest.EnsureSchema "+
				"but the %q job's selector %v does not cover it. That TestMain exists so the "+
				"package can migrate the shared schema and run in this lane (memql#2551); not "+
				"being in the selector means its DB assertions run nowhere (memql#2886).",
				dir, dbTestsJob, pkgs))
		}
	}

	// The inverse (memql#3095). Keyed on `known` -- the directories that carry
	// db-gated tests -- and NOT on the selector's expansion, which is the
	// distinction that makes this rule usable rather than absurd: the selector
	// expands to many packages with no DB assertion in them, and none of those
	// has any business owning a migration TestMain. Only a package that both is
	// RUN by this lane and TOUCHES the shared database needs one.
	provisionedSet := make(map[string]bool, len(provisioned))
	for _, dir := range provisioned {
		provisionedSet[dir] = true
	}
	for _, dir := range known {
		// selfPkg imports dbtest for the RequireDB predicate and its env-var
		// name, never to reach a database, so it must never be asked for a
		// migration TestMain. scanDBGatedTests already drops it upstream; the
		// check is repeated here because this function is pure and unit-tested
		// with synthetic input, where that upstream guarantee does not hold.
		// Matched EXACTLY, never as a prefix, exactly as the upstream one is.
		if dir == selfPkg || provisionedSet[dir] || !covers(pkgs, dir) {
			continue
		}
		out = append(out, fmt.Sprintf("package %q carries db-gated tests and IS covered by the %q "+
			"job's selector %v, but has no TestMain calling dbtest.EnsureSchema. The lane runs "+
			"per-package binaries as PARALLEL PROCESSES against one shared database, so without "+
			"that TestMain this package races the others' migration and the lane fails "+
			"INTERMITTENTLY with `relation \"MemoryNodes\" does not exist` (memql#2551).\n\n"+
			"REMEDY: add %s/main_dbschema_test.go with a TestMain that calls "+
			"dbtest.EnsureSchema(context.Background()) and os.Exit(1)s on error before "+
			"os.Exit(m.Run()) -- copy any of the existing ones. If this package genuinely must "+
			"not touch the shared database, remove it from the selector instead; leaving it in "+
			"both states is the drift this asserts against (memql#3095).",
			dir, dbTestsJob, pkgs, dir))
	}
	return out
}

// wholeModuleWildcard returns a `./...` argument from the selector, if any.
// It makes every coverage assertion vacuous, so it is never right in this lane.
func wholeModuleWildcard(pkgs []string) (string, bool) {
	for _, p := range pkgs {
		if strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/") == "..." {
			return p, true
		}
	}
	return "", false
}

// scanDBGatedTests walks the tree for _test.go files that import dbtest and
// collects the Test functions they declare.
//
// Only files that compile in the DEFAULT build are counted: the db-tests lane
// passes no -tags, so a //go:build-tagged file would not run there even though
// it imports dbtest. (scripts/citags is the gate for that separate hazard.)
func scanDBGatedTests(t *testing.T, root string) []dbGatedTest {
	t.Helper()

	var out []dbGatedTest
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (n == "testdata" || n == "node_modules" || n == "vendor" ||
				strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}

		dir := filepath.Dir(path)
		built, mErr := build.Default.MatchFile(dir, d.Name())
		if mErr != nil {
			t.Fatalf("MatchFile(%s, %s): %v", dir, d.Name(), mErr)
		}
		if !built {
			return nil
		}

		f, pErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if pErr != nil {
			t.Fatalf("parsing %s: %v", path, pErr)
		}
		if !importsDBTest(f) {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		relDir := filepath.ToSlash(filepath.Dir(rel))
		if relDir == selfPkg {
			return nil
		}
		for _, name := range testFuncNames(f) {
			out = append(out, dbGatedTest{dir: relDir, file: rel, name: name})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	return out
}

// provisionedPkgs returns the repo-relative directories of packages that
// declare a TestMain calling dbtest.EnsureSchema -- i.e. packages explicitly
// provisioned to share this lane's one database (memql#2551).
func provisionedPkgs(t *testing.T, root string) []string {
	t.Helper()

	seen := map[string]bool{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if path != root && (n == "testdata" || n == "node_modules" || n == "vendor" ||
				strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		built, mErr := build.Default.MatchFile(filepath.Dir(path), d.Name())
		if mErr != nil {
			t.Fatalf("MatchFile(%s, %s): %v", filepath.Dir(path), d.Name(), mErr)
		}
		if !built {
			return nil
		}
		f, pErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if pErr != nil {
			t.Fatalf("parsing %s: %v", path, pErr)
		}
		if !importsDBTest(f) || !declaresEnsureSchemaTestMain(f) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		relDir := filepath.ToSlash(filepath.Dir(rel))
		if relDir != selfPkg {
			seen[relDir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}

	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// declaresEnsureSchemaTestMain reports whether the file declares a TestMain
// whose body calls dbtest.EnsureSchema.
func declaresEnsureSchemaTestMain(f *ast.File) bool {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name == nil || fn.Name.Name != "TestMain" || fn.Body == nil {
			continue
		}
		local := dbtestLocalName(f)
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "EnsureSchema" {
				return true
			}
			if ident, ok := sel.X.(*ast.Ident); ok && local != "" && ident.Name == local {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// dbtestLocalName returns the identifier the file refers to the dbtest package
// by -- its alias when aliased, otherwise "dbtest" -- or "" if it does not
// import it. Hardcoding "dbtest" would miss `dbt "…/dbtest"`.
func dbtestLocalName(f *ast.File) string {
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != dbtestImport {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name // aliased, including "." and "_"
		}
		return "dbtest"
	}
	return ""
}

// importsDBTest reports whether the parsed file imports the dbtest package.
func importsDBTest(f *ast.File) bool {
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if p == dbtestImport {
			return true
		}
	}
	return false
}

// testFuncNames returns the `func TestXxx(*testing.T)` declarations in the file.
//
// TestMain is excluded: it is the harness that migrates the shared schema, not
// an assertion, so a package whose only db-gated function were TestMain would
// still execute zero db tests.
func testFuncNames(f *ast.File) []string {
	var names []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name == nil {
			continue
		}
		name := fn.Name.Name
		if name == "TestMain" || !strings.HasPrefix(name, "Test") {
			continue
		}
		// Exclude Testify and friends: the next rune after "Test" must not be
		// lower-case, matching `go test`'s own rule.
		if len(name) > 4 && name[4] >= 'a' && name[4] <= 'z' {
			continue
		}
		names = append(names, name)
	}
	return names
}

// TestDBTestsLaneMakesAnUnreachableDatabaseFail is the first gate.
//
// The db-tests job must set MEMQL_REQUIRE_DB to a value the production
// predicate treats as truthy. Without it every db-gated test self-skips when
// Postgres is unreachable and the lane reports `ok` having asserted nothing
// (memql#2886).
//
// The truthiness check calls dbtest.RequireDB rather than comparing to "1", so
// the gate cannot drift from the parser it is guarding: `MEMQL_REQUIRE_DB: no`
// is syntactically present and semantically off, and this catches it.
func TestDBTestsLaneMakesAnUnreachableDatabaseFail(t *testing.T) {
	step := nativeDatabaseStep(t, nativeSpec(t)).step
	directory := t.TempDir()
	// Execute the declared shell, replacing only its external build/test tools.
	// The probe sees the effective environment and fails like a failed suite.
	for name, script := range map[string]string{
		"bash":    "#!/bin/sh\ntest \"$*\" = scripts/identity/build-css.sh\n",
		"python3": "#!/bin/sh\ntest \"$*\" = scripts/ci/pipeline-db-tests.py || exit 90\nprintf '%s\\n' \"$MEMQL_REQUIRE_DB\" \"$GOFLAGS\" \"$MEMQL_PACKAGES\" > receipt\nexit 73\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/sh", "-c", step.Run)
	cmd.Dir = directory
	cmd.Env = []string{"PATH=" + directory, "HOME=" + directory, "MEMQL_PACKAGES=./component/memql"}
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 73 {
		t.Fatalf("database step hid the failing test runner or never reached it: %v %s", err, out)
	}
	receipt, err := os.ReadFile(filepath.Join(directory, "receipt"))
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Split(strings.TrimSpace(string(receipt)), "\n")
	if len(values) != 3 || !truthy(t, values[0]) || values[2] != "./component/memql" {
		t.Fatalf("database runner received an unguarded or replaced selection: %q", receipt)
	}
	if flag := goflagsDisablesTests(values[1]); flag != "" {
		t.Fatalf("effective GOFLAGS disables tests: %s", flag)
	}
}

// Exercise the real native runner's failure, isolation, empty-selection and
// timeout-budget tests. These replace the retired Actions shell/matrix model.
func TestDBTestsLaneCannotPassWithoutRunning(t *testing.T) {
	pyyaml.Require(t)
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "scripts/ci", "-p", "pipeline_db_tests_test.py")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native database runner: %v\n%s", err, out)
	}
}

// TestDBTestsLaneRunsAtLeastOneDBGatedTest is the count assertion memql#2886
// asks for.
//
// MEMQL_REQUIRE_DB=1 makes "the database was unreachable" a failure. It says
// nothing about "the selector matched no db-gated test at all", which is the
// same silent-nothing failure one level up -- the lane would provision
// Postgres, run a suite of ordinary tests, and report `ok` having executed zero
// DB assertions.
//
// Asserting statically that the selector covers db-gated tests makes that
// outcome impossible by construction rather than detected after the fact, and
// -- unlike a runtime count -- it is checked on EVERY pull request, including
// the ones where the db-tests lane itself is skipped.
//
// EVERY package argument must contribute at least one, not merely the selector
// as a whole: with three arguments, "at least one overall" would stay green
// after two of them went to zero. Per-argument is the assertion that actually
// catches drift, and it passes on the tree today.
func TestDBTestsLaneRunsAtLeastOneDBGatedTest(t *testing.T) {
	root := repoRoot(t)
	spec := nativeSpec(t)
	var pkgs []string
	for _, tree := range spec.Select.DBGated {
		pkgs = append(pkgs, "./"+tree+"/...")
	}

	all := scanDBGatedTests(t, root)
	if len(all) == 0 {
		t.Fatalf("found no _test.go file importing %s anywhere in the tree.\n\n"+
			"That is the scanner being broken, not the tree: the db-gated suites are what the "+
			"%q lane exists to run (memql#2886).", dbtestImport, dbTestsJob)
	}

	for _, f := range coverageFindings(pkgs, all, provisionedPkgs(t, root)) {
		t.Error(f)
	}

	var covered, uncovered []dbGatedTest
	for _, dbt := range all {
		if covers(pkgs, dbt.dir) {
			covered = append(covered, dbt)
		} else {
			uncovered = append(uncovered, dbt)
		}
	}

	// NOW AN ASSERTION (memql#3030). This was a log line while four packages
	// were a known, separately tracked gap -- deliberately not a failure, so
	// the tree was not red over a defect the change that added this gate did
	// not fix.
	//
	// Those four (component/automations, component/grpc, integrations/cognition,
	// integrations/planner) joined the lane in #3030, so the exemption has no
	// subject left, and the log becomes the failure it was always meant to be:
	// the next db-gated package added outside the selector reds immediately
	// instead of silently contributing zero DB assertions to CI.
	//
	// That is the whole shape of this defect class -- the four packages each
	// printed `ok` for weeks while running none of their 19 DB assertions,
	// because nothing asserted the selector reached them.
	if len(uncovered) > 0 {
		dirs := map[string]int{}
		for _, dbt := range uncovered {
			dirs[dbt.dir]++
		}
		names := make([]string, 0, len(dirs))
		for d := range dirs {
			names = append(names, fmt.Sprintf("%s (%d)", d, dirs[d]))
		}
		sort.Strings(names)
		t.Errorf("Test functions in dbtest-importing files NOT covered by the %q lane, so their DB "+
			"assertions never run in CI: %v. Each package needs a TestMain calling "+
			"dbtest.EnsureSchema (memql#2551) before it can join the lane -- then add it to the "+
			"selector. The lane runs per-package binaries as parallel processes against ONE "+
			"database, so the TestMain is what stops them racing each other's migration.\n\n"+
			"Treat the COUNTS as a proxy, not a census: they are exact in neither direction. A "+
			"dbtest-importing file may hold non-DB tests (over-counts), and a genuinely db-gated "+
			"test may reach dbtest through a helper in a sibling file rather than importing it "+
			"itself (under-counts -- component/memql/nodespec_persist_2885_db_test.go is the "+
			"live example). The pass/fail decisions above depend only on whether a package "+
			"contributes any, which is unaffected.", dbTestsJob, names)
	}
	t.Logf("%q covers %d Test functions in dbtest-importing files across the selector %v",
		dbTestsJob, len(covered), pkgs)
}

// canonicalPatterns runs scripts/ci/db-gated-packages.sh --patterns: the same
// canonical set the planner reads, read the same way, so this gate and the
// planner cannot disagree about what the lane owns.
func canonicalPatterns(t *testing.T, root string) []string {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(root, "scripts", "ci", "db-gated-packages.sh"), "--patterns")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("db-gated-packages.sh --patterns: %v", err)
	}
	var pkgs []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			pkgs = append(pkgs, l)
		}
	}
	if len(pkgs) == 0 {
		t.Fatal("db-gated-packages.sh --patterns printed nothing: the planned lane would own no package")
	}
	return pkgs
}
