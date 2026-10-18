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
// reads only native `pipeline.stages[].steps[].run`.
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
// is pinned separately by TestClusterE2ECompileLaneGatesTheMerge -- which also
// holds every file of the package to the lane's own tag sets. An exclusion
// excuses a file that needs the excluded tag and others besides
// (needsExcludedTag), so without that hold such a file could be compiled by
// nothing at all (memql#5497).
//
// Untagged on purpose: this must run in the default lane, because it is what
// catches the tags that do not.

import (
	"go/build"
	"go/build/constraint"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/component/pipelines"
)

// deliberatelyNotRunInCI maps a build tag to WHY no CI lane runs its tests.
//
// An entry is a considered decision, not a parking space: each needs a reason
// that would survive review. A tag that is merely inconvenient belongs in a
// lane.
var deliberatelyNotRunInCI = map[string]string{
	"clustere2e": "needs a provisioned cluster to run (make cluster-e2e for the 2-replica parity suite); the native build-clustere2e step " +
		"compiles and vets it under the tag so the package cannot rot uncompiled (memql#4212), which is not a run. One test of the " +
		"package, TestPipelinesSubstrate, does run: install-cluster-e2e.yml's pipelines leg runs it against the cluster that leg " +
		"installs (memql#5497) -- nightly and on that workflow's own paths, so it gates no pull request and is not a lane here, " +
		"and the tag stays out of the node-tag passes. That test also needs the agent's build (`//go:build clustere2e && agent`: " +
		"it runs the executor's fleet half, which is agent-only), so the compile lane builds the package under both tag sets",
}

// goTestCmd matches a `go test` invocation. Applied only to the text of a
// step's `run:` block, never to the whole file.
var goTestCmd = regexp.MustCompile(`(?m)^\s*go test\b([^\n]*)`)

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

// mergeSteps reads only unconditional native commands that run on BOTH PRs
// and merge candidates. A scheduled/release-only command, a path-gated step,
// or a selected subset of packages cannot establish coverage for every merge.
func mergeSteps(spec *pipelines.Spec) map[string]pipelines.StepSpec {
	out := map[string]pipelines.StepSpec{}
	for _, stage := range spec.Stages {
		if len(stage.On) > 0 && !(slices.Contains(stage.On, "pull_request") && slices.Contains(stage.On, "merge_group")) {
			continue
		}
		for _, step := range stage.Steps {
			if step.When == nil && step.Packages == "" && step.Run != "" {
				out[pipelines.StepKey(stage.Name, step.Name)] = step
			}
		}
	}
	return out
}

func nativeSpec(t *testing.T, root string) *pipelines.Spec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "memql-package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Pipeline *pipelines.Spec `yaml:"pipeline"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if refusal := pipelines.Validate(manifest.Pipeline); refusal != nil {
		t.Fatal(refusal)
	}
	return manifest.Pipeline
}

// lanesFromSteps counts actual commands, never names or comments. Restricted
// -run/-skip invocations do not establish coverage of every tagged test.
func lanesFromSteps(steps map[string]pipelines.StepSpec) []lane {
	var out []lane
	for name, step := range steps {
		if !strings.Contains(step.Run, "set -eu") || strings.Contains(step.Run, "||") {
			continue
		}
		for _, cmd := range goTestCmd.FindAllStringSubmatch(step.Run, -1) {
			args := cmd[1]
			m := tagsFlag.FindStringSubmatch(args)
			if m == nil {
				continue
			}
			var pkgs []string
			compileOnly, filtered := false, false
			for _, f := range strings.Fields(args) {
				if strings.HasPrefix(f, "./") || f == "..." {
					pkgs = append(pkgs, f)
				}
				if f == "-c" {
					compileOnly = true
				}
				if f == "-run" || strings.HasPrefix(f, "-run=") || f == "-skip" || strings.HasPrefix(f, "-skip=") {
					filtered = true
				}
			}
			if !filtered {
				out = append(out, lane{tags: strings.Split(m[1], ","), pkgs: pkgs, workflow: name, compileOnly: compileOnly})
			}
		}
	}
	return out
}

