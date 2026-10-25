package pipelines

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func testInputTree() fstest.MapFS {
	return fstest.MapFS{
		"go.mod":                          {Data: []byte("module example.test/inputs\ngo 1.26.0\n")},
		"a/a.go":                          {Data: []byte("package a\nimport _ \"embed\"\n//go:embed data/runtime.txt runtime-dir\nvar runtime []byte\n")},
		"a/a_test.go":                     {Data: []byte("package a\nimport _ \"embed\"\n//go:embed data/test.txt\nvar testData []byte\n")},
		"a/external_test.go":              {Data: []byte("package a_test\nimport _ \"embed\"\n//go:embed data/external.txt\nvar external []byte\n")},
		"a/tagged_windows.go":             {Data: []byte("//go:build windows\n\npackage different\nimport _ \"embed\"\n//go:embed data/windows.txt\nvar platform []byte\n")},
		"a/data/runtime.txt":              {Data: []byte("runtime")},
		"a/data/test.txt":                 {Data: []byte("test")},
		"a/data/external.txt":             {Data: []byte("external")},
		"a/data/windows.txt":              {Data: []byte("windows")},
		"a/data/readme.md":                {Data: []byte("prose")},
		"a/runtime-dir/nested/n.go":       {Data: []byte("package nested\n")},
		"a/runtime-dir/nested/value.json": {Data: []byte("{}")},
		"a/header.h":                      {Data: []byte("/* native input */")},
		"b/b.go":                          {Data: []byte("package b\nimport _ \"example.test/inputs/a\"\n")},
		"c/c.go":                          {Data: []byte("package c\n")},
		"c/c_test.go":                     {Data: []byte("package c_test\nimport _ \"example.test/inputs/b\"\n")},
		"cockpit/c.go":                    {Data: []byte("package cockpit\nimport _ \"example.test/inputs/c\"\n")},
		"unknown/go.mod":                  {Data: []byte("module example.test/unknown\n")},
		"unknown/readme.md":               {Data: []byte("no package owns this")},
	}
}

func inputFacts(t *testing.T, tree fstest.MapFS, changed, declarations []string) (*Graph, SelectionFacts) {
	t.Helper()
	g, err := ScanGoTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := AnalyzeSelectionWithTestInputs(g, changed, true, nil, declarations)
	if err != nil {
		t.Fatal(err)
	}
	return g, facts
}

func TestDeclaredTestInputsKeepGoAndTestImportFanout(t *testing.T) {
	const prose = "a/data/readme.md"
	for _, tc := range []struct {
		name    string
		changed []string
		local   bool
		want    []string
	}{
		{"declared prose", []string{prose}, true, []string{"example.test/inputs/a"}},
		{"legacy policy", []string{prose}, false, []string{"example.test/inputs/a", "example.test/inputs/b", "example.test/inputs/c", "example.test/inputs/cockpit"}},
		{"mixed source", []string{prose, "a/a.go"}, true, []string{"example.test/inputs/a", "example.test/inputs/b", "example.test/inputs/c", "example.test/inputs/cockpit"}},
		{"mixed reverse order", []string{"a/a.go", prose}, true, []string{"example.test/inputs/a", "example.test/inputs/b", "example.test/inputs/c", "example.test/inputs/cockpit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, facts := inputFacts(t, testInputTree(), tc.changed, []string{prose})
			decision := SelectionDecision{}
			if tc.local {
				decision.TestInputs = []string{prose}
			}
			got, err := ResolveSelection(g, facts, decision)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Packages, tc.want) {
				t.Fatalf("packages=%v want=%v", got.Packages, tc.want)
			}
		})
	}
}

func TestDeclaredTestInputsProtectEveryBuildAndEmbedOwner(t *testing.T) {
	for _, tc := range []struct {
		path     string
		eligible bool
		owners   []string
	}{
		{"a/data/runtime.txt", false, []string{"example.test/inputs/a"}},
		{"a/data/windows.txt", false, []string{"example.test/inputs/a"}},
		{"a/header.h", false, []string{"example.test/inputs/a"}},
		{"a/a.go", false, []string{"example.test/inputs/a"}},
		{"a/data/test.txt", true, []string{"example.test/inputs/a"}},
		{"a/data/external.txt", true, []string{"example.test/inputs/a"}},
		{"a/runtime-dir/nested/value.json", false, []string{"example.test/inputs/a", "example.test/inputs/a/runtime-dir/nested"}},
		{"a/runtime-dir/nested/n.go", false, []string{"example.test/inputs/a", "example.test/inputs/a/runtime-dir/nested"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			g, facts := inputFacts(t, testInputTree(), []string{tc.path}, []string{tc.path})
			p := facts.Paths[0]
			if p.TestInputUncertain || p.TestInputEligible != tc.eligible || !reflect.DeepEqual(p.owners, tc.owners) {
				t.Fatalf("facts=%+v owners=%v", p, p.owners)
			}
			_, err := ResolveSelection(g, facts, SelectionDecision{TestInputs: []string{tc.path}})
			if (err == nil) != tc.eligible {
				t.Fatalf("local selection err=%v eligible=%v", err, tc.eligible)
			}
			if !tc.eligible {
				got, err := ResolveSelection(g, facts, SelectionDecision{})
				if err != nil || !slices.Contains(got.Packages, "example.test/inputs/cockpit") {
					t.Fatalf("runtime fanout lost: %+v %v", got, err)
				}
			}
		})
	}
}

