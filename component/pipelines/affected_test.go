package pipelines

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func affectedOrFatal(t *testing.T, g *Graph, changed []string, fullGlobs ...string) Selection {
	t.Helper()
	facts, err := AnalyzeSelection(g, changed, true, fullGlobs)
	if err != nil {
		t.Fatalf("AnalyzeSelection(%v, %v): %v", changed, fullGlobs, err)
	}
	sel, err := ResolveSelection(g, facts, SelectionDecision{})
	if err != nil {
		t.Fatalf("ResolveSelection(%v): %v", changed, err)
	}
	return sel
}

func fullOrFatal(t *testing.T, g *Graph, changed []string, fullGlobs ...string) Selection {
	t.Helper()
	facts, err := AnalyzeSelection(g, changed, true, fullGlobs)
	if err != nil {
		t.Fatalf("AnalyzeSelection(%v, %v): %v", changed, fullGlobs, err)
	}
	sel, err := ResolveSelection(g, facts, SelectionDecision{Full: true, Reason: "full coverage selected by DSL"})
	if err != nil {
		t.Fatalf("ResolveSelection(%v): %v", changed, err)
	}
	return sel
}

var everyFixturePackage = []string{
	"example.test/shop",
	"example.test/shop/a",
	"example.test/shop/b",
	"example.test/shop/c",
	"example.test/shop/sub/client",
	"example.test/shop/tagged",
}

// The seed is the package a changed path belongs to; the selection is the
// seed plus everything that imports it, transitively. a reaches every
// package: b imports it, c's test imports b, and tagged, the root package and
// the nested module's client import it directly.
func TestAffectedSelectsTheSeedAndItsImportersTransitively(t *testing.T) {
	g := scanTree(t, true)
	cases := []struct {
		changed []string
		seeds   []string
		want    []string
	}{
		{[]string{"a/a.go"}, []string{"example.test/shop/a"}, everyFixturePackage},
		{[]string{"b/b.go"}, []string{"example.test/shop/b"}, []string{"example.test/shop/b", "example.test/shop/c"}},
		// A test-only change still seeds its package; nothing imports c.
		{[]string{"c/c_test.go"}, []string{"example.test/shop/c"}, []string{"example.test/shop/c"}},
		// Testdata is read by its package's tests.
		{[]string{"a/testdata/golden.json"}, []string{"example.test/shop/a"}, everyFixturePackage},
		// A non-Go file seeds the nearest package above it.
		{[]string{"docs/guide.md"}, []string{"example.test/shop"}, []string{"example.test/shop"}},
		{
			[]string{"tagged/tagged.go", "sub/client/client.go"},
			[]string{"example.test/shop/sub/client", "example.test/shop/tagged"},
			[]string{"example.test/shop/sub/client", "example.test/shop/tagged"},
		},
	}
	for _, c := range cases {
		sel := affectedOrFatal(t, g, c.changed)
		if sel.Full {
			t.Errorf("%v: Full (%s), want a narrowed selection", c.changed, sel.Reason)
			continue
		}
		if !reflect.DeepEqual(sel.Seeds, c.seeds) {
			t.Errorf("%v: Seeds = %v, want %v", c.changed, sel.Seeds, c.seeds)
		}
		if !reflect.DeepEqual(sel.Packages, c.want) {
			t.Errorf("%v: Packages = %v, want %v", c.changed, sel.Packages, c.want)
		}
		if sel.Reason == "" {
			t.Errorf("%v: a narrowed selection carries no reason", c.changed)
		}
	}
}

// The graph adapter applies a full-coverage decision uniformly. The actual
// policy that selects Full for these facts is exercised through the pinned
// DSL by component/pipelinerun.
func TestResolveSelectionAppliesFullDecisionToEveryPackage(t *testing.T) {
	g := scanTree(t, true)
	cases := []struct {
		name    string
		changed []string
	}{
		{"an empty change list", nil},
		{"the root go.mod", []string{"go.mod"}},
		{"a nested go.mod", []string{"docs/guide.md", "sub/go.mod"}},
		{"a go.sum anywhere", []string{"tools/go.sum"}},
		{"go.work", []string{"go.work"}},
		{"go.work.sum", []string{"go.work.sum"}},
		{"the manifest", []string{"memql-package.yaml"}},
		{"vendored code", []string{"vendor/example.test/dep/dep.go"}},
		{"a path outside the repository", []string{"../elsewhere/x.go"}},
		{"an absolute path", []string{"/a/a.go"}},
	}
	for _, c := range cases {
		sel := fullOrFatal(t, g, c.changed)
		if !sel.Full {
			t.Errorf("%s: selected %v, want Full", c.name, sel.Packages)
			continue
		}
		if sel.Reason == "" {
			t.Errorf("%s: full selection has no explanation", c.name)
		}
		if !reflect.DeepEqual(sel.Packages, everyFixturePackage) {
			t.Errorf("%s: a Full selection lists %v, want every package", c.name, sel.Packages)
		}
	}
}

