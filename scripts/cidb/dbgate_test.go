package cidb

// dbgate_test.go -- the CI drift gate for the db-tests lane (memql#2886).
//
// Three assertions, one per way the lane could report a non-failure while
// having verified nothing. See doc.go for the rationale.

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

// goTestCmd matches a `go test` invocation at the START of a comment-stripped
// line, applied only to the text of a step's `run:` block and never to the
// whole file -- the mistake scripts/citags records is a gate satisfied by a
// step's `name:` label.
//
// The anchor is NOT sufficient on its own, and it is important to be honest
// about why: `go test` can be the first word of a line that never executes,
// inside `if`/`while`/`case`, a shell function body, or after `exit`. Regex
// cannot see that, and this gate is not a shell parser. runBlockIsPlain closes
// the gap from the other side -- it REFUSES a run block containing any such
// construct -- so what remains here only has to be right for a plain sequence
// of commands.
var goTestCmd = regexp.MustCompile(`^go test\b(.*)$`)

// zeroExecutionFlags are `go test` flags that make it exit 0 having run nothing.
//
// `-run` was the only one guarded originally, which read as though the category
// were closed. It is not, and the cheapest bypass is the nastiest: the lane
// already carries `-count=1`, so `-count=0` is a ONE CHARACTER edit that runs
// zero tests and still reports ok. `-test.`-prefixed spellings are accepted by
// the compiled test binary and bypass a guard that only knows the short form.
var zeroExecutionFlags = regexp.MustCompile(`^-{1,2}(test\.)?(run|skip|count=0)(=|$)`)

// valueTakingFlags take their value as the NEXT argument, so that argument is
// not a package even when it starts with "./". `go test -coverprofile
// ./cover.out ./pkg/...` otherwise reports ./cover.out as a package matching no
// db-gated test, and reds the lane for a legitimate edit.
var valueTakingFlags = map[string]bool{
	"-coverprofile": true, "-o": true, "-outputdir": true, "-cpuprofile": true,
	"-memprofile": true, "-blockprofile": true, "-mutexprofile": true,
	"-trace": true, "-tags": true, "-run": true, "-skip": true, "-count": true,
	"-timeout": true, "-exec": true, "-gcflags": true, "-ldflags": true,
}

// shellControlKeywords are line-initial words that mean the block is more than
// a plain sequence of commands. See runBlockIsPlain.
var shellControlKeywords = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
	"case": true, "esac": true, "function": true, "exit": true, "trap": true,
	"return": true, "eval": true, "source": true, ".": true,
	// cd/exec/alias change what every LATER line means: `cd examples/x` then
	// `go test ./...` runs one package while the gate reads ./... as the whole
	// module and reports every db-gated package covered.
	"cd": true, "exec": true, "alias": true,
}

