package dslimports

// Tests for DeclarationSites (declsites.go): resolving a construct name to the
// file AND source position that declares it, which is what editor
// go-to-definition jumps to (memql#2754).

import (
	"testing"
	"testing/fstest"
)

// TestDeclarationSites_PositionOfDeclaredName pins the position scan against a
// realistically-shaped file: the declared name is the LAST identifier of the
// header, so `shape candidate candidateFull` declares candidateFull while
// `concept candidate` declares candidate.
func TestDeclarationSites_PositionOfDeclaredName(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"actions/concepts.memql": file(`@version("1.0.0")
@namespace("actions")
/// A captured trace.
concept candidate {
  id  string  @required
}`),
		"actions/shapes.memql": file(`@row
shape candidate candidateFull {
  id
}`),
	})
	idx := tree.NewIndex()

	cases := []struct {
		name     string
		wantFile string
		wantKind string
		wantLine int
		wantCol  int
	}{
		// `concept candidate {` is line 4 (after two annotations and the doc
		// comment); "concept " is 8 chars, so the name starts at column 9.
		{"candidate", "actions/concepts.memql", "concept", 4, 9},
		// `shape candidate candidateFull {` is line 2; "shape candidate " is
		// 16 chars, so candidateFull starts at column 17.
		{"candidateFull", "actions/shapes.memql", "shape", 2, 17},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sites := idx.DeclarationSites(tc.name)
			if len(sites) != 1 {
				t.Fatalf("want 1 site for %q, got %d (%+v)", tc.name, len(sites), sites)
			}
			got := sites[0]
			if got.File != tc.wantFile || got.Kind != tc.wantKind {
				t.Errorf("site = %+v, want file %s kind %s", got, tc.wantFile, tc.wantKind)
			}
			if got.Line != tc.wantLine || got.Column != tc.wantCol {
				t.Errorf("position = %d:%d, want %d:%d", got.Line, got.Column, tc.wantLine, tc.wantCol)
			}
		})
	}
}

// TestDeclarationSites_ReturnsEveryCollidingDeclaration is the reason the API
// is plural. 46 bare names collide across the engine tree; returning a single
// "best" site here would silently send the editor to the wrong file, so the
// caller (which knows the referencing file's domain) chooses.
func TestDeclarationSites_ReturnsEveryCollidingDeclaration(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"planner/concepts.memql": file(`@version("1.0.0")
@namespace("planner")
concept plan {
  id  string  @required
}`),
		"harness/concepts.memql": file(`@version("1.0.0")
@namespace("harness")
concept plan {
  id  string  @required
}`),
	})
	sites := tree.NewIndex().DeclarationSites("plan")
	if len(sites) != 2 {
		t.Fatalf("want both declarations of `plan`, got %d (%+v)", len(sites), sites)
	}
	// Sorted by file for a stable order.
	if sites[0].File != "harness/concepts.memql" || sites[1].File != "planner/concepts.memql" {
		t.Errorf("sites = %+v, want them sorted by file", sites)
	}
	for _, s := range sites {
		if s.Line != 3 || s.Column != 9 {
			t.Errorf("%s position = %d:%d, want 3:9", s.File, s.Line, s.Column)
		}
	}
}

// TestDeclarationSites_UnknownName: a name nothing declares yields nothing --
// the caller treats that as inconclusive, not proof of absence.
func TestDeclarationSites_UnknownName(t *testing.T) {
	tree := loadTree(t, fstest.MapFS{
		"actions/concepts.memql": file(`@version("1.0.0")
@namespace("actions")
concept candidate {
  id  string  @required
}`),
	})
	idx := tree.NewIndex()
	if sites := idx.DeclarationSites("nothingDeclaresThis"); len(sites) != 0 {
		t.Errorf("want no sites, got %+v", sites)
	}
	if sites := idx.DeclarationSites(""); len(sites) != 0 {
		t.Errorf("empty name must yield no sites, got %+v", sites)
	}
}

// TestDeclNamePosition_NotConfusedByBodyOrImports pins the two shapes that
// could be mistaken for a declaration header: a field inside a body (indented,
// at brace depth > 0) and a `use` line.
func TestDeclNamePosition_NotConfusedByBodyOrImports(t *testing.T) {
	const src = `use common.traits.{ isActiveRecord }

concept order {
  candidate  string
}`
	// `candidate` here is a FIELD, not a declaration -- it sits inside the
	// concept body, so the scan must not report it.
	if _, _, ok := declNamePosition(src, "candidate"); ok {
		t.Error("a field inside a construct body must not read as a declaration")
	}
	// `isActiveRecord` is imported, not declared here.
	if _, _, ok := declNamePosition(src, "isActiveRecord"); ok {
		t.Error("an imported name must not read as a declaration")
	}
	// The real declaration still resolves.
	line, col, ok := declNamePosition(src, "order")
	if !ok || line != 3 || col != 9 {
		t.Errorf("order position = %d:%d ok=%v, want 3:9 true", line, col, ok)
	}
}
