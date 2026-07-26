package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// memql#2804: lane 5 validates the fields a query `filter` compares, but a
// spec body was never walked -- so the same typo went silent the moment the
// predicate moved into a spec.
//
// A bare spec/trait predicate parses as a SpecReferenceExpr, not a field
// reference, which is exactly what makes lane 5 tractable (it never has to
// guess whether a bare identifier is a property or a construct). The cost is
// that the spec's own body is unchecked, and a spec is where authorization
// predicates live.
//
// A spec binds one shape XOR concept in its signature (#2281), so the
// binding needed to check the body is right there. Traits are deliberately
// unbound and stay out of scope here.

// specBodyTree builds a one-domain tree: a concept, an @actor shape, and
// whatever specs the caller supplies.
func specBodyTree(specs string) fstest.MapFS {
	return fstest.MapFS{
		"lab/concepts.memql": &fstest.MapFile{Data: []byte(
			"/// A lab widget.\nconcept widget {\n" +
				"  region       string  @description(\"Region.\")\n" +
				"  ownerUserId  string  @description(\"Owner.\")\n" +
				"}\n")},
		"lab/shapes.memql": &fstest.MapFile{Data: []byte(
			"/// Actor identity envelope.\n@actor\nshape labActor {\n" +
				"  actor.userId\n  actor.role\n}\n")},
		"lab/specs.memql": &fstest.MapFile{Data: []byte(specs)},
	}
}

func specBodyErrs(t *testing.T, specs string) []error {
	t.Helper()
	tree, err := Load(specBodyTree(specs))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out []error
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "spec") && strings.Contains(e.Error(), "does not") {
			out = append(out, e)
		}
	}
	return out
}

// The defect: a misspelled field in an @actor-bound spec body. Every live
// spec in the tree is @actor-bound, so this is the case that actually
// occurs -- and the authorization predicates are exactly the ones where a
// silently-wrong field matters most.
func TestSpecBodyRejectsUndeclaredActorShapeKey(t *testing.T) {
	errs := specBodyErrs(t, `use lab.shapes.{ labActor }

/// Caller must be an admin -- with a typo'd envelope key.
spec labActor requiresAdmin {
  return rolle == "admin"
}
`)
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 spec-body error, got %d: %v", len(errs), errs)
	}
	msg := errs[0].Error()
	for _, want := range []string{"requiresAdmin", "rolle", "labActor"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message must name %q; got: %s", want, msg)
		}
	}
}

// The concept-bound form. No live instance today -- the corpus is all
// @actor-bound -- but it is the form the authoring reference teaches, and
// it is the one lane 5 mirrors exactly.
func TestSpecBodyRejectsUndeclaredConceptProperty(t *testing.T) {
	errs := specBodyErrs(t, `use lab.concepts.{ widget }

/// Rows in a region -- with a typo'd property.
spec widget inRegion {
  return regoin == "eu"
}
`)
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 spec-body error, got %d: %v", len(errs), errs)
	}
	if msg := errs[0].Error(); !strings.Contains(msg, "regoin") || !strings.Contains(msg, "widget") {
		t.Errorf("message must name the field and the concept; got: %s", msg)
	}
}

// Correct bodies must stay silent, on both binding kinds.
func TestSpecBodyAcceptsDeclaredFields(t *testing.T) {
	errs := specBodyErrs(t, `use lab.shapes.{ labActor }
use lab.concepts.{ widget }

/// Caller must be an admin.
spec labActor requiresAdmin {
  return role == "admin"
}

/// Caller is a known user.
spec labActor knownUser {
  return userId != ""
}

/// Rows in a region.
spec widget inRegion {
  return region == "eu"
}
`)
	if len(errs) != 0 {
		t.Fatalf("correct spec bodies must not be flagged, got: %v", errs)
	}
}

// A trait is deliberately UNBOUND -- bare payload fields validated at the
// call site -- so it has no binding to check against and must be skipped
// rather than guessed at. Flagging it would be a false positive on every
// trait in the tree.
func TestSpecBodySkipsTraits(t *testing.T) {
	errs := specBodyErrs(t, `
/// Matches active rows. Unbound by design.
trait labIsActive {
  return active == true
}
`)
	if len(errs) != 0 {
		t.Fatalf("traits are unbound by design and must be skipped, got: %v", errs)
	}
}

// Same conservatism as lane 5: when the binding does not resolve, skip.
// Lane 2 reports the unresolved binding, and guessing here would produce a
// second, wronger diagnostic -- and would fire on a product bundle whose
// binding lives in a namespace absent from the linted root.
func TestSpecBodySkipsUnresolvableBinding(t *testing.T) {
	errs := specBodyErrs(t, `
/// Bound to something this tree does not declare.
spec nowhereShape orphan {
  return whatever == "x"
}
`)
	if len(errs) != 0 {
		t.Fatalf("an unresolvable binding must be skipped, got: %v", errs)
	}
}