// suppressors are trailing shell forms that swallow a non-zero exit, making the
// step succeed even when the suite failed.
//
// Forbidding YAML `continue-on-error` while accepting `|| true` -- which is
// shorter to write and the first thing anyone reaches for on a flaky lane --
// was incoherent. All three below were confirmed to exit 0 under `bash -e`,
// which is Actions' default shell:
//
//	go test … || true          exit 0
//	go test … &  + wait        exit 0  (bare `wait` always returns 0)
//	go test … | tee out.log    exit 0  (no pipefail by default)
//
// funcDefinition matches a shell function definition, including the no-space
// one-liner `go(){ …; }` which can shadow the toolchain itself.
var funcDefinition = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*\(\s*\)`)

var suppressors = regexp.MustCompile(`(\|\|\s*(true|:)\s*$|[^&]&\s*$|[^|]\|[^|]*$)`)

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
		If              string         `yaml:"if"`
		ContinueOnError any            `yaml:"continue-on-error"`
		Defaults        struct {
			Run struct {
				WorkingDirectory string `yaml:"working-directory"`
			} `yaml:"run"`
		} `yaml:"defaults"`
		Steps []struct {
			Run              string         `yaml:"run"`
			If               string         `yaml:"if"`
			Env              map[string]any `yaml:"env"`
			ContinueOnError  any            `yaml:"continue-on-error"`
			WorkingDirectory string         `yaml:"working-directory"`
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

// zeroExecutionFlag returns the first flag on this step that makes `go test`
// exit 0 having run nothing, or "".
func (s goTestStep) zeroExecutionFlag() string {
	for i, f := range s.flags {
		if zeroExecutionFlags.MatchString(f) {
			return f
		}
		// The SPACE-separated form. `-run`/`-skip` already match bare above,
		// but `-count` only disables when its value is 0 -- and `-count 1` ->
		// `-count 0` is the same one-character edit the `=` form warns about.
		if (f == "-count" || f == "-test.count") && i+1 < len(s.flags) &&
			strings.TrimSpace(s.flags[i+1]) == "0" {
			return f + " " + s.flags[i+1]
		}
	}
	return ""
}

// jobIfIsPathRouting reports whether a job-level `if:` is (only) path routing.
//
// A job `if:` decides whether the lane runs at all, and ci-required treats a
// SKIPPED job as a pass -- so a constant-false condition disables the db-gated
// suites with no visible failure. Path routing is legitimate and expected here;
// anything that could evaluate false regardless of the diff is not.
//
// Requiring a `needs.changes.outputs` mention is necessary but not sufficient,
// so a bare `false` literal is rejected outright: `${{ false && needs.changes.
// outputs.go == 'true' }}` mentions the right thing and never runs.
func jobIfIsPathRouting(cond string) bool {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return true // no condition: the lane always runs
	}
	if !strings.Contains(cond, "needs.changes.outputs") {
		return false
	}
	for _, f := range strings.Fields(strings.NewReplacer("(", " ", ")", " ", "!", " ").Replace(cond)) {
		if f == "false" {
			return false
		}
	}
	return true
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

// laneSpec is everything the gates need to know about the db-tests job.
type laneSpec struct {
	jobEnv          map[string]any
	jobIf           string
	continueOnError any
	steps           []goTestStep
	// workingDir is any job- or step-level working-directory. It relocates
	// every relative package argument without appearing in the shell at all.
	workingDir string
	// nonPlainRun holds the offending line of each run block this gate
	// refused to scan. See runBlockIsPlain -- refusing is how it fails closed.
	nonPlainRun []string
	// unparsedRun holds run blocks that mention `go test` but not as the first
	// word of a line, so the scanner could not read their packages.
	unparsedRun []string
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

// heredocDelimiter returns the delimiter of a heredoc opened on this line, if
// any.
//
// Hand-parsed rather than regex'd because the delimiter must be the LAST thing
// on the line for this to be a heredoc at all. A regex that merely looks for
// `<<` fires on `echo "redirect is a << b"`, captures `b`, and then swallows
// every following line -- which silently hides the real `go test` below it.
// Bash also permits `<<\EOF` and digit-leading delimiters like `<<1SQL`, so the
// delimiter charset is deliberately loose.
func heredocDelimiter(line string) string {
	i := strings.Index(line, "<<")
	if i < 0 {
		return ""
	}
	// `<<<` is a HERE-STRING, not a heredoc: it has no terminator, so treating
	// it as one skips the entire rest of the block looking for a delimiter that
	// never arrives -- hiding both constructs and invocations below it.
	if strings.HasPrefix(line[i:], "<<<") {
		return ""
	}
	rest := strings.TrimSpace(line[i+2:])
	rest = strings.TrimPrefix(rest, "-") // <<-EOF
	rest = strings.TrimSpace(rest)
	rest = strings.TrimPrefix(rest, `\`) // <<\EOF
	if rest == "" {
		return ""
	}

	// An opening quote must be matched by a closing one at end of line.
	if q := rest[0]; q == '\'' || q == '"' {
		body := rest[1:]
		end := strings.IndexByte(body, q)
		if end < 0 {
			return "" // unbalanced: not a heredoc opener
		}
		// Bash permits redirections and pipes AFTER the delimiter word
		// (`psql <<'SQL' >/dev/null`), so only ordinary trailing text
		// disqualifies it.
		if tail := strings.TrimSpace(body[end+1:]); tail != "" &&
			!strings.HasPrefix(tail, ">") && !strings.HasPrefix(tail, "|") &&
			!strings.HasPrefix(tail, "2>") {
			return ""
		}
		return body[:end]
	}
	// Unquoted: the delimiter is the whole remainder, and nothing may follow.
	if strings.ContainsAny(rest, " \t'\"") {
		return ""
	}
	return rest
}

// joinContinuations folds backslash-continued lines into one logical line.
//
// Wrapping a long invocation is the single likeliest future edit to this lane
// -- the selector grows past three packages and someone breaks the line. That
// used to produce three failures, two of them misdiagnosed ("the test step was
// removed"). Joining first makes the wrapped form simply work, which is better
// than any error message.
func joinContinuations(run string) string {
	var out []string
	var cur strings.Builder
	for _, raw := range strings.Split(run, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if strings.HasSuffix(line, `\`) {
			cur.WriteString(strings.TrimSuffix(line, `\`))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(line)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return strings.Join(out, "\n")
}

// mentionsGoTest reports whether a run block invokes `go test` anywhere,
// ignoring comment lines.
//
// The probe must ignore comments for the same reason both scanners do: keying a
// gate on prose is the defect this package criticises twice. A comment reading
// "wait for postgres before go test runs" above the extensions step's `for`
// loop otherwise armed the plainness check on a block that cannot hide
// anything.
func mentionsGoTest(run string) bool {
	for _, raw := range strings.Split(run, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "go test") {
			return true
		}
	}
	return false
}

// runBlockIsPlain reports whether a `run:` block is a plain sequence of
// commands, and names the first line that says otherwise.
//
// This is the gate failing CLOSED, and it is what makes the line-oriented scan
// trustworthy rather than merely plausible. `go test` as the first word of a
// line inside `if [ "$X" = 1 ]; then … fi`, a `while false` loop, a `case` arm,
// an uncalled function body, or below an `exit 0` is text that never runs, and
// no regex over lines can tell. Rather than pretend otherwise, refuse the whole
// block: this lane must run unconditionally, so a conditional in it is a defect
// whether or not it was meant as one.
//
// A legitimate future need for a conditional therefore has to update this gate
// deliberately. For a lane whose entire purpose is that it always executes,
// that friction is the feature.
func runBlockIsPlain(run string) (string, bool) {
	var heredocEnd string
	for _, raw := range strings.Split(joinContinuations(run), "\n") {
		line := strings.TrimSpace(raw)
		if heredocEnd != "" {
			if line == heredocEnd {
				heredocEnd = ""
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 && shellControlKeywords[strings.TrimSuffix(fields[0], ";")] {
			return line, false
		}
		// A function definition (with or without a space before the brace), or
		// a brace/subshell group opening a scope. `go(){ echo skip; }` shadows
		// the toolchain outright, so the no-space form matters.
		if strings.HasSuffix(line, "{") || strings.HasSuffix(line, "(") ||
			strings.Contains(line, "() {") || funcDefinition.MatchString(line) {
			return line, false
		}
		// A trailing connective makes the NEXT line part of this command, so
		// `true ||` above a `go test` line means the invocation never runs.
		if strings.HasSuffix(line, "&&") || strings.HasSuffix(line, "||") {
			return line, false
		}
		// Exit-status suppression: the shell equivalent of continue-on-error.
		if suppressors.MatchString(line) {
			return line, false
		}
		heredocEnd = heredocDelimiter(line)
	}
	return "", true
}

// goTestArgs extracts the argument string of every `go test` invocation in one
// `run:` block.
//
// Not a shell parser, and deliberately conservative: it counts a line only when
// the command is the FIRST word of a non-comment line outside a heredoc body.
// Every case it declines to count means fewer packages found, which reds the
// coverage gate -- the safe direction for a gate whose whole purpose is to
// refuse to be satisfied by something that never executes.
//
// Correct ONLY for a plain sequence of commands; runBlockIsPlain is what
// guarantees it is only ever asked about one.
func goTestArgs(run string) []string {
	var out []string
	var heredocEnd string

	for _, raw := range strings.Split(joinContinuations(run), "\n") {
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
		heredocEnd = heredocDelimiter(line)
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

	spec := laneSpec{
		jobEnv:          job.Env,
		jobIf:           job.If,
		continueOnError: job.ContinueOnError,
		workingDir:      strings.TrimSpace(job.Defaults.Run.WorkingDirectory),
	}
	for _, step := range job.Steps {
		// Only blocks that MENTION `go test` are policed. The fail-closed
		// check exists to stop a `go test` hiding inside a construct, and the
		// lane's "create required Postgres extensions" step legitimately loops
		// over pg_isready -- refusing that would red the lane over a shape that
		// cannot hide anything.
		if !mentionsGoTest(step.Run) {
			continue
		}
		if offending, ok := runBlockIsPlain(step.Run); !ok {
			spec.nonPlainRun = append(spec.nonPlainRun, offending)
			continue
		}
		args := goTestArgs(step.Run)
		if len(args) == 0 {
			// The block mentions `go test` -- checked above -- but not as the
			// first word of its own line, so the scanner cannot read its
			// packages. Record it: reporting this as "no go test step" would
			// send a maintainer looking for a step that is plainly right
			// there. `set -e; go test …` and `GOFLAGS=… go test …` both land
			// here, and both are legitimate; the gate asks for the invocation
			// on its own line rather than trying to parse around them, because
			// accepting `X && go test` would also accept `false && go test`.
			spec.unparsedRun = append(spec.unparsedRun, strings.TrimSpace(step.Run))
			continue
		}
		if wd := strings.TrimSpace(step.WorkingDirectory); wd != "" {
			spec.workingDir = wd
		}
		gs := goTestStep{ifCond: step.If, env: step.Env, continueOnError: step.ContinueOnError}
		for _, a := range args {
			fields := strings.Fields(a)
			for i := 0; i < len(fields); i++ {
				f := fields[i]
				// A value-taking flag consumes the next argument, which is
				// therefore not a package even when it starts with "./".
				if valueTakingFlags[f] && i+1 < len(fields) {
					gs.flags = append(gs.flags, f, fields[i+1])
					i++
					continue
				}
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

	// THE INVERSE (memql#3095). The arm above is provisioned -> in selector.
	// This is in selector -> provisioned, and without it the gate was
	// one-directional: every main_dbschema_test.go in the tree could be
	// deleted while the selector still named the packages, and this gate
	// stayed fully green. Proved by deleting all seven and running it.
	//
	// What it prevents is the memql#2551 lane-safety invariant those TestMains
	// exist to satisfy. Without one, the per-package test binaries race the
	// shared migration and the lane fails INTERMITTENTLY with
	// `relation "MemoryNodes" does not exist` -- an intermittent red being the
	// worst shape of failure this lane can produce, and the exact flake
	// component/database/dbtest was written to kill.
	//
	// Keyed on the directories that actually CARRY db-gated tests, not on the
	// selector arguments: a selector may legitimately name a parent path, and
	// only a directory with db-gated tests needs to provision.
	provisionedSet := map[string]bool{}
	for _, dir := range provisioned {
		provisionedSet[dir] = true
	}
	for _, dir := range known {
		// selfPkg imports dbtest for the RequireDB predicate rather than to
		// reach a database, so it neither needs nor has a TestMain. The
		// exemption that hides it from the scan has to apply here too, or this
		// rule reports the gate against itself.
		if dir == selfPkg {
			continue
		}
		if !covers(pkgs, dir) {
			continue // not run by the lane; the arm above owns that case
		}
		if provisionedSet[dir] {
			continue
		}
		out = append(out, fmt.Sprintf("package %q carries db-gated tests and the %q job's "+
			"selector %v RUNS it, but it has no TestMain calling dbtest.EnsureSchema. Without one "+
			"its test binary races the shared migration instead of provisioning it, so the lane "+
			"fails intermittently with `relation \"MemoryNodes\" does not exist` -- the flake "+
			"component/database/dbtest exists to kill (memql#2551, memql#3095). Add a "+
			"main_dbschema_test.go to %s.", dir, dbTestsJob, pkgs, dir))
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
		switch {
		case len(lane.nonPlainRun) > 0:
			t.Fatalf("the %q job's only `go test` block is not a plain sequence of commands "+
				"(first offending line: %q), so this gate refused to scan it. See "+
				"TestDBTestsLaneCannotPassWithoutRunning for why (memql#2886).",
				dbTestsJob, lane.nonPlainRun[0])
		case len(lane.unparsedRun) > 0:
			t.Fatalf("the %q job in %s runs `go test`, but not as the FIRST WORD of its own "+
				"line, so this gate cannot read which packages it runs:\n\n  %s\n\n"+
				"Put the invocation on its own line (`set -o pipefail` or an env assignment on "+
				"a preceding line is fine). The gate deliberately does not parse around `;` or "+
				"`&&`, because accepting `make deps && go test` would also accept "+
				"`false && go test` (memql#2886).", dbTestsJob, ciWorkflow, lane.unparsedRun[0])
		default:
			t.Fatalf("the %q job in %s has no `go test` step, so the env key it sets guards "+
				"nothing (memql#2886).", dbTestsJob, ciWorkflow)
		}
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
	for _, f := range laneRunFindings(loadDBTestsJob(t, repoRoot(t))) {
		t.Error(f)
	}
}

// laneRunFindings returns one message per way the lane could report a
// non-failure while executing nothing.
//
// Pure, and separate from the test that calls it, so every check below can be
// table-tested against a synthetic laneSpec. When these lived inline they were
// only ever evaluated against the real ci.yml -- which is correct -- so
// deleting any one of them reded nothing at all. An assertion no test can
// distinguish from its own absence is the same defect this package exists to
// catch, one level up.
func laneRunFindings(lane laneSpec) []string {
	var out []string

	if !jobIfIsPathRouting(lane.jobIf) {
		out = append(out, fmt.Sprintf("the %q job's `if:` is not path routing:\n\n  %s\n\n"+
			"A job-level condition decides whether the lane runs at all, and ci-required treats "+
			"a SKIPPED job as a pass -- so a constant-false or mis-typed condition disables the "+
			"db-gated suites with no visible failure (memql#2886).", dbTestsJob, lane.jobIf))
	}
	if !isFalsy(lane.continueOnError) {
		out = append(out, fmt.Sprintf("the %q job sets continue-on-error: %v -- the lane could "+
			"fail without failing the build, and ci-required would still report green "+
			"(memql#2886).", dbTestsJob, lane.continueOnError))
	}
	if lane.workingDir != "" {
		out = append(out, fmt.Sprintf("the %q job sets working-directory: %q.\n\n"+
			"That relocates every relative package argument without appearing in the shell at "+
			"all, so the selector this gate reads is not the set of packages that actually run "+
			"(memql#2886).", dbTestsJob, lane.workingDir))
	}
	for _, line := range lane.nonPlainRun {
		out = append(out, fmt.Sprintf("a `run:` block in the %q job is not a plain sequence of "+
			"commands:\n\n  %s\n\n"+
			"This gate refuses to scan such a block rather than guess: `go test` can be the "+
			"first word of a line that never executes, and an exit status can be suppressed by "+
			"`|| true`, backgrounding, or an unguarded pipe. This lane must run "+
			"UNCONDITIONALLY and must fail when the suite fails (memql#2886).", dbTestsJob, line))
	}
	for i, s := range lane.steps {
		if strings.TrimSpace(s.ifCond) != "" {
			out = append(out, fmt.Sprintf("`go test` step %d of the %q job carries `if: %s` -- a "+
				"step condition can skip the suite while the job still succeeds (memql#2886).",
				i, dbTestsJob, s.ifCond))
		}
		if !isFalsy(s.continueOnError) {
			out = append(out, fmt.Sprintf("`go test` step %d of the %q job sets "+
				"continue-on-error: %v (memql#2886).", i, dbTestsJob, s.continueOnError))
		}
		if raw, ok := lane.effectiveEnv(s, "GOFLAGS"); ok {
			if f := goflagsDisablesTests(raw); f != "" {
				out = append(out, fmt.Sprintf("`go test` step %d of the %q job resolves "+
					"GOFLAGS=%q, which carries %q. GOFLAGS reaches the toolchain without "+
					"appearing on the command line (memql#2886).",
					i, dbTestsJob, fmt.Sprintf("%v", raw), f))
			}
		}
		if f := s.zeroExecutionFlag(); f != "" {
			out = append(out, fmt.Sprintf("`go test` step %d of the %q job passes %q, which can "+
				"make the suite exit 0 having run NOTHING. Note -count=0 is a one-character "+
				"edit to the -count=1 already there (memql#2886).", i, dbTestsJob, f))
		}
	}
	return out
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
	lane := loadDBTestsJob(t, root)
	pkgs := lane.pkgs()

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