// The manifest selects everything only at the root, where Deployables and
// pipelines read it; a file of that name elsewhere is an ordinary file.
func TestAffectedReadsTheManifestAtTheRootOnly(t *testing.T) {
	sel := affectedOrFatal(t, scanTree(t, true), []string{"b/memql-package.yaml"})
	if sel.Full {
		t.Fatalf("b/memql-package.yaml selected everything (%s)", sel.Reason)
	}
	if want := []string{"example.test/shop/b", "example.test/shop/c"}; !reflect.DeepEqual(sel.Packages, want) {
		t.Errorf("Packages = %v, want %v", sel.Packages, want)
	}
}

// A file that does not parse is reported as incomplete graph evidence; the
// DSL policy selects full coverage from that fact.
func TestSelectionFactsReportAnIncompleteGraph(t *testing.T) {
	g := scanTree(t, false)
	facts, err := AnalyzeSelection(g, []string{"docs/guide.md"}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if facts.GraphComplete || len(facts.Incomplete) != 1 || !strings.Contains(facts.Incomplete[0], "broken/x.go") {
		t.Errorf("incomplete graph facts = %v, want broken/x.go", g.Incomplete())
	}
}

// select.full is the pipeline's own list of paths whose change means
// everything, read as Validate admits it: PathSet, so a `!` glob excepts.
func TestAffectedSelectsEverythingOnASelectFullMatch(t *testing.T) {
	g := scanTree(t, true)
	full := []string{"docs/**", "!docs/drafts/**"}

	facts, err := AnalyzeSelection(g, []string{"a/a_test.go", "docs/guide.md"}, true, full)
	if err != nil || len(facts.Paths) != 2 || !facts.Paths[1].ConfiguredFull {
		t.Fatalf("configured full-match facts = %+v, %v", facts, err)
	}
	sel := fullOrFatal(t, g, []string{"a/a_test.go", "docs/guide.md"}, full...)
	if !sel.Full || sel.Reason == "" {
		t.Errorf("docs/guide.md under select.full %v: got %+v, want Full with a reason", full, sel)
	}
	// The excepted path narrows like any other file.
	sel = affectedOrFatal(t, g, []string{"docs/drafts/plan.md"}, full...)
	if sel.Full {
		t.Errorf("docs/drafts/plan.md is excepted by %q, yet selected everything (%s)", full[1], sel.Reason)
	}
}

// A select.full list Validate would refuse is an error, never a list that
// matches nothing: a full trigger silently narrowing a run is the failure
// selection must not have.
func TestAffectedRefusesASelectFullItCannotRead(t *testing.T) {
	g := scanTree(t, true)
	for _, full := range [][]string{{"docs/{a,b}/**"}, {"!docs/**"}} {
		if sel, err := AnalyzeSelection(g, []string{"docs/guide.md"}, true, full); err == nil {
			t.Errorf("select.full %v read as %+v, want an error", full, sel)
		}
	}
	if _, err := AnalyzeSelection(nil, []string{"a/a.go"}, true, nil); err == nil {
		t.Error("AnalyzeSelection over no graph answered without an error")
	}
}

func TestChangedPathFactsSupportBucketOnlyPipelines(t *testing.T) {
	facts, err := AnalyzeChangedPaths(
		[]string{"component/memql/engine.go", "vendor/example.test/dep/dep.go", "../outside"},
		true,
		[]string{"component/**", "!component/generated/**"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.Known || !facts.GraphComplete || facts.ChangedCount != 3 {
		t.Fatalf("path-only facts = %+v, want known complete facts for three paths", facts)
	}
	if got := facts.Paths[0]; !got.RepositoryPath || !got.ConfiguredFull || got.Vendored {
		t.Errorf("ordinary path facts = %+v, want valid configured-full path", got)
	}
	if got := facts.Paths[1]; !got.RepositoryPath || !got.Vendored {
		t.Errorf("vendor path facts = %+v, want a conservatively vendored path", got)
	}
	if got := facts.Paths[2]; got.RepositoryPath {
		t.Errorf("outside path facts = %+v, want invalid repository path", got)
	}
}

// deletedPackageTree is a tree at the head of a change that deleted package
// a, which b still imports: the reviewer's probe (epic memql#5477). Nothing
// owns a/a.go any more and no package lives at the root, so a selection that
// asked only "which package owns this path" would seed nothing, and every
// `packages: affected` step would skip green over a b that no longer builds.
//
// e imports the root package, which the change deleted too; tools is a
// nested module whose path does not follow its directory, and whose gen
// imports its own deleted lib; d imports only what is not the repository's,
// one of them a path sharing the module path's prefix without being under it.
func deletedPackageTree() fstest.MapFS {
	return fstest.MapFS{
		"go.mod":            {Data: []byte("module example.test/probe\n")},
		"b/b.go":            {Data: []byte("package b\n\nimport \"example.test/probe/a\"\n\nvar Name = a.Name\n")},
		"c/c.go":            {Data: []byte("package c\n\nimport \"example.test/probe/b\"\n\nvar Name = b.Name\n")},
		"d/d.go":            {Data: []byte("package d\n\nimport (\n\t\"fmt\"\n\n\t\"example.test/probes/lib\"\n\t\"github.com/other/lib\"\n)\n")},
		"e/e.go":            {Data: []byte("package e\n\nimport \"example.test/probe\"\n\nvar Name = probe.Name\n")},
		"f/f.go":            {Data: []byte("package f\n\nimport \"example.test/probe/e\"\n\nvar Name = e.Name\n")},
		"tools/go.mod":      {Data: []byte("module example.test/devtools\n")},
		"tools/gen/main.go": {Data: []byte("package main\n\nimport \"example.test/devtools/lib\"\n\nfunc main() { lib.Run() }\n")},
	}
}

// A change that deletes a package breaks every package still importing it,
// so it selects them and, transitively, their importers. The path the change
// lists is where the package was; the import path its directory would have
// -- the nearest go.mod's module path plus the directory below it -- is what
// the importers name.
func TestAffectedSelectsTheImportersOfADeletedPackage(t *testing.T) {
	g, err := ScanGoTree(deletedPackageTree())
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Incomplete(); len(got) != 0 {
		t.Fatalf("Incomplete() = %v: an import of a package the tree lacks is not a file that cannot be read", got)
	}
	cases := []struct {
		name    string
		changed []string
		seeds   []string
		want    []string
	}{
		{"the probe: b imports the deleted a", []string{"a/a.go"},
			[]string{"example.test/probe/b"}, []string{"example.test/probe/b", "example.test/probe/c"}},
		{"every file of the deleted a", []string{"a/a.go", "a/a_test.go"},
			[]string{"example.test/probe/b"}, []string{"example.test/probe/b", "example.test/probe/c"}},
		{"the deleted root package", []string{"probe.go"},
			[]string{"example.test/probe/e"}, []string{"example.test/probe/e", "example.test/probe/f"}},
		{"a nested module's deleted package", []string{"tools/lib/lib.go"},
			[]string{"example.test/devtools/gen"}, []string{"example.test/devtools/gen"}},
		// What still resolves still narrows: a deleted package must not turn
		// every change into everything.
		{"a live package nothing imports", []string{"d/d.go"},
			[]string{"example.test/probe/d"}, []string{"example.test/probe/d"}},
		{"a live package and its importer", []string{"b/b.go"},
			[]string{"example.test/probe/b"}, []string{"example.test/probe/b", "example.test/probe/c"}},
		// The repository's packages are named by its module paths: d's import
		// that only shares a prefix with one is another module's, and the
		// directory where it would be names nothing anyone imports.
		{"a path no import names", []string{"probes/lib/lib.go"}, nil, nil},
	}
	for _, c := range cases {
		sel := affectedOrFatal(t, g, c.changed)
		if sel.Full {
			t.Errorf("%s: Full (%s), want a narrowed selection", c.name, sel.Reason)
			continue
		}
		if !sameList(sel.Seeds, c.seeds) {
			t.Errorf("%s: Seeds = %v, want %v", c.name, sel.Seeds, c.seeds)
		}
		if !sameList(sel.Packages, c.want) {
			t.Errorf("%s: Packages = %v, want %v", c.name, sel.Packages, c.want)
		}
	}
	// The selection says why b runs when only a changed.
	sel := affectedOrFatal(t, g, []string{"a/a.go"})
	if !strings.Contains(sel.Reason, "example.test/probe/a") {
		t.Errorf("reason %q does not name the import path no package answers", sel.Reason)
	}
}

// sameList compares two lists, an empty one and nil alike.
func sameList(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}

// A changed path no package owns -- here, a file at the root of a nested
// module with no package of its own there, and that no import names -- seeds
// nothing: the selection is empty, not everything, and the steps that select
// packages skip.
func TestAffectedSeedsNothingForAPathUnderNoPackage(t *testing.T) {
	sel := affectedOrFatal(t, scanTree(t, true), []string{"sub/README.md"})
	if sel.Full {
		t.Fatalf("sub/README.md selected everything (%s)", sel.Reason)
	}
	if len(sel.Seeds) != 0 || len(sel.Packages) != 0 {
		t.Errorf("sub/README.md seeded %v and selected %v, want nothing", sel.Seeds, sel.Packages)
	}
	if sel.Reason == "" {
		t.Error("an empty selection carries no reason")
	}
}

// The adapter Compile consults: the graph's packages, the selection as given,
// and each package's directory.
func TestGraphSelectorAnswersFromTheGraphAndTheSelection(t *testing.T) {
	g := scanTree(t, true)
	sel := affectedOrFatal(t, g, []string{"b/b.go"})
	s := GraphSelector(g, sel)
	if got := s.All(); !reflect.DeepEqual(got, everyFixturePackage) {
		t.Errorf("All() = %v, want %v", got, everyFixturePackage)
	}
	if got := s.Affected(); !reflect.DeepEqual(got, sel) {
		t.Errorf("Affected() = %+v, want %+v", got, sel)
	}
	for ip, want := range map[string]string{
		"example.test/shop":            ".",
		"example.test/shop/sub/client": "sub/client",
		"example.test/shop/nope":       "",
	} {
		if got := s.DirOf(ip); got != want {
			t.Errorf("DirOf(%q) = %q, want %q", ip, got, want)
		}
	}
	// Answers are copies.
	s.All()[0] = "mutated"
	s.Affected().Packages[0] = "mutated"
	if s.All()[0] != everyFixturePackage[0] || s.Affected().Packages[0] != "example.test/shop/b" {
		t.Error("GraphSelector handed out storage a caller can change")
	}
}

// The seam end to end: a graph read from source, a change, and Compile
// choosing a step's packages from what the graph selected.
func TestCompileSelectsPackagesThroughTheGraph(t *testing.T) {
	g := scanTree(t, true)
	sel := affectedOrFatal(t, g, []string{"b/b.go"})
	spec := &Spec{
		Select: &Select{Go: SelectImportGraph},
		Stages: []StageSpec{{
			Name:  "tests",
			Steps: []StepSpec{{Name: "go-tests", Run: "go test $MEMQL_PACKAGES", Packages: PackagesAffected}},
		}},
	}
	plan, refusal := Compile(spec, CompileInput{
		Mode:     ModeAffected,
		Event:    EventPullRequest,
		Selector: GraphSelector(g, sel),
		PackagePolicies: map[string]PackagePolicy{
			"tests.go-tests": {Coverage: PackageCoverageAffected, Filter: PackageFilterAll},
		},
	})
	if refusal != nil {
		t.Fatal(refusal)
	}
	steps := plan.Steps()
	if len(steps) != 1 {
		t.Fatalf("plan has %d steps, want 1", len(steps))
	}
	if want := []string{"example.test/shop/b", "example.test/shop/c"}; !reflect.DeepEqual(steps[0].Packages, want) {
		t.Errorf("go-tests selects %v, want %v", steps[0].Packages, want)
	}
}

// A nil graph is a selector over nothing, not a panic.
func TestGraphSelectorOverNoGraph(t *testing.T) {
	s := GraphSelector(nil, Selection{Full: true})
	if len(s.All()) != 0 || s.DirOf("x") != "" || !s.Affected().Full {
		t.Errorf("GraphSelector(nil) answered All=%v DirOf=%q Affected=%+v", s.All(), s.DirOf("x"), s.Affected())
	}
}

// Nothing the graph cannot see may narrow: a package in a vendor directory
// below the root is not the vendor tree at a module root, and is just a path.
func TestAffectedReadsVendorOnlyAtAModuleRoot(t *testing.T) {
	g, err := ScanGoTree(fstest.MapFS{
		"go.mod":               {Data: []byte("module example.test/v\n")},
		"web/vendor/README.md": {Data: []byte("front-end assets\n")},
		"web/web.go":           {Data: []byte("package web\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	sel := affectedOrFatal(t, g, []string{"web/vendor/README.md"})
	if sel.Full {
		t.Fatalf("web/vendor/README.md selected everything (%s)", sel.Reason)
	}
	if want := []string{"example.test/v/web"}; !reflect.DeepEqual(sel.Packages, want) {
		t.Errorf("Packages = %v, want %v", sel.Packages, want)
	}
}
