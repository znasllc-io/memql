package ci

// pipelines_parity_test.go -- the pipelines leaf (component/pipelines, epic
// memql#5477) ports three of the CI bridge's readers, so a pipeline selects
// paths, splits shards and reads test times exactly as the bridge does: a
// path bucket moved from ci.yml into a manifest must mean what it meant, and
// a timing table written by one must balance shards for the other. The leaf
// imports the standard library only, so it cannot test itself against the
// bridge. The root module can, and this file does.
//
// Only SINGLE globs are compared. A list of globs means different things on
// purpose: paths-filter ORs a bucket's patterns, so a `!` pattern widens it,
// while a manifest's PathSet reads `!` as "except" (component/pipelines,
// glob.go).

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/scripts/ci/selection"
)

// pipelinesExtraGlobs are constructs the grammar admits that ci.yml's own
// buckets do not happen to use, so the sweep covers the grammar and not only
// today's workflow.
var pipelinesExtraGlobs = []string{
	"**",
	"*",
	"*.go",
	"*.*",
	"**/*_test.go",
	"**/testdata/**",
	"**/.*",
	".github/**",
	"a/**/b.go",
	"component/*/testdata/**",
	"docs/*.md",
	"dsl/**",
	"!docs/**",
	"!**/*.md",
	"scripts/ci/*.go",
	"**/go.mod",
	"go.*",
}

// pipelinesSyntheticPaths are paths the tracked tree may not hold, chosen for
// the edges of the grammar: the root, dot directories, a name that is a
// prefix of another, a directory named like a file.
var pipelinesSyntheticPaths = []string{
	"a.md", "x/y/a.md", "a.mdx", ".a.md", "x/.a.md",
	"dsl", "dsl/a/b.memql", "dslx/a", "x/dsl/a",
	"a/b.go", "a/x/y/b.go", "ab.go", "a/x/c.go",
	"go.mod", "sub/go.mod", "sub/go.mod.bak",
	"docs", "docs/a.md", "docs/x/a.md",
	"component/pipelines/testdata/github/push.json", "component/testdata/a",
}

// Every pattern either matcher is asked about, over every tracked path: the
// two agree on what they admit, on what they refuse, and on every match.
func TestPipelinesCompileGlobAgreesWithPathsFilter(t *testing.T) {
	raw, err := ReadCIWorkflow()
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	buckets, err := ParseChangesFilters(raw)
	if err != nil {
		t.Fatal(err)
	}
	patterns := map[string]bool{}
	for _, list := range buckets {
		for _, p := range list {
			patterns[p] = true
		}
	}
	for _, p := range pipelinesExtraGlobs {
		patterns[p] = true
	}
	paths := append(trackedFiles(t), pipelinesSyntheticPaths...)

	compared, matches := 0, 0
	for _, pattern := range SortedKeys(patterns) {
		bridge, berr := CompilePattern(pattern)
		leaf, lerr := pipelines.CompileGlob(pattern)
		if (berr == nil) != (lerr == nil) {
			t.Errorf("pattern %q: CompilePattern error %v, CompileGlob error %v", pattern, berr, lerr)
			continue
		}
		if berr != nil {
			continue
		}
		compared++
		var disagree []string
		for _, p := range paths {
			b := bridge(p)
			if b {
				matches++
			}
			if leaf(p) != b {
				disagree = append(disagree, p)
			}
		}
		if len(disagree) > 0 {
			t.Errorf("pattern %q: the matchers disagree on %d path(s), first %q (CompilePattern says %t)",
				pattern, len(disagree), disagree[0], bridge(disagree[0]))
		}
	}
	// A sweep that compared nothing would pass the same way.
	if compared < 50 || matches == 0 {
		t.Fatalf("compared %d pattern(s) with %d match(es) between them: too little to vouch for anything", compared, matches)
	}
	t.Logf("%d patterns agree over %d paths (%d matches)", compared, len(paths), matches)
}

