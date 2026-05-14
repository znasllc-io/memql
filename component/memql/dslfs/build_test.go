package dslfs

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestBuildImportGraph_HappyPath locks the canonical happy path:
// resolution + default alias + graph wiring.
func TestBuildImportGraph_HappyPath(t *testing.T) {
	raw := map[string][]RawImport{
		"cognition/queries/spaceParticipants.memql": {
			{Path: "../participant"},
			{Path: "../../common/space"},
		},
		"cognition/participant.memql": {},
		"common/space.memql":          {},
	}
	graph, aliases, err := BuildImportGraph(raw)
	if err != nil {
		t.Fatalf("BuildImportGraph: %v", err)
	}

	// Verify alias resolution.
	got := aliases["cognition/queries/spaceParticipants.memql"]
	want := FileImports{
		"participant": "cognition/participant.memql",
		"space":       "common/space.memql",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("aliases = %v, want %v", got, want)
	}

	// Verify graph edges.
	out := graph.OutEdges("cognition/queries/spaceParticipants.memql")
	wantEdges := []string{"cognition/participant.memql", "common/space.memql"}
	if !reflect.DeepEqual(out, wantEdges) {
		t.Errorf("OutEdges = %v, want %v", out, wantEdges)
	}

	// Verify topo includes all files in correct order.
	order, err := graph.Topo()
	if err != nil {
		t.Fatalf("Topo: %v", err)
	}
	// The query file should come last (depends on the other two).
	last := order[len(order)-1]
	if last != "cognition/queries/spaceParticipants.memql" {
		t.Errorf("last in topo = %q, want spaceParticipants", last)
	}
}

// TestBuildImportGraph_ExplicitAlias locks the `as <name>` path,
// including alias-collision detection within a single file.
func TestBuildImportGraph_ExplicitAlias(t *testing.T) {
	raw := map[string][]RawImport{
		"foo.memql": {
			{Path: "./a", Alias: "alpha"},
			{Path: "./b", Alias: "beta"},
		},
	}
	_, aliases, err := BuildImportGraph(raw)
	if err != nil {
		t.Fatalf("BuildImportGraph: %v", err)
	}
	got := aliases["foo.memql"]
	want := FileImports{
		"alpha": "a.memql",
		"beta":  "b.memql",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("aliases = %v, want %v", got, want)
	}
}

// TestBuildImportGraph_DuplicateAliasCollision locks the
// alias-collision-within-one-file error.
func TestBuildImportGraph_DuplicateAliasCollision(t *testing.T) {
	raw := map[string][]RawImport{
		"foo.memql": {
			{Path: "./cognition/participant"},
			{Path: "./other/participant"},
		},
	}
	_, _, err := BuildImportGraph(raw)
	if err == nil {
		t.Fatal("expected alias-collision error, got nil")
	}
	if !strings.Contains(err.Error(), "alias") || !strings.Contains(err.Error(), "participant") {
		t.Errorf("error %q should mention alias + name", err.Error())
	}
}

// TestBuildImportGraph_ExplicitAliasResolvesCollision locks the
// happy path for fixing a default-alias collision by adding `as`.
func TestBuildImportGraph_ExplicitAliasResolvesCollision(t *testing.T) {
	raw := map[string][]RawImport{
		"foo.memql": {
			{Path: "./cognition/participant"},
			{Path: "./other/participant", Alias: "otherParticipant"},
		},
	}
	_, aliases, err := BuildImportGraph(raw)
	if err != nil {
		t.Fatalf("BuildImportGraph: %v", err)
	}
	got := aliases["foo.memql"]
	want := FileImports{
		"participant":      "cognition/participant.memql",
		"otherParticipant": "other/participant.memql",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("aliases = %v, want %v", got, want)
	}
}

// TestBuildImportGraph_AccumulatesErrors locks the
// errors-from-many-files behavior. A single bad import in one file
// should not stop other files from resolving.
func TestBuildImportGraph_AccumulatesErrors(t *testing.T) {
	raw := map[string][]RawImport{
		"good.memql": {
			{Path: "./common/space"},
		},
		"bad1.memql": {
			{Path: "/absolute/path"},
		},
		"bad2.memql": {
			{Path: "../../../escape"},
		},
	}
	_, _, err := BuildImportGraph(raw)
	if err == nil {
		t.Fatal("expected accumulated errors, got nil")
	}
	var be *BuildErrors
	if !errors.As(err, &be) {
		t.Fatalf("expected *BuildErrors, got %T", err)
	}
	if len(be.Errors) != 2 {
		t.Errorf("got %d errors, want 2", len(be.Errors))
	}
}

// TestBuildImportGraph_ReservedAliasDefaultRejected locks the
// reserved-name rejection through the build path.
func TestBuildImportGraph_ReservedAliasDefaultRejected(t *testing.T) {
	raw := map[string][]RawImport{
		"foo.memql": {
			{Path: "./actor"}, // basename "actor" is reserved
		},
	}
	_, _, err := BuildImportGraph(raw)
	if err == nil {
		t.Fatal("expected reserved-alias error, got nil")
	}
	if !errors.Is(err, ErrAliasReserved) {
		// errors.Is should reach through BuildErrors -> the wrapped
		// reserved-alias error.
		t.Errorf("err = %v, want errors.Is(err, ErrAliasReserved)", err)
	}
}

// TestBuildImportGraph_EmptyInput locks the no-files case.
func TestBuildImportGraph_EmptyInput(t *testing.T) {
	graph, aliases, err := BuildImportGraph(nil)
	if err != nil {
		t.Fatalf("BuildImportGraph(nil): %v", err)
	}
	if len(graph.Nodes()) != 0 {
		t.Errorf("graph should be empty, got %d nodes", len(graph.Nodes()))
	}
	if len(aliases) != 0 {
		t.Errorf("aliases should be empty, got %d entries", len(aliases))
	}
}

// TestBuildImportGraph_FilesWithNoImports locks that files with
// empty import lists still get registered as nodes (so leaves with
// no edges are not silently dropped).
func TestBuildImportGraph_FilesWithNoImports(t *testing.T) {
	raw := map[string][]RawImport{
		"a.memql": {},
		"b.memql": {},
	}
	graph, _, err := BuildImportGraph(raw)
	if err != nil {
		t.Fatalf("BuildImportGraph: %v", err)
	}
	nodes := graph.Nodes()
	if len(nodes) != 2 {
		t.Errorf("got %d nodes, want 2 (got %v)", len(nodes), nodes)
	}
}

// TestHasErrors covers the convenience helper.
func TestHasErrors(t *testing.T) {
	if HasErrors(nil) {
		t.Error("HasErrors(nil) should be false")
	}
	be := &BuildErrors{Errors: []error{errors.New("boom")}}
	if !HasErrors(be) {
		t.Error("HasErrors should detect *BuildErrors with entries")
	}
	empty := &BuildErrors{}
	if HasErrors(empty) {
		t.Error("HasErrors on empty BuildErrors should be false")
	}
}
