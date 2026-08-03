package cidb

// dbgate_test.go -- the CI drift gate for the db-tests lane (memql#2886).
//
// Three assertions, one per way the lane could report a non-failure while
// having verified nothing. See doc.go for the rationale and for what this
// deliberately leaves to a separate issue.

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// dbTestsJob is the ci.yml job key this gate reads. A rename must update this
// constant, and the gate fails loudly rather than silently passing if it does
// not -- an absent job is a broken gate, not a satisfied one.
const dbTestsJob = "db-tests"

// dbtestImport is what makes a test db-gated: the package holding the skip /
// fail decision (dbtest.Unreachable) and the shared-schema migration
// (dbtest.EnsureSchema).
const dbtestImport = "github.com/znasllc-io/memql/component/database/dbtest"

// ciWorkflow is the PR-critical workflow carrying the lane.
const ciWorkflow = "ci.yml"

// selfPkg is this gate's own directory, excluded from the scan.
//
// It imports dbtest for the RequireDB predicate and its env-var name, not to
// reach a database, so counting it would report the gate as a db-gated suite
// that the lane fails to run. The import heuristic is right for the tree and
// wrong for exactly one package: the one doing the checking. Matched EXACTLY,
// never as a prefix, so it hides that one directory and nothing near it.
const selfPkg = "scripts/cidb"

// goTestCmd matches a `go test` invocation. Applied only to the text of a
// step's `run:` block, never to the whole file -- the mistake scripts/citags
// records is a gate satisfied by a step's `name:` label -- and only at the
// START of a comment-stripped line, so the same mistake cannot recur one layer
// down inside the shell. `echo go test ./x/`, `false && go test ./x/` and a
// heredoc body are all text this gate must not count, and a `# go test ./x/`
// left behind by someone debugging the lane is the likeliest of them.
var goTestCmd = regexp.MustCompile(`^go test\b(.*)$`)

// runFlag matches a -run selector. The lane must not carry one: a -run pattern
// that matches nothing executes zero tests while the step still reports ok,
// which is precisely the silent-nothing failure this gate exists to prevent.
var runFlag = regexp.MustCompile(`^-{1,2}run(=|$)`)