func TestDeclaredTestInputsIncludeAncestorTestEmbedOwners(t *testing.T) {
	tree := testInputTree()
	tree["a/a_test.go"].Data = []byte("package a\nimport \"embed\"\n//go:embed data/test.txt test-dir\nvar testData embed.FS\n")
	tree["a/test-dir/nested/n.go"] = &fstest.MapFile{Data: []byte("package nested\n")}
	const input = "a/test-dir/nested/value.json"
	tree[input] = &fstest.MapFile{Data: []byte("{}")}
	g, facts := inputFacts(t, tree, []string{input}, []string{input})
	want := []string{"example.test/inputs/a", "example.test/inputs/a/test-dir/nested"}
	if !facts.Paths[0].TestInputEligible || !reflect.DeepEqual(facts.Paths[0].owners, want) {
		t.Fatalf("test embed owners: %+v %v", facts.Paths[0], facts.Paths[0].owners)
	}
	got, err := ResolveSelection(g, facts, SelectionDecision{TestInputs: []string{input}})
	if err != nil || !reflect.DeepEqual(got.Packages, want) {
		t.Fatalf("direct test owners = %+v %v", got, err)
	}
}

func TestDeclaredTestInputUncertaintyCannotNarrow(t *testing.T) {
	for _, tc := range []struct {
		name, changed string
		alter         func(fstest.MapFS)
	}{
		{"deleted", "a/data/deleted.md", nil},
		{"unknown owner", "unknown/readme.md", nil},
		{"directory", "a/data", nil},
		{"symlink", "a/data/readme.md", func(tree fstest.MapFS) { tree["a/data/readme.md"].Mode = os.ModeSymlink }},
		{"malformed embed", "a/data/readme.md", func(tree fstest.MapFS) {
			tree["a/a.go"].Data = []byte("package a\nimport _ \"embed\"\n//go:embed [bad\nvar x []byte\n")
		}},
		{"unreadable imports", "a/data/readme.md", func(tree fstest.MapFS) { tree["a/a.go"].Data = []byte("package a\nimport \"unterminated\n") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := testInputTree()
			if tc.alter != nil {
				tc.alter(tree)
			}
			g, facts := inputFacts(t, tree, []string{tc.changed}, []string{tc.changed})
			if facts.GraphComplete && !facts.Paths[0].TestInputUncertain {
				t.Fatalf("unsafe input became proven: %+v", facts)
			}
			for _, decision := range []SelectionDecision{{}, {TestInputs: []string{tc.changed}}} {
				if got, err := ResolveSelection(g, facts, decision); err == nil {
					t.Fatalf("uncertainty narrowed: %+v", got)
				}
			}
			got, err := ResolveSelection(g, facts, SelectionDecision{Full: true})
			if err != nil || !got.Full || !reflect.DeepEqual(got.Packages, facts.AllPackages) {
				t.Fatalf("full fallback=%+v %v", got, err)
			}
		})
	}
	t.Run("unreadable declared content", func(t *testing.T) {
		const p = "a/data/readme.md"
		g, err := ScanGoTree(inputOpenFailure{FS: testInputTree(), name: p})
		if err != nil {
			t.Fatal(err)
		}
		facts, err := AnalyzeSelectionWithTestInputs(g, []string{p}, true, nil, []string{p})
		if err != nil {
			t.Fatal(err)
		}
		if !facts.Paths[0].TestInputUncertain {
			t.Fatalf("unreadable input was eligible: %+v", facts.Paths[0])
		}
		if _, err := ResolveSelection(g, facts, SelectionDecision{}); err == nil {
			t.Fatal("unreadable declaration narrowed")
		}
	})
}

type inputOpenFailure struct {
	fs.FS
	name string
}

func (f inputOpenFailure) Open(name string) (fs.File, error) {
	if name == f.name {
		return nil, fs.ErrPermission
	}
	return f.FS.Open(name)
}

func TestDeclaredInputScanUsesOnlySuppliedSource(t *testing.T) {
	// A source-only driver does not have a go command or a checkout. Neither
	// hostile host environment nor mutually exclusive target files may alter
	// the immutable source classification.
	for _, key := range []string{"PATH", "GOROOT", "GOPATH", "GOENV", "GOWORK"} {
		t.Setenv(key, "/not-a-source-or-toolchain")
	}
	t.Setenv("GOOS", "windows")
	t.Setenv("GOARCH", "arm64")
	t.Setenv("CGO_ENABLED", "0")
	for _, tc := range []struct {
		path     string
		eligible bool
	}{{"a/data/readme.md", true}, {"a/data/windows.txt", false}, {"a/header.h", false}} {
		_, facts := inputFacts(t, testInputTree(), []string{tc.path}, []string{tc.path})
		if f := facts.Paths[0]; f.TestInputUncertain || f.TestInputEligible != tc.eligible {
			t.Fatalf("host environment changed source classification: %+v", f)
		}
	}
}

