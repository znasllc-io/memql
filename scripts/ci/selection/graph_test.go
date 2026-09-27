package selection

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fixturePrefix = "example.test/memql"

func loadFixture(t *testing.T) *Graph {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	g, err := ReadGraph(f)
	if err != nil {
		t.Fatalf("ReadGraph(testdata/graph.json): %v", err)
	}
	return g
}

func ip(dir string) string {
	if dir == "." {
		return fixturePrefix
	}
	return fixturePrefix + "/" + dir
}

func TestNewGraphRefusesWhatItCouldAnswerWrongly(t *testing.T) {
	base := func() []Package {
		return []Package{
			{ImportPath: ip("."), Dir: ".", ModuleDir: "."},
			{ImportPath: ip("a"), Dir: "a", ModuleDir: ".", Deps: []string{ip(".")}},
		}
	}
	cases := map[string]func([]Package) []Package{
		"duplicate import path": func(p []Package) []Package {
			return append(p, Package{ImportPath: ip("a"), Dir: "b", ModuleDir: "."})
		},
		"duplicate directory": func(p []Package) []Package {
			return append(p, Package{ImportPath: ip("b"), Dir: "a", ModuleDir: "."})
		},
		"dangling dependency": func(p []Package) []Package {
			p[1].Deps = []string{ip("gone")}
			return p
		},
		"missing directory": func(p []Package) []Package {
			p[1].Dir = ""
			return p
		},
		"missing module directory": func(p []Package) []Package {
			p[1].ModuleDir = ""
			return p
		},
	}
	if _, err := NewGraph(base()); err != nil {
		t.Fatalf("the unmodified base graph must be valid, or every case below proves nothing: %v", err)
	}
	for name, mutate := range cases {
		if _, err := NewGraph(mutate(base())); err == nil {
			t.Errorf("%s: NewGraph accepted it", name)
		}
	}
}

func TestRecordedGraphRoundTrips(t *testing.T) {
	g := loadFixture(t)
	var buf bytes.Buffer
	if err := g.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	again, err := ReadGraph(&buf)
	if err != nil {
		t.Fatalf("re-reading WriteJSON output: %v", err)
	}
	if !reflect.DeepEqual(g.ImportPaths(), again.ImportPaths()) {
		t.Fatalf("import paths changed across a round trip")
	}
	for _, p := range g.ImportPaths() {
		a, _ := g.Package(p)
		b, _ := again.Package(p)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s changed across a round trip:\n  %+v\n  %+v", p, a, b)
		}
	}
}