// heredocStart matches a heredoc redirection and captures its delimiter,
// covering the `<<EOF`, `<<-EOF`, `<<'EOF'` and `<<"EOF"` spellings. The lane's
// own "create required Postgres extensions" step uses `<<'SQL'`, so heredocs
// are not hypothetical in this file.
var heredocStart = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// workflowDoc is the sliver of the GitHub Actions schema this gate reads.
//
// Env is map[string]any rather than map[string]string because Actions accepts
// unquoted scalars: `MEMQL_REQUIRE_DB: 1` is a YAML int and would fail to
// unmarshal into a string, which would read as "the key is absent" -- the exact
// false negative this gate exists to prevent. ContinueOnError is `any` for the
// same reason: it may be a bool or an expression string.
type workflowDoc struct {
	Jobs map[string]struct {
		Env             map[string]any `yaml:"env"`
		ContinueOnError any            `yaml:"continue-on-error"`
		Steps           []struct {
			Run             string         `yaml:"run"`
			If              string         `yaml:"if"`
			Env             map[string]any `yaml:"env"`
			ContinueOnError any            `yaml:"continue-on-error"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// goTestStep is one step of the lane that actually invokes `go test`.
type goTestStep struct {
	ifCond          string
	env             map[string]any
	continueOnError any
	pkgs            []string
	flags           []string
}

// hasRunFilter reports whether the step passes a -run selector to `go test`.
func (s goTestStep) hasRunFilter() bool {
	for _, f := range s.flags {
		if runFlag.MatchString(f) {
			return true
		}
	}
	return false
}

// laneSpec is everything the gates need to know about the db-tests job.
type laneSpec struct {
	jobEnv          map[string]any
	continueOnError any
	steps           []goTestStep
}

// pkgs returns every package argument across the lane's `go test` steps.
func (l laneSpec) pkgs() []string {
	var out []string
	for _, s := range l.steps {
		out = append(out, s.pkgs...)
	}
	return out
}

// effectiveEnv resolves one variable the way Actions does for a given step:
// step-level env wins over job-level env.
//
// Modelling only the job level was a real hole -- a step-level
// `MEMQL_REQUIRE_DB: '0'` reverts the whole lane to green-by-skip while the
// gate stays green, which is worse than the original bug, because ci.yml then
// carries a comment claiming the key is enforced.
func (l laneSpec) effectiveEnv(s goTestStep, key string) (val any, present bool) {
	if v, ok := s.env[key]; ok {
		return v, true
	}
	v, ok := l.jobEnv[key]
	return v, ok
}

// truthy reports whether a YAML scalar reads as on, using the SAME predicate
// the production code uses, so the gate cannot drift from the parser it guards.
func truthy(t *testing.T, raw any) bool {
	t.Helper()
	t.Setenv(dbtest.RequireDBEnv, fmt.Sprintf("%v", raw))
	return dbtest.RequireDB()
}

// isFalsy reports whether a `continue-on-error:` value is absent or false. Any
// other value -- true, or an expression -- lets the lane fail without failing
// the build, and ci-required treats the resulting non-failure as a pass.
func isFalsy(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "" || s == "false"
	default:
		return false
	}
}

// goTestArgs extracts the argument string of every `go test` invocation in one
// `run:` block.
//
// Not a shell parser, and deliberately conservative: it counts a line only when
// the command is the FIRST word of a non-comment line outside a heredoc body.
// Every case it declines to count means fewer packages found, which reds the
// coverage gate -- the safe direction for a gate whose whole purpose is to
// refuse to be satisfied by something that never executes.
func goTestArgs(run string) []string {
	var out []string
	var heredocEnd string

	for _, raw := range strings.Split(run, "\n") {
		line := strings.TrimSpace(raw)

		// Inside a heredoc body: text, not commands, until the delimiter.
		if heredocEnd != "" {
			if line == heredocEnd {
				heredocEnd = ""
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := goTestCmd.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
		if m := heredocStart.FindStringSubmatch(line); m != nil {
			heredocEnd = m[1]
		}
	}
	return out
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

// parseDBTestsJob extracts the db-tests job's env, its guards, and the `go
// test` steps within it, from raw workflow YAML.
//
// Pure, so the gate's own parsing is unit-testable on synthetic fixtures: a
// scanner that silently finds nothing is the exact failure mode this whole
// issue is about, and a gate nobody tests is one that passes when it breaks.
func parseDBTestsJob(data []byte) (laneSpec, error) {
	var wf workflowDoc
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return laneSpec{}, fmt.Errorf("parsing workflow YAML: %w", err)
	}

	job, ok := wf.Jobs[dbTestsJob]
	if !ok {
		keys := make([]string, 0, len(wf.Jobs))
		for k := range wf.Jobs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return laneSpec{}, fmt.Errorf("no %q job (jobs: %v)", dbTestsJob, keys)
	}

	spec := laneSpec{jobEnv: job.Env, continueOnError: job.ContinueOnError}
	for _, step := range job.Steps {
		args := goTestArgs(step.Run)
		if len(args) == 0 {
			continue
		}
		gs := goTestStep{ifCond: step.If, env: step.Env, continueOnError: step.ContinueOnError}
		for _, a := range args {
			for _, f := range strings.Fields(a) {
				// ONLY a ./-prefixed argument is a package. Loosening this to
				// "contains a slash" swallows flags carrying paths, and each
				// one silently becomes a package argument matching nothing.
				if strings.HasPrefix(f, "./") {
					gs.pkgs = append(gs.pkgs, f)
				} else {
					gs.flags = append(gs.flags, f)
				}
			}
		}
		spec.steps = append(spec.steps, gs)
	}
	return spec, nil
}

// loadDBTestsJob reads ci.yml from disk and parses the db-tests job out of it.
func loadDBTestsJob(t *testing.T, root string) laneSpec {
	t.Helper()

	path := filepath.Join(root, ".github", "workflows", ciWorkflow)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	spec, err := parseDBTestsJob(data)
	if err != nil {
		t.Fatalf("%s: %v.\n\n"+
			"Either the lane was removed -- in which case the db-gated suites run nowhere and "+
			"this gate should not be quietly deleted with it -- or it was renamed, in which case "+
			"update dbTestsJob in this file (memql#2886).", path, err)
	}
	return spec
}

// coversOne reports whether a single package argument covers dir, a
// repo-relative directory such as "component/memql". The root package is ".".
func coversOne(pkg, dir string) bool {
	p := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(pkg), "./"), "/")
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

// dbGatedTest is one Test function in a _test.go file that imports dbtest.
type dbGatedTest struct {
	dir  string // repo-relative, e.g. "component/memql"
	file string // repo-relative path
	name string // e.g. "TestQueryByRowId"
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
	root := repoRoot(t)
	lane := loadDBTestsJob(t, root)

	if _, present := lane.jobEnv[dbtest.RequireDBEnv]; !present {
		t.Fatalf("the %q job in %s does not set %s.\n\n"+
			"Every db-gated test self-skips when it cannot reach Postgres, so without this key a "+
			"slow, unhealthy or unpullable service container degrades the whole DB-gated suite to "+
			"GREEN SKIPS instead of a red build -- and the PRs resting their only end-to-end "+
			"evidence on this lane merge on a signal that never fired. Add "+
			"`%s: '1'` to the job's env block (memql#2886).",
			dbTestsJob, ciWorkflow, dbtest.RequireDBEnv, dbtest.RequireDBEnv)
	}

	if len(lane.steps) == 0 {
		t.Fatalf("the %q job in %s has no `go test` step, so the env key it sets guards nothing "+
			"(memql#2886).", dbTestsJob, ciWorkflow)
	}

	// The EFFECTIVE value per step, not just the job-level one: Actions gives
	// step-level env precedence, so a step override silently reverts the lane
	// to green-by-skip while the job env still reads as guarded.
	for i, s := range lane.steps {
		raw, present := lane.effectiveEnv(s, dbtest.RequireDBEnv)
		if !present {
			t.Fatalf("`go test` step %d of the %q job resolves no %s (memql#2886).",
				i, dbTestsJob, dbtest.RequireDBEnv)
		}
		if !truthy(t, raw) {
			t.Fatalf("`go test` step %d of the %q job in %s resolves %s=%q, which "+
				"dbtest.RequireDB treats as OFF.\n\n"+
				"A present-but-falsy value is worse than an absent one: it reads as though the "+
				"lane is guarded while every db-gated test still degrades to a green skip. Note "+
				"that a STEP-level env overrides the job-level one. Use '1' (memql#2886).",
				i, dbTestsJob, ciWorkflow, dbtest.RequireDBEnv, fmt.Sprintf("%v", raw))
		}
	}
}

// TestDBTestsLaneCannotPassWithoutRunning guards the ways the lane can report a
// non-failure while executing nothing at all. `ci-required` treats a skipped or
// continue-on-error job as a pass, so each of these is a silent hole rather
// than a visible one (memql#2886).
func TestDBTestsLaneCannotPassWithoutRunning(t *testing.T) {
	root := repoRoot(t)
	lane := loadDBTestsJob(t, root)

	if !isFalsy(lane.continueOnError) {
		t.Errorf("the %q job in %s sets continue-on-error: %v.\n\n"+
			"The lane could then fail without failing the build, and ci-required would still "+
			"report green -- which is the bug this lane exists to prevent, one level up.",
			dbTestsJob, ciWorkflow, lane.continueOnError)
	}

	for i, s := range lane.steps {
		if strings.TrimSpace(s.ifCond) != "" {
			t.Errorf("`go test` step %d of the %q job carries `if: %s`.\n\n"+
				"A step-level condition can skip the suite entirely while the job still "+
				"succeeds. Path routing belongs on the JOB's `if:`, which is where it already "+
				"is (memql#2886).", i, dbTestsJob, s.ifCond)
		}
		if !isFalsy(s.continueOnError) {
			t.Errorf("`go test` step %d of the %q job sets continue-on-error: %v -- the suite "+
				"could fail without failing the lane (memql#2886).",
				i, dbTestsJob, s.continueOnError)
		}
		if s.hasRunFilter() {
			t.Errorf("`go test` step %d of the %q job passes a -run selector (%v).\n\n"+
				"A -run pattern that matches nothing executes ZERO tests and still reports ok. "+
				"This lane must run its packages unfiltered (memql#2886).",
				i, dbTestsJob, s.flags)
		}
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
	pkgs := loadDBTestsJob(t, root).pkgs()

	if len(pkgs) == 0 {
		t.Fatalf("parsed no `go test ./...` package arguments from the %q job in %s.\n\n"+
			"Either the test step was removed, or this gate's YAML parsing broke -- both are "+
			"failures, because neither leaves anything running the db-gated suites (memql#2886).",
			dbTestsJob, ciWorkflow)
	}

	all := scanDBGatedTests(t, root)
	if len(all) == 0 {
		t.Fatalf("found no _test.go file importing %s anywhere in the tree.\n\n"+
			"That is the scanner being broken, not the tree: the db-gated suites are what the "+
			"%q lane exists to run (memql#2886).", dbtestImport, dbTestsJob)
	}

	var covered, uncovered []dbGatedTest
	for _, dbt := range all {
		if covers(pkgs, dbt.dir) {
			covered = append(covered, dbt)
		} else {
			uncovered = append(uncovered, dbt)
		}
	}

	dbGatedDirs := map[string]bool{}
	for _, dbt := range all {
		dbGatedDirs[dbt.dir] = true
	}
	knownDirs := make([]string, 0, len(dbGatedDirs))
	for d := range dbGatedDirs {
		knownDirs = append(knownDirs, d)
	}
	sort.Strings(knownDirs)

	// Per-argument, so a selector entry that has silently gone to zero is
	// caught even while its siblings still carry tests.
	for _, p := range pkgs {
		n := 0
		for _, dbt := range all {
			if coversOne(p, dbt.dir) {
				n++
			}
		}
		if n == 0 {
			t.Errorf("the %q job's package argument %q matches NO db-gated test.\n\n"+
				"That argument contributes nothing to the lane: it provisions Postgres and then "+
				"executes zero DB assertions under it, reporting `ok`. Packages that do carry "+
				"db-gated tests: %v.\n\n"+
				"Either drop the argument or point it at a db-gated package, remembering that a "+
				"new one also needs a TestMain calling dbtest.EnsureSchema before it can share "+
				"the lane's one database (memql#2551, memql#2886).",
				dbTestsJob, p, knownDirs)
		}
	}

	if len(covered) == 0 {
		t.Fatalf("the %q job runs %v, which covers NONE of the %d db-gated tests in the tree.\n\n"+
			"The lane would provision Postgres and then execute zero DB assertions against it, "+
			"reporting `ok`. Packages that do carry db-gated tests: %v (memql#2886).",
			dbTestsJob, pkgs, len(all), knownDirs)
	}

	// Not an assertion -- the four uncovered packages are a known, separately
	// tracked gap (see doc.go). Logging keeps them visible instead of letting a
	// green gate imply full coverage.
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
		t.Logf("Test functions in dbtest-importing files NOT covered by the %q lane, so their DB "+
			"assertions never run in CI: %v. Tracked in memql#3030 -- each package needs a "+
			"TestMain calling dbtest.EnsureSchema (memql#2551) before it can join the lane. A "+
			"log and not a failure on purpose; see doc.go.\n\n"+
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