func TestDeclaredTestInputsRefuseForgedPolicySelections(t *testing.T) {
	const p = "a/data/readme.md"
	for _, tc := range []struct {
		name                           string
		changed, declared, local, full []string
	}{
		{"undeclared", []string{p}, nil, []string{p}, nil},
		{"outside diff", []string{p}, []string{"a/**"}, []string{"a/data/test.txt"}, nil},
		{"duplicate", []string{p}, []string{p}, []string{p, p}, nil},
		{"module", []string{p, "go.mod"}, []string{p}, []string{p}, nil},
		{"manifest", []string{p, ManifestPath}, []string{p}, []string{p}, nil},
		{"configured full", []string{p}, []string{p}, []string{p}, []string{p}},
		{"vendor", []string{p, "vendor/lib/a.go"}, []string{p}, []string{p}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := inputFacts(t, testInputTree(), tc.changed, tc.declared)
			facts, err := AnalyzeSelectionWithTestInputs(g, tc.changed, true, tc.full, tc.declared)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := ResolveSelection(g, facts, SelectionDecision{TestInputs: tc.local}); err == nil {
				t.Fatalf("forged policy narrowed: %+v", got)
			}
		})
	}
}

func TestDeclaredTestInputManifestValidation(t *testing.T) {
	for _, tc := range []struct {
		name, strategy string
		globs          []string
		valid          bool
	}{
		{"explicit", SelectImportGraph, []string{"docs/guide.md"}, true},
		{"bounded glob", SelectImportGraph, []string{"docs/**", "!docs/runtime/**"}, true},
		{"no graph", "", []string{"docs/guide.md"}, false},
		{"invalid", SelectImportGraph, []string{"../outside"}, false},
		{"negative parent", SelectImportGraph, []string{"docs/**", "!../outside"}, false},
		{"absolute", SelectImportGraph, []string{"/docs/**"}, false},
		{"dot segment", SelectImportGraph, []string{"docs/../guide.md"}, false},
		{"root itself", SelectImportGraph, []string{"."}, false},
		{"malformed glob", SelectImportGraph, []string{"docs/[bad"}, false},
		{"empty", SelectImportGraph, []string{""}, false},
		{"negative only", SelectImportGraph, []string{"!docs/**"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := validateSelect(&Select{Go: tc.strategy, TestInputs: tc.globs})
			if (r == nil) != tc.valid {
				t.Fatalf("refusal=%v valid=%v", r, tc.valid)
			}
		})
	}
}

// The production classifier uses source only. This qualification checks its
// conservative owners against the compiler's actual files on a closed fixture;
// no external modules or downloads are needed.
func TestDeclaredInputsCoverActualGoListEmbedFiles(t *testing.T) {
	tree := testInputTree()
	delete(tree, "a/tagged_windows.go") // Different tagged package is tested by the source-only union control.
	delete(tree, "a/header.h")
	tree["a/a.go"].Data = []byte("package a\nimport \"embed\"\n//go:embed data/runtime.txt runtime-dir all:hidden \"data/space name.txt\"\nvar runtime embed.FS\n")
	tree["a/hidden/.dot"] = &fstest.MapFile{Data: []byte("included by all:")}
	tree["a/data/space name.txt"] = &fstest.MapFile{Data: []byte("quoted")}
	dir := t.TempDir()
	for name, file := range tree {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), file.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-test", "-json", ".")
	cmd.Dir = filepath.Join(dir, "a")
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !slices.Contains([]string{"GOWORK", "GOPROXY", "GOSUMDB", "GOTOOLCHAIN", "GOFLAGS", "CGO_ENABLED", "GOOS", "GOARCH"}, key) {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOFLAGS=", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	data, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list fixture: %v", err)
	}
	// Only -test resolves both test embed lanes. Its stream also includes
	// generated test packages; select the original package exactly once.
	type embedPackage struct {
		ImportPath                                  string
		EmbedFiles, TestEmbedFiles, XTestEmbedFiles []string
	}
	var actual embedPackage
	matches := 0
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	for {
		var pkg embedPackage
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if pkg.ImportPath == "example.test/inputs/a" {
			actual = pkg
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("compiler returned %d original fixture packages, want one", matches)
	}
	for _, lane := range []struct {
		name    string
		files   []string
		runtime bool
	}{{"runtime", actual.EmbedFiles, true}, {"test", actual.TestEmbedFiles, false}, {"external test", actual.XTestEmbedFiles, false}} {
		if len(lane.files) == 0 {
			t.Fatalf("compiler returned no embed files for required %s lane", lane.name)
		}
		for _, file := range lane.files {
			p := "a/" + file
			_, facts := inputFacts(t, tree, []string{p}, []string{p})
			f := facts.Paths[0]
			if f.TestInputUncertain || f.TestInputEligible == lane.runtime || !slices.Contains(f.owners, "example.test/inputs/a") {
				t.Fatalf("compiler input %s runtime=%v: %+v owners=%v", file, lane.runtime, f, f.owners)
			}
		}
	}
}