func TestExpand(t *testing.T) {
	g := loadFixture(t)
	cases := []struct {
		pattern string
		want    []string
	}{
		{"./", []string{ip(".")}},
		{".", []string{ip(".")}},
		{"./component/memql", []string{ip("component/memql")}},
		{"./component/memql/...", []string{ip("component/memql"), ip("component/memql/offline")}},
		{"./scripts/...", []string{ip("scripts/ci")}},
	}
	for _, c := range cases {
		got, err := g.Expand(c.pattern)
		if err != nil {
			t.Errorf("Expand(%q): %v", c.pattern, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Expand(%q) = %v, want %v", c.pattern, got, c.want)
		}
	}
	for _, bad := range []string{"./...", "./nope", "./nope/..."} {
		if got, err := g.Expand(bad); err == nil {
			t.Errorf("Expand(%q) = %v, want an error", bad, got)
		}
	}
}

// fakeList returns canned `go list -json` streams keyed by tag.
func fakeList(streams map[string]string) ListFunc {
	return func(_ context.Context, tags string) ([]byte, error) {
		s, ok := streams[tags]
		if !ok {
			return nil, fmt.Errorf("unexpected configuration %q", tags)
		}
		return []byte(s), nil
	}
}

func TestLoadGraphUnionsConfigurations(t *testing.T) {
	root := "/repo"
	untagged := `
{"ImportPath":"m","Dir":"/repo","Module":{"Path":"m","Dir":"/repo"},"GoFiles":["main.go"],"IgnoredGoFiles":["agent.go"],"Deps":["m/b","fmt"]}
{"ImportPath":"m/b","Dir":"/repo/b","Module":{"Path":"m","Dir":"/repo"},"GoFiles":["b.go"],"EmbedFiles":["data/x.json"],"TestImports":["m/c"]}
{"ImportPath":"m/c","Dir":"/repo/c","Module":{"Path":"m/c","Dir":"/repo/c"},"GoFiles":["c.go"],"Deps":["m/d"]}
{"ImportPath":"m/d","Dir":"/repo/d","Module":{"Path":"m/c","Dir":"/repo/c"},"GoFiles":["d.go"]}
{"ImportPath":"m/e","Dir":"/repo/e","Module":{"Path":"m","Dir":"/repo"},"GoFiles":["e.go"],"TestGoFiles":["e_test.go"],"IgnoredGoFiles":["agent_test.go"],"TestEmbedFiles":["testdata/golden.txt"]}
{"ImportPath":"other/x","Dir":"/elsewhere","Module":{"Path":"other","Dir":"/elsewhere"}}
`
	tagged := `
{"ImportPath":"m","Dir":"/repo","Module":{"Path":"m","Dir":"/repo"},"GoFiles":["main.go","agent.go"],"Deps":["m/b","m/e"]}
{"ImportPath":"m/b","Dir":"/repo/b","Module":{"Path":"m","Dir":"/repo"},"GoFiles":["b.go"]}
{"ImportPath":"m/c","Dir":"/repo/c","Module":{"Path":"m/c","Dir":"/repo/c"},"GoFiles":["c.go"],"Deps":["m/d"]}
{"ImportPath":"m/d","Dir":"/repo/d","Module":{"Path":"m/c","Dir":"/repo/c"},"GoFiles":["d.go"]}
{"ImportPath":"m/e","Dir":"/repo/e","Module":{"Path":"m","Dir":"/repo"},"GoFiles":["e.go"],"TestGoFiles":["e_test.go","agent_test.go"]}
`
	g, err := LoadGraph(context.Background(), root, "m", []string{"agent"},
		fakeList(map[string]string{"": untagged, "agent": tagged}))
	if err != nil {
		t.Fatal(err)
	}
	if g.Len() != 5 {
		t.Fatalf("want the 5 first-party packages (other/x is not first-party), got %v", g.ImportPaths())
	}
	rootPkg, _ := g.Package("m")
	if want := []string{"m/b", "m/e"}; !reflect.DeepEqual(rootPkg.Deps, want) {
		t.Errorf("root deps = %v, want %v: the agent-only edge to m/e must survive the union", rootPkg.Deps, want)
	}
	if want := []string{"agent.go"}; !reflect.DeepEqual(rootPkg.TaggedFiles, want) {
		t.Errorf("root tagged files = %v, want %v", rootPkg.TaggedFiles, want)
	}
	b, _ := g.Package("m/b")
	if want := []string{"m/c", "m/d"}; !reflect.DeepEqual(b.Deps, want) {
		t.Errorf("m/b deps = %v, want %v: a test import joins with everything it depends on", b.Deps, want)
	}
	if want := []string{"b/b.go", "b/data/x.json"}; !reflect.DeepEqual(b.Files, want) {
		t.Errorf("m/b files = %v, want %v", b.Files, want)
	}
	e, _ := g.Package("m/e")
	if want := []string{"e/e.go"}; !reflect.DeepEqual(e.Files, want) {
		t.Errorf("m/e build files = %v, want %v", e.Files, want)
	}
	if want := []string{"e/agent_test.go", "e/e_test.go", "e/testdata/golden.txt"}; !reflect.DeepEqual(e.TestFiles, want) {
		t.Errorf("m/e test files = %v, want %v: test sources, an ignored _test.go and test embeds are TEST scope", e.TestFiles, want)
	}
	if want := []string{"e/agent_test.go"}; !reflect.DeepEqual(e.TaggedFiles, want) {
		t.Errorf("m/e tagged files = %v, want %v", e.TaggedFiles, want)
	}
	c, _ := g.Package("m/c")
	if c.ModuleDir != "c" || c.Dir != "c" {
		t.Errorf("m/c dir/module = %q/%q, want c/c", c.Dir, c.ModuleDir)
	}
	if want := []string{".", "c"}; !reflect.DeepEqual(g.Modules(), want) {
		t.Errorf("Modules() = %v, want %v", g.Modules(), want)
	}
}

func TestLoadGraphRefusesABrokenLoad(t *testing.T) {
	cases := map[string]string{
		"package error": `{"ImportPath":"m","Dir":"/repo","Module":{"Path":"m","Dir":"/repo"},"Error":{"Err":"pattern static/app.css: no matching files found"}}`,
		"deps error":    `{"ImportPath":"m","Dir":"/repo","Module":{"Path":"m","Dir":"/repo"},"DepsErrors":[{"Err":"no required module provides package x"}]}`,
		"no module":     `{"ImportPath":"m","Dir":"/repo"}`,
		"outside repo":  `{"ImportPath":"m","Dir":"/elsewhere","Module":{"Path":"m","Dir":"/elsewhere"}}`,
		"empty listing": ``,
	}
	for name, stream := range cases {
		_, err := LoadGraph(context.Background(), "/repo", "m", nil, fakeList(map[string]string{"": stream}))
		if err == nil {
			t.Errorf("%s: LoadGraph accepted it", name)
		}
	}
}

func TestLoadGraphRefusesAPackageThatMovesAcrossConfigurations(t *testing.T) {
	a := `{"ImportPath":"m","Dir":"/repo","Module":{"Path":"m","Dir":"/repo"},"GoFiles":["main.go"]}`
	b := `{"ImportPath":"m","Dir":"/repo/moved","Module":{"Path":"m","Dir":"/repo"},"GoFiles":["main.go"]}`
	_, err := LoadGraph(context.Background(), "/repo", "m", []string{"agent"}, fakeList(map[string]string{"": a, "agent": b}))
	if err == nil || !strings.Contains(err.Error(), "under tags") {
		t.Fatalf("want a refusal naming the disagreeing configuration, got %v", err)
	}
}

func TestNodeTagsReadsTheBuildFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"build_agent.go", "build_default.go", "build_edge.go", "build_edge_test.go", "app.go"} {
		if err := os.WriteFile(filepath.Join(dir, "app", f), []byte("package app\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := NodeTags(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"agent", "edge"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NodeTags = %v, want %v", got, want)
	}
	if _, err := NodeTags(t.TempDir()); err == nil {
		t.Error("NodeTags on a tree with no app/build_<type>.go must refuse")
	}
}
