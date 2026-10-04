package pipelines

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// goTree loads testdata/gotree.txtar as a file system. complete leaves out
// broken/x.go, the one file that does not parse, so the graph is whole.
func goTree(t *testing.T, complete bool) fstest.MapFS {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "gotree.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{}
	for name, content := range parseArchive(t, string(data)) {
		if complete && strings.HasPrefix(name, "broken/") {
			continue
		}
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return fsys
}

// parseArchive reads the txtar layout: a "-- path --" line starts a file and
// every line up to the next one is its content. What comes before the first
// marker is the archive's own comment.
func parseArchive(t *testing.T, data string) map[string]string {
	t.Helper()
	files := map[string]string{}
	name := ""
	var body strings.Builder
	flush := func() {
		if name != "" {
			files[name] = body.String()
		}
	}
	for _, line := range strings.SplitAfter(data, "\n") {
		marker := strings.TrimSuffix(line, "\n")
		if len(marker) > len("--  --") && strings.HasPrefix(marker, "-- ") && strings.HasSuffix(marker, " --") {
			flush()
			name = strings.TrimSpace(marker[len("-- ") : len(marker)-len(" --")])
			if _, dup := files[name]; dup {
				t.Fatalf("the archive holds %s twice", name)
			}
			body.Reset()
			continue
		}
		if name != "" {
			body.WriteString(line)
		}
	}
	flush()
	if len(files) == 0 {
		t.Fatal("the archive holds no files")
	}
	return files
}

func scanTree(t *testing.T, complete bool) *Graph {
	t.Helper()
	g, err := ScanGoTree(goTree(t, complete))
	if err != nil {
		t.Fatalf("ScanGoTree: %v", err)
	}
	return g
}

// The whole graph, as the go tool would see it under the union of every build
// tag: test imports and tag-only imports are edges, and nothing the go tool
// skips is a package.
func TestScanGoTreeReadsTheImportGraphFromSource(t *testing.T) {
	g := scanTree(t, false)
	want := []Package{
		{ImportPath: "example.test/shop", Dir: ".", Imports: []string{"example.test/shop/a"}},
		// a/_scratch.go imports c; the go tool ignores a file whose name
		// begins with "_", and so does the graph.
		{ImportPath: "example.test/shop/a", Dir: "a"},
		// b/.#b.go imports c; a name beginning with "." is ignored too.
		{ImportPath: "example.test/shop/b", Dir: "b", Imports: []string{"example.test/shop/a"}},
		// The broken file still makes its directory a package; what it
		// imports is unknown, which is what Incomplete says.
		{ImportPath: "example.test/shop/broken", Dir: "broken"},
		{ImportPath: "example.test/shop/c", Dir: "c", Imports: []string{"example.test/shop/b"}},
		{ImportPath: "example.test/shop/sub/client", Dir: "sub/client", Imports: []string{"example.test/shop/a"}},
		{ImportPath: "example.test/shop/tagged", Dir: "tagged", Imports: []string{"example.test/shop/a"}},
	}
	if got := g.Packages(); !reflect.DeepEqual(got, want) {
		t.Errorf("Packages() =\n%s\nwant\n%s", describePackages(got), describePackages(want))
	}
}

// c imports b only from c_test.go (an external test package), and tagged
// imports a only under //go:build tools. Both are edges: a change to b changes
// c's test binary, and some build of tagged links a. The union over tags is a
// superset of what `go list` reports for any one build, which is the safe
// direction for a selection.
func TestScanGoTreeCountsTestOnlyAndTagOnlyImports(t *testing.T) {
	g := scanTree(t, true)
	imports := map[string][]string{}
	for _, p := range g.Packages() {
		imports[p.ImportPath] = p.Imports
	}
	if got := imports["example.test/shop/c"]; !reflect.DeepEqual(got, []string{"example.test/shop/b"}) {
		t.Errorf("c imports %v, want [example.test/shop/b] from its _test.go", got)
	}
	if got := imports["example.test/shop/tagged"]; !reflect.DeepEqual(got, []string{"example.test/shop/a"}) {
		t.Errorf("tagged imports %v, want [example.test/shop/a] under its build tag", got)
	}
	// fmt and testing are imports too, but not the repository's.
	if got := imports["example.test/shop"]; !reflect.DeepEqual(got, []string{"example.test/shop/a"}) {
		t.Errorf("the root package imports %v, want only its first-party import", got)
	}
}

// A directory below a go.mod belongs to that module: sub/client is named from
// sub's module path, and its import of the root module's package is an edge.
func TestScanGoTreeNamesANestedModulesPackagesFromItsOwnGoMod(t *testing.T) {
	fsys := fstest.MapFS{
		"go.mod":            {Data: []byte("module example.test/root\n")},
		"lib/lib.go":        {Data: []byte("package lib\n")},
		"tools/go.mod":      {Data: []byte("// tools is its own module.\nmodule \"example.test/devtools\" // quoted\n\ngo 1.22\n")},
		"tools/gen/main.go": {Data: []byte("package main\n\nimport \"example.test/root/lib\"\n")},
	}
	g, err := ScanGoTree(fsys)
	if err != nil {
		t.Fatal(err)
	}
	want := []Package{
		{ImportPath: "example.test/devtools/gen", Dir: "tools/gen", Imports: []string{"example.test/root/lib"}},
		{ImportPath: "example.test/root/lib", Dir: "lib"},
	}
	if got := g.Packages(); !reflect.DeepEqual(got, want) {
		t.Errorf("Packages() =\n%s\nwant\n%s", describePackages(got), describePackages(want))
	}
}

// testdata, vendor and any directory beginning with "." or "_" are invisible
// to the go tool, and to the graph: no package, no edge, no go.mod.
func TestScanGoTreeSkipsWhatTheGoToolSkips(t *testing.T) {
	g := scanTree(t, true)
	for _, p := range g.Packages() {
		for _, skipped := range []string{"testdata", "_hidden", ".tools", "vendor"} {
			if p.Dir == skipped || strings.HasPrefix(p.Dir, skipped+"/") {
				t.Errorf("package %s lives in %s, which the go tool skips", p.ImportPath, p.Dir)
			}
		}
	}
	// testdata/fixture.go and .tools/tool.go import b; _hidden/hidden.go and
	// vendor's dep.go import c. None of them is an importer.
	sel := affectedOrFatal(t, g, []string{"b/b.go"})
	if want := []string{"example.test/shop/b", "example.test/shop/c"}; !reflect.DeepEqual(sel.Packages, want) {
		t.Errorf("b's importers selected %v, want %v", sel.Packages, want)
	}
}

// A file whose imports cannot be read leaves the graph unable to say what it
// reaches. The scan still succeeds; the graph says so.
func TestScanGoTreeMarksAFileThatDoesNotParseIncomplete(t *testing.T) {
	if got, want := scanTree(t, false).Incomplete(), []string{"broken/x.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Incomplete() = %v, want %v", got, want)
	}
	if got := scanTree(t, true).Incomplete(); len(got) != 0 {
		t.Errorf("Incomplete() = %v for a tree where every file parses", got)
	}
}

