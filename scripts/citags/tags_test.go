package citags

// tags_test.go -- the CI drift gate for BUILD-TAGGED TEST SUITES (memql#2903).
//
// The untagged `go test ./...` never RUNS a //go:build-tagged test file, so a
// tagged suite is CI-invisible unless some lane runs it with `-tags`. That fact
// used to live in a hand-maintained audit comment in .github/workflows/ci.yml,
// which had drifted on four tags at once: mcp was absent entirely (three files),
// and three more were covered by a closing "no other build tag carries test
// files today" that was false of each. It bit memql#2888 concretely: that
// vulnerability's seam is in an //go:build mcp file, so a regression test
// placed beside it would never have run.
//
// # Why go/build rather than a regex
//
// A first cut of this gate scanned for `//go:build` with a line regex and split
// the expression on non-identifier characters. Review demolished it, and the
// failures are worth recording because they are what this file must not
// regress into:
//
//   - blind to a constraint placed after a licence header (it stopped at the
//     first blank line), so a whole class of tagged file was invisible;
//   - blind to the legacy `// +build` form, still honoured by the toolchain;
//   - term-split rather than evaluated, so `!(a || b)` -- which builds by
//     DEFAULT -- demanded lanes for a and b, and `a || b` demanded both when
//     either suffices;
//   - no notion of GOOS/GOARCH/cgo, so `//go:build linux` would have demanded a
//     CI lane called "linux".
//
// go/build.Context.MatchFile answers the only question that matters -- "would
// THIS build configuration compile THIS file?" -- and gets constraint
// placement, `// +build`, boolean evaluation and platform tags for free.
//
// # Why YAML rather than a regex
//
// That same first cut grepped ci.yml as raw text. Four of its eight matches
// were step `name:` lines: the gate was satisfied by a LABEL, and deleting
// every `run:` line still passed. It could not tell a lane from its title,
// which is exactly the failure it exists to catch. This parses the YAML and
// reads only `jobs.*.steps[].run`.
//
// # Why package lists, not just tags
//
// Matching tag -> workflow says nothing about whether the lane runs the PACKAGE
// the tagged file lives in. A `//go:build mcp` test under component/grpc/ would
// have been "covered" by a lane running `-tags mcp ./app/ ./component/mcp/`
// while never executing. Now that every node tag has a lane, that is the most
// likely way this bug recurs, so the comparison is tag AND package.
//
// # Compile-only lanes are not coverage
//
// A `go test -tags X -c` step compiles a tagged package to a binary it never
// runs. That is worth having -- test/clustere2e did not even BUILD for a
// stretch before memql#4201, and nothing in CI could tell (memql#4212) -- but
// it asserts nothing about behaviour, so it must not satisfy this gate: a
// `-c` step would otherwise be the cheapest way to make any tag "covered".
// ciLanes marks such a lane compileOnly and the coverage walk skips it; the
// tag stays in deliberatelyNotRunInCI with its reason, and the compile lane
// is pinned separately by TestClusterE2ECompileLaneGatesTheMerge.
//
// Untagged on purpose: this must run in the default lane, because it is what
// catches the tags that do not.

