package pipelines

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func affectedOrFatal(t *testing.T, g *Graph, changed []string, fullGlobs ...string) Selection {
	t.Helper()
	sel, err := Affected(g, changed, fullGlobs)
	if err != nil {
		t.Fatalf("Affected(%v, %v): %v", changed, fullGlobs, err)
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

// Every way a change can defeat the graph selects everything, and says why.
func TestAffectedSelectsEverythingWhenTheChangeDefeatsTheGraph(t *testing.T) {
	g := scanTree(t, true)
	cases := []struct {
		name    string
		changed []string
		reason  string // a fragment the reason must carry
	}{
		{"an empty change list", nil, "empty"},
		{"the root go.mod", []string{"go.mod"}, "go.mod"},
		{"a nested go.mod", []string{"docs/guide.md", "sub/go.mod"}, "sub/go.mod"},
		{"a go.sum anywhere", []string{"tools/go.sum"}, "tools/go.sum"},
		{"go.work", []string{"go.work"}, "go.work"},
		{"go.work.sum", []string{"go.work.sum"}, "go.work.sum"},
		{"the manifest", []string{"memql-package.yaml"}, "memql-package.yaml"},
		{"vendored code", []string{"vendor/example.test/dep/dep.go"}, "vendor"},
		{"a path outside the repository", []string{"../elsewhere/x.go"}, "not repository-relative"},
		{"an absolute path", []string{"/a/a.go"}, "not repository-relative"},
	}
	for _, c := range cases {
		sel := affectedOrFatal(t, g, c.changed)
		if !sel.Full {
			t.Errorf("%s: selected %v, want Full", c.name, sel.Packages)
			continue
		}
		if !strings.Contains(sel.Reason, c.reason) {
			t.Errorf("%s: reason %q does not mention %q", c.name, sel.Reason, c.reason)
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

// A file that does not parse leaves the graph unable to say what a change
// reaches, so every change selects everything -- a docs-only one included.
func TestAffectedSelectsEverythingOverAnIncompleteGraph(t *testing.T) {
	g := scanTree(t, false)
	sel := affectedOrFatal(t, g, []string{"docs/guide.md"})
	if !sel.Full {
		t.Fatalf("an incomplete graph selected %v, want Full", sel.Packages)
	}
	if !strings.Contains(sel.Reason, "broken/x.go") {
		t.Errorf("reason %q does not name the file that did not parse", sel.Reason)
	}
	if len(sel.Packages) != len(g.Packages()) {
		t.Errorf("Full selected %d of %d packages", len(sel.Packages), len(g.Packages()))
	}
}

// select.full is the pipeline's own list of paths whose change means
// everything, read as Validate admits it: PathSet, so a `!` glob excepts.
func TestAffectedSelectsEverythingOnASelectFullMatch(t *testing.T) {
	g := scanTree(t, true)
	full := []string{"docs/**", "!docs/drafts/**"}

	sel := affectedOrFatal(t, g, []string{"a/a_test.go", "docs/guide.md"}, full...)
	if !sel.Full || !strings.Contains(sel.Reason, "docs/guide.md") {
		t.Errorf("docs/guide.md under select.full %v: got %+v, want Full naming the path", full, sel)
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
		if sel, err := Affected(g, []string{"docs/guide.md"}, full); err == nil {
			t.Errorf("select.full %v read as %+v, want an error", full, sel)
		}
	}
	if _, err := Affected(nil, []string{"a/a.go"}, nil); err == nil {
		t.Error("Affected over no graph answered without an error")
	}
}

// A changed path no package owns -- here, a file at the root of a nested
// module with no package of its own there -- seeds nothing: the selection is
// empty, not everything, and the steps that select packages skip.
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
