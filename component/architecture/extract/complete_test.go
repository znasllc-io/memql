package extract

import (
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestCompleteLoadRejectsQuietDependencyErrors(t *testing.T) {
	dependency := &packages.Package{PkgPath: "example.test/dependency", Errors: []packages.Error{{Msg: "broken dependency", Kind: packages.TypeError}}}
	root := &packages.Package{PkgPath: "example.test/root", Imports: map[string]*packages.Package{"dependency": dependency}}
	if err := requireCompleteLoad([]*packages.Package{root}, true); err == nil || !strings.Contains(err.Error(), "broken dependency") {
		t.Fatalf("quiet dependency error accepted: %v", err)
	}
}

func TestCompleteLoadRejectsAbsentTypeDataAndEmptyRoots(t *testing.T) {
	for _, tc := range []struct {
		name  string
		roots []*packages.Package
	}{
		{"empty", nil},
		{"ill typed without diagnostic", []*packages.Package{{PkgPath: "example.test/root", IllTyped: true}}},
		{"missing syntax", []*packages.Package{{PkgPath: "example.test/root", CompiledGoFiles: []string{"root.go"}, Types: types.NewPackage("example.test/root", "root"), TypesInfo: &types.Info{}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := requireCompleteLoad(tc.roots, true); err == nil {
				t.Fatal("incomplete load accepted")
			}
		})
	}
}

// Real go/packages loads: both formerly returned a partial model with exit 0.
func TestRunRefusesBrokenSourceAndDependency(t *testing.T) {
	for _, tc := range []struct{ name, root, dependency, want string }{
		{"missing import", "package fixture\nimport _ \"example.test/absent\"\n", "", "incomplete packages load"},
		{"dependency type error", "package fixture\nimport _ \"example.test/fixture/dep\"\n", "package dep\nvar Broken int = \"wrong\"\n", "incomplete types load"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := sourceFixture(t, tc.root, tc.dependency)
			result, err := Run(Options{WorkspaceRoot: dir, IncludeTypes: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) || result != nil {
				t.Fatalf("result=%v error=%v, want no partial model and %q", result, err, tc.want)
			}
		})
	}
}

func TestRunBuildsFromCleanSourceWithoutTopology(t *testing.T) {
	dir := sourceFixture(t, "package fixture\ntype Item struct { Name string }\n", "")
	result, err := Run(Options{WorkspaceRoot: dir, IncludeTypes: true, ClusterName: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) < 4 {
		t.Fatalf("generated too few nodes: %d", len(result.Nodes))
	}
	if _, err := os.Stat(filepath.Join(dir, "topology.model.json")); !os.IsNotExist(err) {
		t.Fatalf("extractor requires or writes topology input: %v", err)
	}
}

func sourceFixture(t *testing.T, source, dependency string) string {
	t.Helper()
	dir := t.TempDir()
	for key, value := range map[string]string{"GOWORK": "off", "GOFLAGS": "-mod=readonly", "GOPROXY": "off", "GOSUMDB": "off", "GOPACKAGESDRIVER": "off", "GOTOOLCHAIN": "local"} {
		t.Setenv(key, value)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.test/fixture\n\ngo 1.26.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if dependency != "" {
		if err := os.Mkdir(filepath.Join(dir, "dep"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "dep", "dep.go"), []byte(dependency), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