// A spec body may compose other specs and traits by bare name; those are
// construct references, not fields, and must not be reported as undeclared
// properties.
func TestSpecBodyAcceptsConstructReferences(t *testing.T) {
	errs := specBodyErrs(t, `use lab.shapes.{ labActor }

/// Caller must be an admin.
spec labActor requiresAdmin {
  return role == "admin"
}

/// Composed predicate referencing another spec by bare name.
spec labActor requiresAdminToo {
  return requiresAdmin
}
`)
	if len(errs) != 0 {
		t.Fatalf("a bare construct reference must not be read as a field, got: %v", errs)
	}
}

// An EMPTY shape body is the default-projection form (#2035 C5): the loader
// expands it at bootstrap to every projectable field of the bound concept.
// Reading the empty path list literally flagged EVERY field the spec reads
// -- a guaranteed false positive on documented, working DSL, with no name
// collision or product bundle needed (memql#2804 review).
func TestSpecBodyAcceptsDefaultProjectionShape(t *testing.T) {
	tree, err := Load(fstest.MapFS{
		"lab/concepts.memql": &fstest.MapFile{Data: []byte(
			"/// A lab widget.\nconcept widget {\n  region string  @description(\"Region.\")\n}\n")},
		"lab/shapes.memql": &fstest.MapFile{Data: []byte(
			"/// Default projection of every field.\n@row\nshape widget widgetFull {\n}\n")},
		"lab/specs.memql": &fstest.MapFile{Data: []byte(
			"/// Rows in a region.\nspec widgetFull inRegion {\n  return region == \"eu\"\n}\n")},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "inRegion") {
			t.Errorf("default-projection shape must not flag its concept's fields: %v", e)
		}
	}
}

// A shape-bound spec may read PROJECTED KEYS ONLY. The engine's
// shapeFieldMapper has no intrinsic escape hatch, so admitting `createdAt`
// here would lint clean and then refuse at boot -- the lane would be
// reporting a laxer contract than the engine enforces.
func TestSpecBodyRejectsIntrinsicOnShapeBoundSpec(t *testing.T) {
	errs := specBodyErrs(t, `use lab.shapes.{ labActor }

/// Reads a row intrinsic through an @actor binding.
spec labActor recentActor {
  return createdAt != ""
}
`)
	if len(errs) != 1 {
		t.Fatalf("a shape-bound spec must not admit row intrinsics, got %d: %v", len(errs), errs)
	}
	if msg := errs[0].Error(); !strings.Contains(msg, "createdAt") {
		t.Errorf("message must name the field; got: %s", msg)
	}
}

// A concept-bound spec IS a row predicate, so it may name the intrinsics a
// filter may -- same rule lane 5 applies.
func TestSpecBodyAcceptsIntrinsicOnConceptBoundSpec(t *testing.T) {
	errs := specBodyErrs(t, `use lab.concepts.{ widget }

/// Rows created after a cutoff.
spec widget recentWidget {
  return createdAt != ""
}
`)
	if len(errs) != 0 {
		t.Fatalf("a concept-bound row spec may name intrinsics, got: %v", errs)
	}
}

// A binding imported from a namespace this root does not carry is supplied
// externally -- the product-bundle case. Resolving it against an unrelated
// local shape that merely shares a short name would report undeclared fields
// on legal DSL, which is the failure resolveFilterConcept's explicit-import
// guard exists to prevent.
func TestSpecBodySkipsExternallySuppliedBinding(t *testing.T) {
	tree, err := Load(fstest.MapFS{
		// A bundle-local shape that happens to share the engine shape's name.
		"catalog/shapes.memql": &fstest.MapFile{Data: []byte(
			"/// Unrelated local shape.\n@row\nshape labActor {\n  sku\n  price\n}\n")},
		"orders/specs.memql": &fstest.MapFile{Data: []byte(
			"use common.shapes.{ labActor }\n\n" +
				"/// Bound to the ENGINE shape, absent from this root.\n" +
				"spec labActor requiresAdmin {\n  return role == \"admin\"\n}\n")},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "requiresAdmin") && strings.Contains(e.Error(), "does not declare") {
			t.Errorf("an externally-supplied binding must be skipped, not resolved against a "+
				"same-named local shape: %v", e)
		}
	}
}

// A construct name elsewhere in the tree must not mask a typo. Lane 5 admits
// construct names because a query filter genuinely compares them; a spec body
// does not -- the engine's mapper resolves fields only -- so admitting them
// here would silence a real typo whenever some unrelated trait shared its
// spelling (memql#2804 review).
func TestSpecBodyTypoNotMaskedByUnrelatedConstruct(t *testing.T) {
	tree, err := Load(fstest.MapFS{
		"lab/shapes.memql": &fstest.MapFile{Data: []byte(
			"/// Actor envelope.\n@actor\nshape labActor {\n  actor.role\n}\n")},
		// An unrelated trait in another namespace sharing the typo's spelling.
		"other/traits.memql": &fstest.MapFile{Data: []byte(
			"/// Unrelated.\ntrait roles {\n  return active == true\n}\n")},
		"lab/specs.memql": &fstest.MapFile{Data: []byte(
			"/// Typo'd envelope key that collides with a trait name.\n" +
				"spec labActor requiresAdmin {\n  return roles == \"admin\"\n}\n")},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "requiresAdmin") && strings.Contains(e.Error(), "roles") {
			found = true
		}
	}
	if !found {
		t.Error("a typo colliding with an unrelated construct name must still be reported")
	}
}