// A go.mod naming no module cannot name its packages; two directories that
// come out as one import path cannot both be it. Either way the graph is
// incomplete rather than guessed -- and nothing it cannot name is named.
func TestScanGoTreeMarksWhatItCannotNameIncomplete(t *testing.T) {
	cases := map[string]struct {
		fsys     fstest.MapFS
		want     []string
		packages []string
	}{
		"a go.mod with no module line": {
			fsys: fstest.MapFS{
				"go.mod":  {Data: []byte("go 1.22\n")},
				"main.go": {Data: []byte("package main\n")},
			},
			want: []string{"go.mod"},
		},
		// The broken module still owns its directory. Read as part of the
		// module above it, tool/ would be named example.test/root/tool, a
		// package no build has, and a selection of everything would hand
		// that name to `go test`.
		"a nested go.mod with no module line": {
			fsys: fstest.MapFS{
				"go.mod":        {Data: []byte("module example.test/root\n")},
				"lib/lib.go":    {Data: []byte("package lib\n")},
				"tool/go.mod":   {Data: []byte("go 1.22\n")},
				"tool/main.go":  {Data: []byte("package main\n")},
				"tool/x/x.go":   {Data: []byte("package x\n")},
				"tool/README":   {Data: []byte("a tool\n")},
				"other/go.mod":  {Data: []byte("module example.test/other\n")},
				"other/main.go": {Data: []byte("package main\n")},
			},
			want:     []string{"tool/go.mod"},
			packages: []string{"example.test/other", "example.test/root/lib"},
		},
		"two modules claiming one path": {
			fsys: fstest.MapFS{
				"one/go.mod":   {Data: []byte("module example.test/same\n")},
				"one/one.go":   {Data: []byte("package same\n")},
				"two/go.mod":   {Data: []byte("module example.test/same\n")},
				"two/two.go":   {Data: []byte("package same\n")},
				"three/go.mod": {Data: []byte("module example.test/three\n")},
				"three/x.go":   {Data: []byte("package three\n")},
			},
			want: []string{"one", "two"},
		},
	}
	for name, c := range cases {
		g, err := ScanGoTree(c.fsys)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := g.Incomplete(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Incomplete() = %v, want %v", name, got, c.want)
		}
		if c.packages != nil {
			var got []string
			for _, p := range g.Packages() {
				got = append(got, p.ImportPath)
			}
			if !reflect.DeepEqual(got, c.packages) {
				t.Errorf("%s: packages %v, want %v", name, got, c.packages)
			}
			if owner, ok := g.PackageAt("tool/README"); ok {
				t.Errorf("%s: PackageAt(tool/README) = %s, a package of the module above a broken one", name, owner)
			}
		}
	}
}