func ciLanes(t *testing.T, root string) []lane {
	t.Helper()
	return lanesFromSteps(mergeSteps(nativeSpec(t, root)))
}

func TestOnlyMergeGatingNativeCommandsCountAsTagCoverage(t *testing.T) {
	command := "set -eu\ngo test -tags agent ./app/"
	spec := &pipelines.Spec{Stages: []pipelines.StageSpec{
		{Name: "checks", Steps: []pipelines.StepSpec{
			{Name: "real", Run: command},
			{Name: "label-go-test-tags-agent", Run: "set -eu\n# go test -tags agent ./app/\necho ok"},
			{Name: "conditional", Run: command, When: &pipelines.When{Bucket: "go"}},
			{Name: "selected", Run: command, Packages: "affected"},
			{Name: "filtered", Run: command + " -run TestOne"},
			{Name: "swallowed", Run: command + " || true"},
			{Name: "compile", Run: command + " -c"},
		}},
		{Name: "scheduled", On: []string{"schedule"}, Steps: []pipelines.StepSpec{{Name: "hidden", Run: command}}},
		{Name: "pr-only", On: []string{"pull_request"}, Steps: []pipelines.StepSpec{{Name: "hidden", Run: command}}},
		{Name: "both", On: []string{"pull_request", "merge_group"}, Steps: []pipelines.StepSpec{{Name: "real", Run: command}}},
	}}
	lanes := lanesFromSteps(mergeSteps(spec))
	got := map[string]bool{}
	for _, lane := range lanes {
		got[lane.workflow] = lane.compileOnly
	}
	want := map[string]bool{"checks.real": false, "checks.compile": true, "both.real": false}
	if !maps.Equal(got, want) {
		t.Fatalf("merge-gating lanes = %v, want %v", got, want)
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
		if needsExcludedTag(t, dir, name, tag) {
			return reason, true
		}
	}
	return "", false
}

// needsExcludedTag reports whether the file compiles once tag is supplied:
// alone, or with the other tags its own constraint names. The second half is
// for a file that needs an excluded tag AND a node type's build
// (`//go:build clustere2e && agent`, memql#5497). Such a file needs what the
// exclusion is about -- a cluster -- whatever else it needs, and it compiles
// under no single tag. MatchFile still gives the verdict; the constraint is
// read only to learn which tags to supply.
func needsExcludedTag(t *testing.T, dir, name, tag string) bool {
	t.Helper()
	if buildsUnder(t, dir, name, []string{tag}) {
		return true
	}
	named := constraintTags(t, dir, name)
	return slices.Contains(named, tag) && buildsUnder(t, dir, name, named)
}