import (
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// deliberatelyNotRunInCI maps a build tag to WHY no CI lane runs its tests.
//
// An entry is a considered decision, not a parking space: each needs a reason
// that would survive review. A tag that is merely inconvenient belongs in a
// lane.
var deliberatelyNotRunInCI = map[string]string{
	"clustere2e": "needs a provisioned 2-replica parity cluster to run (make cluster-e2e); ci.yml's build-clustere2e job compiles and vets it under the tag so the package cannot rot uncompiled (memql#4212), which is not a run",
}

// prCriticalWorkflows are the workflow files whose lanes actually gate a pull
// request. A `go test -tags X` in a workflow that only runs on
// workflow_dispatch or a schedule does NOT protect a PR, so it must not satisfy
// this gate.
var prCriticalWorkflows = map[string]bool{"ci.yml": true}

// goTestCmd matches a `go test` invocation. Applied only to the text of a
// step's `run:` block, never to the whole file.
var goTestCmd = regexp.MustCompile(`go test\b([^\n]*)`)

// tagsFlag pulls the tag list out of one command's arguments.
var tagsFlag = regexp.MustCompile(`-tags[= ]["']?([A-Za-z0-9_,]+)["']?`)

// lane is one `go test -tags ... <packages>` step.
type lane struct {
	tags     []string
	pkgs     []string
	workflow string
	// compileOnly marks a `go test -c` step: it builds the test binary and
	// never runs it, so it proves the package compiles and nothing else.
	compileOnly bool
}

// covers reports whether this lane's package arguments include dir, a
// repo-relative directory such as "app" or "component/mcp".
func (l lane) covers(dir string) bool {
	for _, p := range l.pkgs {
		p = strings.TrimPrefix(strings.TrimSpace(p), "./")
		p = strings.TrimSuffix(p, "/")
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

// workflowDoc is the sliver of the GitHub Actions schema this gate reads: each
// job's `run:` text, and its strategy.matrix so a matrix job's commands can be
// read per entry (memql#5483).
type workflowDoc struct {
	Jobs map[string]struct {
		Strategy struct {
			Matrix yaml.Node `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// matrixRef matches a `${{ matrix.<key> }}` interpolation, spaces optional.
var matrixRef = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}`)

// matrixCombos expands a job's STATIC strategy.matrix into its combinations:
// the cross product of its list-valued keys, plus each `include` entry. A
// matrix computed at run time (`${{ fromJSON(...) }}`) has nothing static to
// expand and yields none, so its steps are read as written -- and a
// `-tags ${{ matrix.tag }}` left unexpanded matches no tag, which is the safe
// direction: it cannot make a tagged file look covered.
func matrixCombos(m yaml.Node) []map[string]string {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	var keys []string
	values := map[string][]string{}
	var include []map[string]string
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i].Value, m.Content[i+1]
		switch {
		case k == "include" && v.Kind == yaml.SequenceNode:
			for _, entry := range v.Content {
				if entry.Kind != yaml.MappingNode {
					continue
				}
				combo := map[string]string{}
				for j := 0; j+1 < len(entry.Content); j += 2 {
					if entry.Content[j+1].Kind == yaml.ScalarNode {
						combo[entry.Content[j].Value] = entry.Content[j+1].Value
					}
				}
				include = append(include, combo)
			}
		case k == "exclude":
			// An excluded combination runs nothing, so dropping the
			// exclusion can only ADD lanes -- over-reporting coverage the
			// matrix does not have. No workflow here uses one; refuse to
			// guess rather than read one wrongly.
			panic("citags: strategy.matrix.exclude is not modelled -- teach matrixCombos before using it")
		case v.Kind == yaml.SequenceNode:
			keys = append(keys, k)
			for _, item := range v.Content {
				if item.Kind == yaml.ScalarNode {
					values[k] = append(values[k], item.Value)
				}
			}
		}
	}
	var combos []map[string]string
	if len(keys) > 0 {
		combos = []map[string]string{{}}
		for _, k := range keys {
			var next []map[string]string
			for _, base := range combos {
				for _, v := range values[k] {
					merged := map[string]string{k: v}
					for bk, bv := range base {
						merged[bk] = bv
					}
					next = append(next, merged)
				}
			}
			combos = next
		}
	}
	return append(combos, include...)
}

// renderRuns returns a step's `run:` text once per matrix combination with its
// `${{ matrix.<key> }}` references substituted, or the text as written when
// the job has no static matrix or the step names none of it.
func renderRuns(run string, combos []map[string]string) []string {
	if len(combos) == 0 || !matrixRef.MatchString(run) {
		return []string{run}
	}
	out := make([]string, 0, len(combos))
	for _, c := range combos {
		out = append(out, matrixRef.ReplaceAllStringFunc(run, func(ref string) string {
			if v, ok := c[matrixRef.FindStringSubmatch(ref)[1]]; ok {
				return v
			}
			return ref
		}))
	}
	return out
}

// lanesFromWorkflow returns every `go test -tags` invocation in one workflow,
// reading ONLY steps[].run, once per matrix entry for a matrix job.
func lanesFromWorkflow(name string, data []byte) ([]lane, error) {
	var wf workflowDoc
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	var out []lane
	for _, job := range wf.Jobs {
		combos := matrixCombos(job.Strategy.Matrix)
		for _, step := range job.Steps {
			for _, run := range renderRuns(step.Run, combos) {
				for _, cmd := range goTestCmd.FindAllStringSubmatch(run, -1) {
					args := cmd[1]
					m := tagsFlag.FindStringSubmatch(args)
					if m == nil {
						continue
					}
					var pkgs []string
					compileOnly := false
					for _, f := range strings.Fields(args) {
						if strings.HasPrefix(f, "./") || f == "..." {
							pkgs = append(pkgs, f)
						}
						if f == "-c" {
							compileOnly = true
						}
					}
					out = append(out, lane{tags: strings.Split(m[1], ","), pkgs: pkgs, workflow: name, compileOnly: compileOnly})
				}
			}
		}
	}
	return out, nil
}

// ciLanes parses the PR-critical workflows and returns every `go test -tags`
// step, reading ONLY steps[].run.
func ciLanes(t *testing.T, root string) []lane {
	t.Helper()
	var out []lane
	dir := filepath.Join(root, ".github", "workflows")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !prCriticalWorkflows[e.Name()] {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			t.Fatalf("reading %s: %v", e.Name(), readErr)
		}
		lanes, err := lanesFromWorkflow(e.Name(), data)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, lanes...)
	}
	return out
}

// TestMatrixEntriesAreReadAsLanes pins the expansion memql#5483 depends on: the
// tag passes are ONE matrix job, so without it every tagged file would read as
// uncovered -- and, worse, a regression here that expanded nothing would look
// exactly like that and be "fixed" by deleting the matrix.
func TestMatrixEntriesAreReadAsLanes(t *testing.T) {
	wf := []byte(`
jobs:
  tagged:
    strategy:
      matrix:
        include:
          - tag: agent
            packages: ./app/ ./integrations/agent/
          - tag: mcp
            packages: ./component/mcp/
    steps:
      - run: go test -tags ${{ matrix.tag }} -timeout=300s ${{ matrix.packages }}
  crossed:
    strategy:
      matrix:
        tag: [edge, bff]
    steps:
      - run: go test -tags ${{matrix.tag}} ./component/server/
  planned:
    strategy:
      matrix: ${{ fromJSON(needs.plan.outputs.go_matrix) }}
    steps:
      - run: go test -tags ${{ matrix.tag }} ${{ matrix.packages }}
`)
	lanes, err := lanesFromWorkflow("ci.yml", wf)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, l := range lanes {
		got[strings.Join(l.tags, ",")+" "+strings.Join(l.pkgs, " ")] = true
	}
	for _, want := range []string{
		"agent ./app/ ./integrations/agent/",
		"mcp ./component/mcp/",
		"edge ./component/server/",
		"bff ./component/server/",
	} {
		if !got[want] {
			t.Errorf("missing lane %q; got %v", want, got)
		}
	}
	if len(lanes) != 4 {
		t.Errorf("want exactly the 4 static lanes (a computed matrix expands to none), got %d: %v", len(lanes), got)
	}
}

// buildsUnder reports whether the file compiles under the given extra build
// tags, using the real toolchain matcher.
func buildsUnder(t *testing.T, dir, name string, tags []string) bool {
	t.Helper()
	ctx := build.Default
	ctx.BuildTags = tags
	ok, err := ctx.MatchFile(dir, name)
	if err != nil {
		t.Fatalf("MatchFile(%s, %s): %v", dir, name, err)
	}
	return ok
}

// testFiles walks the tree for *_test.go, skipping directories the Go toolchain
// itself ignores.
func testFiles(t *testing.T, root string) [][2]string {
	t.Helper()
	var out [][2]string
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
		if strings.HasSuffix(d.Name(), "_test.go") {
			out = append(out, [2]string{filepath.Dir(path), d.Name()})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	return out
}

// excludedReason reports whether the file is covered by a documented exclusion,
// i.e. it compiles once that excluded tag is supplied.
func excludedReason(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	for tag, reason := range deliberatelyNotRunInCI {
		if buildsUnder(t, dir, name, []string{tag}) {
			return reason, true
		}
	}
	return "", false
}

// TestEveryTaggedTestFileIsRunSomewhere is the gate.
//
// For every *_test.go that does NOT compile in the default build, some
// PR-critical lane must run it -- matching tags AND package -- or its tag must
// carry a documented reason.
func TestEveryTaggedTestFileIsRunSomewhere(t *testing.T) {
	root := repoRoot(t)
	files := testFiles(t, root)
	lanes := ciLanes(t, root)

	if len(files) == 0 {
		t.Fatal("found no *_test.go files at all -- the scanner is broken, not the tree")
	}
	if len(lanes) == 0 {
		t.Fatal("parsed no `go test -tags` lanes from the PR-critical workflows. Either the tagged " +
			"lanes were removed, or this gate's YAML parsing broke -- both are failures")
	}

	var uncovered []string
	for _, f := range files {
		dir, name := f[0], f[1]
		if buildsUnder(t, dir, name, nil) {
			continue // the untagged `go test ./...` already runs it
		}
		rel, _ := filepath.Rel(root, filepath.Join(dir, name))
		rel = filepath.ToSlash(rel)
		relDir := filepath.ToSlash(filepath.Dir(rel))

		covered := false
		for _, l := range lanes {
			if l.compileOnly {
				continue // compiles it; does not run it (see the header)
			}
			if buildsUnder(t, dir, name, l.tags) && l.covers(relDir) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		if reason, ok := excludedReason(t, dir, name); ok {
			t.Logf("%s -- deliberately not run: %s", rel, reason)
			continue
		}
		uncovered = append(uncovered, rel)
	}

	sort.Strings(uncovered)
	for _, rel := range uncovered {
		t.Errorf("%s does not compile in the default build and NO PR-critical CI lane runs it.\n\n"+
			"A //go:build-tagged test is invisible to the untagged `go test ./...`, so it asserts "+
			"nothing on any PR. Either add a `go test -tags <tag> ./%s/` step to "+
			".github/workflows/ci.yml, or add its tag to deliberatelyNotRunInCI with a reason that "+
			"would survive review (memql#2903).", rel, filepath.Dir(rel))
	}
}

// TestDeliberateExclusionsAreHonest keeps the exclusion list from rotting the
// other way: an entry whose files are gone, or whose reason is empty, reads as
// a considered decision and is not one.
func TestDeliberateExclusionsAreHonest(t *testing.T) {
	root := repoRoot(t)
	files := testFiles(t, root)

	tags := make([]string, 0, len(deliberatelyNotRunInCI))
	for tag := range deliberatelyNotRunInCI {
		tags = append(tags, tag)
	}
	sort.Strings(tags)

	for _, tag := range tags {
		if strings.TrimSpace(deliberatelyNotRunInCI[tag]) == "" {
			t.Errorf("deliberatelyNotRunInCI[%q] has an empty reason", tag)
		}
		found := false
		for _, f := range files {
			if !buildsUnder(t, f[0], f[1], nil) && buildsUnder(t, f[0], f[1], []string{tag}) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("deliberatelyNotRunInCI lists %q, but no tagged *_test.go requires it any more. "+
				"Remove the entry (memql#2903).", tag)
		}
	}
}

// ciJobDoc is the fuller slice of the workflow schema the compile-lane pin
// reads: job env + needs, and per-step name / run / if / continue-on-error.
type ciJobDoc struct {
	Jobs map[string]struct {
		If    string            `yaml:"if"`
		Needs any               `yaml:"needs"`
		Env   map[string]string `yaml:"env"`
		Steps []struct {
			Name            string `yaml:"name"`
			Run             string `yaml:"run"`
			If              string `yaml:"if"`
			ContinueOnError any    `yaml:"continue-on-error"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// laneCovers reports whether a `go vet` / `go test` command line carries the
// tag and names the package directory, reusing lane.covers for the package
// matching so the two gates cannot disagree about what "covers" means.
func laneCovers(cmdArgs, tag, dir string) bool {
	m := tagsFlag.FindStringSubmatch(cmdArgs)
	if m == nil {
		return false
	}
	hasTag := false
	for _, tg := range strings.Split(m[1], ",") {
		if tg == tag {
			hasTag = true
		}
	}
	if !hasTag {
		return false
	}
	var pkgs []string
	for _, f := range strings.Fields(cmdArgs) {
		if strings.HasPrefix(f, "./") || f == "..." {
			pkgs = append(pkgs, f)
		}
	}
	return lane{pkgs: pkgs}.covers(dir)
}

var (
	goVetCmd = regexp.MustCompile(`go vet\b([^\n]*)`)
	flagC    = regexp.MustCompile(`(^|\s)-c(\s|$)`)
)

// TestClusterE2ECompileLaneGatesTheMerge pins the memql#4212 compile lane.
//
// The live suite cannot run here, so deliberatelyNotRunInCI excuses it from
// the gate above -- and a compile-only lane is excluded from that gate by
// design. That leaves the lane itself unguarded: delete the job, drop `-tags`,
// point it at another package, let it run in workspace mode, or leave it out
// of ci-required, and every other test in this package stays green while the
// package goes back to rotting. Each of those edits is checked here.
//
// The lane runs with GOWORK=off because scripts/test/cluster-e2e.sh runs the
// live suite with GOWORK=off: under the workspace a module silently satisfies
// an import it does not require (see go.work), so a require the root go.mod
// lacks would compile green here and fail on the cluster.
func TestClusterE2ECompileLaneGatesTheMerge(t *testing.T) {
	const (
		tag = "clustere2e"
		dir = "test/clustere2e"
	)
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var wf ciJobDoc
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}

	// Find the job by what it DOES, not by its name, so a rename is free.
	var found []string
	for name, job := range wf.Jobs {
		vets, compiles := false, false
		for _, step := range job.Steps {
			for _, cmd := range goVetCmd.FindAllStringSubmatch(step.Run, -1) {
				if laneCovers(cmd[1], tag, dir) {
					vets = true
				}
			}
			for _, cmd := range goTestCmd.FindAllStringSubmatch(step.Run, -1) {
				if laneCovers(cmd[1], tag, dir) && flagC.MatchString(cmd[1]) {
					compiles = true
				}
			}
		}
		if vets && compiles {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	if len(found) == 0 {
		t.Fatalf("no ci.yml job both `go vet -tags %s ./%s/` and `go test -tags %s -c ... ./%s/`. "+
			"The tagged package is then compiled by nothing on any PR and rots silently, which is "+
			"exactly memql#4212.", tag, dir, tag, dir)
	}
	if len(found) > 1 {
		t.Fatalf("%d ci.yml jobs compile %s: %v -- one lane, so there is one place to read", len(found), dir, found)
	}
	jobName := found[0]
	job := wf.Jobs[jobName]

	if got := strings.TrimSpace(job.Env["GOWORK"]); got != "off" {
		t.Errorf("ci.yml job %q must set GOWORK: 'off' at job level (got %q): scripts/test/cluster-e2e.sh "+
			"runs the live suite with GOWORK=off, and a require the root go.mod lacks compiles green "+
			"under the workspace and fails on the cluster", jobName, got)
	}
	if !strings.Contains(strings.ReplaceAll(job.If, " ", ""), "needs.changes.outputs.go=='true'") {
		t.Errorf("ci.yml job %q is gated on `if: %s`, which does not fire on the `go` bucket; a tagged "+
			"compile can break on any Go change, so the lane must run whenever Go changed "+
			"(build-node-tags is the model)", jobName, job.If)
	}
	for _, step := range job.Steps {
		if !goVetCmd.MatchString(step.Run) && !goTestCmd.MatchString(step.Run) {
			continue
		}
		if strings.TrimSpace(step.If) != "" {
			t.Errorf("ci.yml job %q step %q is gated on `if: %s`; a condition that evaluates false compiles "+
				"nothing and reports green", jobName, step.Name, step.If)
		}
		if step.ContinueOnError != nil && step.ContinueOnError != false {
			t.Errorf("ci.yml job %q step %q sets continue-on-error=%v; a swallowed compile failure is not a gate",
				jobName, step.Name, step.ContinueOnError)
		}
	}

	// In ci-required's needs, or it runs without blocking anything (memql#3019).
	required, ok := wf.Jobs["ci-required"]
	if !ok {
		t.Fatal("no ci-required job in ci.yml; branch protection requires that check by name")
	}
	inNeeds := false
	switch n := required.Needs.(type) {
	case string:
		inNeeds = n == jobName
	case []any:
		for _, v := range n {
			if v == jobName {
				inNeeds = true
			}
		}
	}
	if !inNeeds {
		t.Errorf("ci.yml job %q is not in ci-required's `needs`, so a red compile does not block a merge", jobName)
	}
}