// A .go file under no go.mod is not a package any module build can name.
func TestScanGoTreeIgnoresGoFilesOutsideEveryModule(t *testing.T) {
	g, err := ScanGoTree(fstest.MapFS{
		"scripts/gen.go": {Data: []byte("package main\n")},
		"README.md":      {Data: []byte("# no Go module here\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Packages(); len(got) != 0 {
		t.Errorf("Packages() = %v for a tree with no go.mod", got)
	}
}

// The package owning a path is the package in its directory or the nearest
// one above it, within the path's own module: what a package can embed, and
// so the only files whose change reaches it without an import.
func TestPackageAtFindsTheOwningPackage(t *testing.T) {
	g := scanTree(t, true)
	cases := []struct {
		path string
		want string
	}{
		{"a/a.go", "example.test/shop/a"},
		{"./a/a.go", "example.test/shop/a"},
		{"a/testdata/golden.json", "example.test/shop/a"},
		{"docs/guide.md", "example.test/shop"},
		{"go.mod", "example.test/shop"},
		// A skipped directory's files still sit under a package directory.
		{"_hidden/hidden.go", "example.test/shop"},
		{"sub/client/client.go", "example.test/shop/sub/client"},
		// sub is its own module with no package at its root: nothing in the
		// root module can embed sub/README.md, so the root package does not
		// own it.
		{"sub/README.md", ""},
		{"sub/docs/notes.md", ""},
		{"../outside.go", ""},
		{"/etc/passwd", ""},
		{"", ""},
	}
	for _, c := range cases {
		got, ok := g.PackageAt(c.path)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("PackageAt(%q) = (%q, %v), want (%q, %v)", c.path, got, ok, c.want, c.want != "")
		}
	}
}

// What the graph hands out is a copy: a caller sorting or appending cannot
// change the graph a second caller reads.
func TestScanGoTreeHandsOutCopies(t *testing.T) {
	g := scanTree(t, false)
	pkgs := g.Packages()
	pkgs[0].Imports[0] = "mutated"
	pkgs[0].ImportPath = "mutated"
	inc := g.Incomplete()
	inc[0] = "mutated"
	again := g.Packages()
	if again[0].ImportPath != "example.test/shop" || again[0].Imports[0] != "example.test/shop/a" {
		t.Errorf("Packages() handed out the graph's own storage: %+v", again[0])
	}
	if g.Incomplete()[0] != "broken/x.go" {
		t.Error("Incomplete() handed out the graph's own storage")
	}
}

func TestScanGoTreeOfAnEmptyTreeIsAnEmptyGraph(t *testing.T) {
	g, err := ScanGoTree(fstest.MapFS{})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Packages()) != 0 || len(g.Incomplete()) != 0 {
		t.Errorf("an empty tree scanned to %v / %v", g.Packages(), g.Incomplete())
	}
}

func describePackages(pkgs []Package) string {
	var b strings.Builder
	for _, p := range pkgs {
		b.WriteString("  " + p.ImportPath + " (" + p.Dir + ") -> [" + strings.Join(p.Imports, " ") + "]\n")
	}
	return b.String()
}
