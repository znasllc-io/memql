package dslimports

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/visionarys-io/memql/component/memql/dslfs"
)

// TestLoad_HappyPath_NewSyntax locks the end-to-end pipeline on a
// small synthetic tree that uses the new import syntax. Walker
// finds files, parser extracts imports, build pipeline resolves
// paths + aliases, graph emits topo order.
func TestLoad_HappyPath_NewSyntax(t *testing.T) {
	root := fstest.MapFS{
		"common/space.memql": {Data: []byte(`
// space concept
@description("a space")
concept space { name string }
`)},
		"cognition/participant.memql": {Data: []byte(`
import (
	"../common/space"
)
@description("a participant")
concept participant { spaceId string }
`)},
		"cognition/queries/listParticipants.memql": {Data: []byte(`
import (
	"../participant"
	"../../common/space"
)
@description("list participants")
func (Query) listParticipants(_ any) (any, error) {
  return nil, nil
}
`)},
	}

	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Every walked file present in Files.
	want := []string{
		"cognition/participant.memql",
		"cognition/queries/listParticipants.memql",
		"common/space.memql",
	}
	got := make([]string, 0, len(tree.Files))
	for p := range tree.Files {
		got = append(got, p)
	}
	if !setEqual(got, want) {
		t.Errorf("Files keys = %v, want %v", got, want)
	}

	// Aliases for the query file.
	queryAliases := tree.Aliases["cognition/queries/listParticipants.memql"]
	wantAliases := dslfs.FileImports{
		"participant": "cognition/participant.memql",
		"space":       "common/space.memql",
	}
	if !reflect.DeepEqual(queryAliases, wantAliases) {
		t.Errorf("query aliases = %v, want %v", queryAliases, wantAliases)
	}

	// Topo order: common/space first (no imports), then participant
	// (imports space), then the query (imports both).
	if len(tree.Order) != 3 {
		t.Fatalf("Order length = %d, want 3", len(tree.Order))
	}
	if tree.Order[0] != "common/space.memql" {
		t.Errorf("Order[0] = %q, want common/space.memql", tree.Order[0])
	}
	if tree.Order[2] != "cognition/queries/listParticipants.memql" {
		t.Errorf("Order[last] = %q, want listParticipants", tree.Order[2])
	}
}

// TestLoad_LegacyUseFiles_LoadCleanly locks the transitional state:
// files that still use the legacy `use` directive (no `import`) load
// without errors through the import-graph pipeline. Their import
// lists are empty; the new graph has no edges for them; the old
// loader (untouched in this commit) still handles them via the
// legacy concept-resolver path.
//
// Uses procedural func form because the struct-form `query NAME {}`
// goes through a rewriter that the bare parser.ParseFile entry
// doesn't run. The point of this test is "presence of legacy `use`
// declarations doesn't trip the new pipeline" -- not full parse
// coverage of every author shape.
func TestLoad_LegacyUseFiles_LoadCleanly(t *testing.T) {
	root := fstest.MapFS{
		"legacy.memql": {Data: []byte(`use cognition.participant

@description("legacy")
@useConcept(participant)
func (Query) legacyQuery(_ any) (any, error) {
  return nil, nil
}
`)},
	}
	_, err := Load(root)
	if err != nil {
		t.Fatalf("Load: legacy file should load cleanly, got %v", err)
	}
}

// TestLoad_MissingImportTarget locks the "imported file does not
// exist" error.
func TestLoad_MissingImportTarget(t *testing.T) {
	root := fstest.MapFS{
		"caller.memql": {Data: []byte(`
import (
	"./does/not/exist"
)
`)},
	}
	_, err := Load(root)
	if err == nil {
		t.Fatal("expected missing-target error, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err %q should mention 'does not exist'", err.Error())
	}
}

// TestLoad_CycleDetection locks cycle detection via the integrated
// pipeline.
func TestLoad_CycleDetection(t *testing.T) {
	root := fstest.MapFS{
		"a.memql": {Data: []byte(`
import (
	"./b"
)
`)},
		"b.memql": {Data: []byte(`
import (
	"./a"
)
`)},
	}
	_, err := Load(root)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	var ce *dslfs.CycleError
	if !errors.As(err, &ce) {
		t.Errorf("expected wrapped *CycleError, got %T (%v)", err, err)
	}
}

// TestLoad_BadImportPathSurfaces locks that import-path errors
// (absolute paths, escapes) are reported with the importing file's
// path.
func TestLoad_BadImportPathSurfaces(t *testing.T) {
	root := fstest.MapFS{
		"caller.memql": {Data: []byte(`
import (
	"/absolute/path"
)
`)},
	}
	_, err := Load(root)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("err %q should mention 'absolute'", err.Error())
	}
	if !strings.Contains(err.Error(), "caller.memql") {
		t.Errorf("err %q should mention 'caller.memql' as the importing file", err.Error())
	}
}

// TestLoad_SoftDisable locks that walker rules apply: _-prefixed
// files and directories are skipped.
func TestLoad_SoftDisable(t *testing.T) {
	body := []byte(`@description("noop")
func (Query) noop(_ any) (any, error) { return nil, nil }
`)
	root := fstest.MapFS{
		"a.memql":           {Data: body},
		"_skip.memql":       {Data: body},
		"_disabled/x.memql": {Data: body},
	}
	tree, err := Load(root)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := tree.Files["_skip.memql"]; ok {
		t.Error("_skip.memql should be excluded")
	}
	if _, ok := tree.Files["_disabled/x.memql"]; ok {
		t.Error("_disabled/x.memql should be excluded")
	}
	if _, ok := tree.Files["a.memql"]; !ok {
		t.Error("a.memql should be loaded")
	}
}

// TestLoad_EmptyTree locks the no-files case.
func TestLoad_EmptyTree(t *testing.T) {
	tree, err := Load(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Load empty tree: %v", err)
	}
	if len(tree.Files) != 0 {
		t.Errorf("Files should be empty, got %d", len(tree.Files))
	}
}

func setEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]bool, len(a))
	for _, s := range a {
		m[s] = true
	}
	for _, s := range b {
		if !m[s] {
			return false
		}
	}
	return true
}