// What the bridge refuses the port refuses, with one exception it adds on
// purpose: `?`, one rune that is not a separator.
func TestPipelinesCompileGlobRefusesWhatPathsFilterRefuses(t *testing.T) {
	for _, pattern := range []string{"", "!", "{a,b}", "a/{b,c}/**", "[ab].go", "a**b/c", "a/!b", "@(x)", "+(x)", `a\b`} {
		if _, err := CompilePattern(pattern); err == nil {
			t.Fatalf("CompilePattern(%q) accepts it; this list is of patterns the bridge refuses", pattern)
		}
		if _, err := pipelines.CompileGlob(pattern); err == nil {
			t.Errorf("CompileGlob(%q) accepts a pattern the bridge refuses", pattern)
		}
	}
	if _, err := CompilePattern("a?.go"); err == nil {
		t.Fatal(`CompilePattern("a?.go") accepts "?"; the port's one addition is no longer one`)
	}
	match, err := pipelines.CompileGlob("a?.go")
	if err != nil {
		t.Fatalf(`CompileGlob("a?.go"): %v`, err)
	}
	if !match("ab.go") || match("a/.go") || match("abc.go") {
		t.Error(`CompileGlob("a?.go") does not read "?" as one rune that is not a separator`)
	}
}

// The bridge's Partition for one default class is the leaf's Partition:
// measured packages longest first into the lightest shard, then the
// unmeasured by name, ties to the lower index.
func TestPipelinesPartitionAgreesWithTheCISelector(t *testing.T) {
	type partitionCase struct {
		name    string
		dirs    []string
		seconds map[string]float64
		n       int
	}
	cases := []partitionCase{
		{"measured", []string{"a", "b", "c", "d", "e", "f", "g"},
			map[string]float64{"a": 7, "b": 30, "c": 12, "d": 4, "e": 19, "f": 25, "g": 1}, 3},
		// The two-phase order: an unmeasured package is placed after every
		// measured one, not in among them at its nominal weight.
		{"unmeasured after measured", []string{"a", "b", "c"}, map[string]float64{"a": 3, "b": 3}, 2},
		{"ties by name and by index", []string{"p", "q", "r", "s", "t"},
			map[string]float64{"p": 5, "q": 5, "r": 5, "s": 5, "t": 5}, 3},
		{"more shards than packages", []string{"x", "y", "z"}, map[string]float64{"x": 1, "y": 2}, 8},
		{"the root package", []string{".", "cmd/tool", "internal/x"}, map[string]float64{".": 53, "cmd/tool": 3}, 2},
		{"one shard", []string{"b", "a", "c"}, map[string]float64{"a": 9}, 1},
	}
	// And the bridge's own timing table, with packages it has not measured.
	f, err := os.Open(filepath.Join(RepoRoot(), "scripts", "ci", "shard-timings.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	table, err := selection.ReadTimings(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, lane := range []string{"go", "db"} {
		dirs := append(SortedKeys(table[lane]), "pipelines/new-one", "pipelines/new-two")
		if len(dirs) < 10 {
			t.Fatalf("the timing table's %s lane holds %d packages: too few to be the real table", lane, len(dirs))
		}
		for _, n := range []int{4, 8} {
			cases = append(cases, partitionCase{"timing table " + lane, dirs, table[lane], n})
		}
	}

	const prefix = "example.test/shop"
	importPath := func(dir string) string {
		if dir == "." {
			return prefix
		}
		return prefix + "/" + dir
	}
	for _, c := range cases {
		classes := []selection.Class{{Lane: "go", Name: "all", Timeout: "600s", Shards: c.n}}
		shards, err := selection.Partition("go", c.dirs, classes, c.seconds, 1)
		if err != nil {
			t.Fatalf("%s (n=%d): the bridge refuses the case: %v", c.name, c.n, err)
		}
		var want [][]string
		for _, s := range shards {
			var shard []string
			for _, d := range s.Dirs {
				shard = append(shard, importPath(d))
			}
			want = append(want, shard)
		}
		var packages []string
		seconds := map[string]float64{}
		for _, d := range c.dirs {
			packages = append(packages, importPath(d))
			if s, ok := c.seconds[d]; ok {
				seconds[importPath(d)] = s
			}
		}
		if got := pipelines.Partition(packages, seconds, c.n); !reflect.DeepEqual(got, want) {
			t.Errorf("%s (n=%d):\n leaf   %v\n bridge %v", c.name, c.n, got, want)
		}
	}
}

// Both readers take the same anchored result line, and a package reported
// more than once gets the median of its times, as the bridge's table refresh
// reads repeated samples.
func TestPipelinesParseGoTestOutputAgreesWithTheCISelector(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join(RepoRoot(), "scripts", "ci", "selection", "testdata", "go-test-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	runnerLog := strings.Join([]string{
		"ok  \texample.test/memql/a\t1.5s\tcoverage: 40.0% of statements\r",
		"ok  \texample.test/memql/b\t2s",
		"2026-10-03T09:21:44.5Z ok  \texample.test/memql/c\t0.25s",
		"ok  \texample.test/memql/c\t0.75s",
		"ok  \texample.test/memql/c\t0.5s",
		"ok  \texample.test/memqlx\t1s",
		"ok  \texample.test/memql\t3.25s",
		"FAIL\texample.test/memql/d\t9.9s",
		"    ok  \texample.test/memql/e\t1.0s",
	}, "\n")

	const prefix = "example.test/memql"
	for name, input := range map[string]string{"the bridge's fixture": string(fixture), "a runner's log": runnerLog} {
		samples, err := selection.ParseGoTestOutput(strings.NewReader(input), prefix)
		if err != nil {
			t.Fatalf("%s: bridge: %v", name, err)
		}
		want := map[string]float64{}
		for dir, xs := range samples {
			want[dir] = pipelinesMedian(xs)
		}
		read, err := pipelines.ParseGoTestOutput(strings.NewReader(input))
		if err != nil {
			t.Fatalf("%s: leaf: %v", name, err)
		}
		got := map[string]float64{}
		for importPath, secs := range read {
			if importPath != prefix && !strings.HasPrefix(importPath, prefix+"/") {
				continue
			}
			dir := strings.TrimPrefix(strings.TrimPrefix(importPath, prefix), "/")
			if dir == "" {
				dir = "."
			}
			got[dir] = secs
		}
		if len(want) == 0 {
			t.Fatalf("%s: the bridge read no times, so agreeing with it proves nothing", name)
		}
		if !pipelinesSameTimes(got, want) {
			t.Errorf("%s:\n leaf   %v\n bridge %v", name, got, want)
		}
	}
}

// The leaf's copy of the bridge's fixture is the bridge's fixture: two copies
// that drift would each pass their own tests and agree with nothing.
func TestPipelinesTimingFixtureIsTheSelectorsFixture(t *testing.T) {
	bridge, err := os.ReadFile(filepath.Join(RepoRoot(), "scripts", "ci", "selection", "testdata", "go-test-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := os.ReadFile(filepath.Join(RepoRoot(), "component", "pipelines", "testdata", "go-test-output.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bridge, leaf) {
		t.Error("component/pipelines/testdata/go-test-output.txt differs from scripts/ci/selection/testdata/go-test-output.txt; copy the bridge's file over the leaf's")
	}
}

func pipelinesMedian(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	mid := len(s) / 2
	if len(s)%2 == 1 {
		return s[mid]
	}
	return (s[mid-1] + s[mid]) / 2
}

func pipelinesSameTimes(got, want map[string]float64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok || math.Abs(g-w) > 1e-9 {
			return false
		}
	}
	return true
}