// constraintTags is every tag the file's build constraint names outside a
// negation: the tags a build would set to compile it. The constraint is read
// from every line before the package clause, so one placed after a licence
// header is found, in either spelling.
func constraintTags(t *testing.T, dir, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Join(dir, name), err)
	}
	var tags []string
	var collect func(e constraint.Expr, negated bool)
	collect = func(e constraint.Expr, negated bool) {
		switch x := e.(type) {
		case *constraint.TagExpr:
			if !negated && !slices.Contains(tags, x.Tag) {
				tags = append(tags, x.Tag)
			}
		case *constraint.NotExpr:
			collect(x.X, !negated)
		case *constraint.AndExpr:
			collect(x.X, negated)
			collect(x.Y, negated)
		case *constraint.OrExpr:
			collect(x.X, negated)
			collect(x.Y, negated)
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) && !constraint.IsPlusBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			t.Fatalf("%s: the build constraint %q does not parse: %v", filepath.Join(dir, name), line, err)
		}
		collect(expr, false)
	}
	return tags
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
		t.Fatal("parsed no `go test -tags` lanes from the native PR/merge candidate pipeline. Either the tagged " +
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
			"memql-package.yaml, or add its tag to deliberatelyNotRunInCI with a reason that "+
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
			if !buildsUnder(t, f[0], f[1], nil) && needsExcludedTag(t, f[0], f[1], tag) {
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

// tagSet is the tags one command's -tags flag names, nil for none.
func tagSet(cmdArgs string) []string {
	m := tagsFlag.FindStringSubmatch(cmdArgs)
	if m == nil {
		return nil
	}
	return strings.Split(m[1], ",")
}

var (
	goVetCmd = regexp.MustCompile(`(?m)^\s*go vet\b([^\n]*)`)
	flagC    = regexp.MustCompile(`(^|\s)-c(\s|$)`)
)

// TestClusterE2ECompileLaneGatesTheMerge pins the memql#4212 compile lane.
//
// The live suite cannot run here, so deliberatelyNotRunInCI excuses it from
// the gate above -- and a compile-only lane is excluded from that gate by
// design. That leaves the lane itself unguarded: delete the job, drop `-tags`,
// point it at another package, let it run in workspace mode, or leave it out
// of the native PR/merge candidate plan, and every other test in this package stays green while the
// package goes back to rotting. Each of those edits is checked here.
//
// So is the one the lane's own discovery cannot see. The job is FOUND by any
// vet and any `-c` step carrying the tag, and a file of the package can need
// more tags than that: test/clustere2e/pipelines_substrate_test.go is
// `//go:build clustere2e && agent` (memql#5497). The coverage gate excuses
// such a file through needsExcludedTag, so if the steps that name its tags
// were deleted, the lane would still be found, every gate would stay green,
// and no pull request would compile the file. So every *_test.go of the
// package must build under the tags of at least one `-c` step AND of at least
// one vet step of this job.
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
	steps := mergeSteps(nativeSpec(t, root))
	var found []string
	for name, step := range steps {
		vets, compiles := false, false
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
		if vets && compiles {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one unconditional native PR/merge candidate command that compiles and vets %s, got %v", dir, found)
	}
	jobName := found[0]
	step := steps[jobName]

	// Every file of the package, compiled and vetted under a tag set the job
	// itself uses (see the doc comment).
	var vetSets, compileSets [][]string
	{
		for _, cmd := range goVetCmd.FindAllStringSubmatch(step.Run, -1) {
			if laneCovers(cmd[1], tag, dir) {
				vetSets = append(vetSets, tagSet(cmd[1]))
			}
		}
		for _, cmd := range goTestCmd.FindAllStringSubmatch(step.Run, -1) {
			if laneCovers(cmd[1], tag, dir) && flagC.MatchString(cmd[1]) {
				compileSets = append(compileSets, tagSet(cmd[1]))
			}
		}
	}
	pkgDir := filepath.Join(root, filepath.FromSlash(dir))
	pkgFiles := 0
	for _, f := range testFiles(t, pkgDir) {
		if f[0] != pkgDir {
			continue // a package below this one is not what `./test/clustere2e/` compiles
		}
		pkgFiles++
		for _, half := range []struct {
			verb, flag string
			sets       [][]string
		}{{"compiles", "go test -c", compileSets}, {"vets", "go vet", vetSets}} {
			reached := false
			for _, set := range half.sets {
				if buildsUnder(t, f[0], f[1], set) {
					reached = true
					break
				}
			}
			if !reached {
				t.Errorf("%s/%s builds under none of the tag sets native step %q %s the package with (%v), so no pull "+
					"request %s it -- memql#4212's rot. Add a `%s -tags <the tags its build constraint needs>` step "+
					"for ./%s/ to the job.", dir, f[1], jobName, half.verb, half.sets, half.verb, half.flag, dir)
			}
		}
	}
	if pkgFiles == 0 {
		t.Fatalf("found no *_test.go in %s: the hold above checked nothing, so it fails closed", dir)
	}

	if !strings.Contains(step.Run, "export GOWORK=off\n") {
		t.Errorf("native step %q must compile outside the workspace, as the live suite does", jobName)
	}
	if !strings.Contains(step.Run, "set -eu\n") || strings.Contains(step.Run, "||") {
		t.Errorf("native step %q can swallow a failed compile or vet command", jobName)
	}
}
