package cidb

// dbgate_test.go -- the CI drift gate for the db-tests lane (memql#2886).
//
// Two assertions, one per way the lane could report `ok` having verified
// nothing. See doc.go for the rationale and for what this deliberately leaves
// to a separate issue.

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
// wrong for exactly one package: the one doing the checking.
const selfPkg = "scripts/cidb"

// goTestCmd matches a `go test` invocation. Applied only to the text of a
// step's `run:` block, never to the whole file -- the mistake scripts/citags
// records is a gate satisfied by a step's `name:` label.
var goTestCmd = regexp.MustCompile(`go test\b([^\n]*)`)

// workflowDoc is the sliver of the GitHub Actions schema this gate reads.
//
// Env is map[string]any rather than map[string]string because Actions accepts
// unquoted scalars: `MEMQL_REQUIRE_DB: 1` is a YAML int and would fail to
// unmarshal into a string, which would read as "the key is absent" -- the exact
// false negative this gate exists to prevent.
type workflowDoc struct {
	Jobs map[string]struct {
		Env   map[string]any `yaml:"env"`
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
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

// parseDBTestsJob extracts the db-tests job's env map and the package arguments
// of every `go test` step in it, from raw workflow YAML.
//
// Pure, so the gate's own parsing is unit-testable on synthetic fixtures: a
// scanner that silently finds nothing is the exact failure mode this whole
// issue is about, and a gate nobody tests is one that passes when it breaks.
func parseDBTestsJob(data []byte) (env map[string]any, pkgs []string, err error) {
	var wf workflowDoc
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, nil, fmt.Errorf("parsing workflow YAML: %w", err)
	}

	job, ok := wf.Jobs[dbTestsJob]
	if !ok {
		keys := make([]string, 0, len(wf.Jobs))
		for k := range wf.Jobs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return nil, nil, fmt.Errorf("no %q job (jobs: %v)", dbTestsJob, keys)
	}

	for _, step := range job.Steps {
		for _, cmd := range goTestCmd.FindAllStringSubmatch(step.Run, -1) {
			for _, f := range strings.Fields(cmd[1]) {
				if strings.HasPrefix(f, "./") {
					pkgs = append(pkgs, f)
				}
			}
		}
	}
	return job.Env, pkgs, nil
}

// loadDBTestsJob reads ci.yml from disk and parses the db-tests job out of it.
func loadDBTestsJob(t *testing.T, root string) (env map[string]any, pkgs []string) {
	t.Helper()

	path := filepath.Join(root, ".github", "workflows", ciWorkflow)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	env, pkgs, err = parseDBTestsJob(data)
	if err != nil {
		t.Fatalf("%s: %v.\n\n"+
			"Either the lane was removed -- in which case the db-gated suites run nowhere and "+
			"this gate should not be quietly deleted with it -- or it was renamed, in which case "+
			"update dbTestsJob in this file (memql#2886).", path, err)
	}
	return env, pkgs
}

// covers reports whether the lane's package arguments include dir, a
// repo-relative directory such as "component/memql".
func covers(pkgs []string, dir string) bool {
	for _, p := range pkgs {
		p = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/")
		switch {
		case p == "..." || p == "":
			return true
		case strings.HasSuffix(p, "/..."):
			if base := strings.TrimSuffix(p, "/..."); dir == base || strings.HasPrefix(dir, base+"/") {
				return true
			}
		case p == dir:
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
		// Exclude TestimonialFoo and friends: the next rune after "Test" must
		// not be lower-case, matching `go test`'s own rule.
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
	env, _ := loadDBTestsJob(t, root)

	raw, present := env[dbtest.RequireDBEnv]
	if !present {
		t.Fatalf("the %q job in %s does not set %s.\n\n"+
			"Every db-gated test self-skips when it cannot reach Postgres, so without this key a "+
			"slow, unhealthy or unpullable service container degrades the whole DB-gated suite to "+
			"GREEN SKIPS instead of a red build -- and the PRs resting their only end-to-end "+
			"evidence on this lane merge on a signal that never fired. Add "+
			"`%s: '1'` to the job's env block (memql#2886).",
			dbTestsJob, ciWorkflow, dbtest.RequireDBEnv, dbtest.RequireDBEnv)
	}

	value := fmt.Sprintf("%v", raw)
	t.Setenv(dbtest.RequireDBEnv, value)
	if !dbtest.RequireDB() {
		t.Fatalf("the %q job in %s sets %s=%q, which dbtest.RequireDB treats as OFF.\n\n"+
			"A present-but-falsy value is worse than an absent one: it reads as though the lane is "+
			"guarded while every db-gated test still degrades to a green skip. Use '1' (memql#2886).",
			dbTestsJob, ciWorkflow, dbtest.RequireDBEnv, value)
	}
}

// TestDBTestsLaneRunsAtLeastOneDBGatedTest is the second gate: the count
// assertion memql#2886 asks for.
//
// MEMQL_REQUIRE_DB=1 makes "the database was unreachable" a failure. It says
// nothing about "the selector matched no db-gated test at all", which is the
// same silent-nothing failure one level up -- the lane would provision
// Postgres, run a suite of ordinary tests, and report `ok` having executed zero
// DB assertions.
//
// Asserting statically that the selector covers at least one db-gated test
// makes that outcome impossible by construction rather than detected after the
// fact, and -- unlike a runtime count -- it is checked on EVERY pull request,
// including the ones where the db-tests lane itself is skipped.
func TestDBTestsLaneRunsAtLeastOneDBGatedTest(t *testing.T) {
	root := repoRoot(t)
	_, pkgs := loadDBTestsJob(t, root)

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

	if len(covered) == 0 {
		dirs := map[string]bool{}
		for _, dbt := range all {
			dirs[dbt.dir] = true
		}
		names := make([]string, 0, len(dirs))
		for d := range dirs {
			names = append(names, d)
		}
		sort.Strings(names)
		t.Fatalf("the %q job runs %v, which covers NONE of the %d db-gated tests in the tree.\n\n"+
			"The lane would provision Postgres and then execute zero DB assertions against it, "+
			"reporting `ok`. Packages that do carry db-gated tests: %v.\n\n"+
			"Add at least one of them to the job's package selector, remembering that a new "+
			"db-gated package also needs a TestMain calling dbtest.EnsureSchema before it can "+
			"share the lane's one database (memql#2551, memql#2886).",
			dbTestsJob, pkgs, len(all), names)
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
			"assertions never run in CI: %v. (A count of functions in such files, which is an "+
			"upper bound on the DB assertions among them.) Tracked separately -- each package "+
			"needs a TestMain calling dbtest.EnsureSchema (memql#2551) before it can join the "+
			"lane.", dbTestsJob, names)
	}
	t.Logf("%q covers %d Test functions in dbtest-importing files across the selector %v",
		dbTestsJob, len(covered), pkgs)
}
