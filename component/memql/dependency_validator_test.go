package memql

import (
	"strings"
	"testing"
	"testing/fstest"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestValidateDependencyTree_RealTree asserts the shipped DSL tree
// passes the dependency-tree validator. The tree is still
// prefix-NAMED (query* / shape* etc.) but that is now just a name --
// every structural reference is concept-correct, so the validator
// must be silent on the real tree (acceptance: "the current tree still
// loads").
func TestValidateDependencyTree_RealTree(t *testing.T) {
	if err := validateDependencyTree(memqldsl.Tree()); err != nil {
		t.Fatalf("real DSL tree must pass the dependency-tree validator, got:\n%v", err)
	}
}

// TestValidateDependencyTree_PrefixFreeResolution proves a reference
// resolves WITHOUT a prefixed name: a query named `byId` (no `query`
// prefix) bound to concept `guide` projects shape `card` (no `shape`
// prefix), also of `guide`. The validator must accept it.
func TestValidateDependencyTree_PrefixFreeResolution(t *testing.T) {
	tree := fstest.MapFS{
		"guide/shapes.memql": &fstest.MapFile{Data: []byte(`
@row
shape guide card {
  row.id
  row.payload.title
}
`)},
		"guide/queries.memql": &fstest.MapFile{Data: []byte(`
use guide.shapes.{ card }

@description("get a guide by id")
query guide byId {
  args { id string @required }
  filter id==args.id
  shape card
}
`)},
	}
	if err := validateDependencyTree(tree); err != nil {
		t.Fatalf("prefix-free same-concept reference must resolve, got: %v", err)
	}
}

// TestValidateDependencyTree_CrossConceptQualified proves an explicit
// `concept.name` cross-concept reference is accepted: a query of
// concept `guide` projects `tour.card`, a shape that belongs to
// concept `tour`.
func TestValidateDependencyTree_CrossConceptQualified(t *testing.T) {
	tree := fstest.MapFS{
		"tour/shapes.memql": &fstest.MapFile{Data: []byte(`
@row
shape tour card {
  row.id
}
`)},
		"guide/queries.memql": &fstest.MapFile{Data: []byte(`
use tour.shapes.{ card }

query guide listGuides {
  filter id==args.id
  shape tour.card
}
`)},
	}
	if err := validateDependencyTree(tree); err != nil {
		t.Fatalf("qualified cross-concept reference must resolve, got: %v", err)
	}
}

// TestValidateDependencyTree_UnqualifiedCrossConceptFails proves an
// UNqualified cross-concept reference is a hard error with an
// actionable message (names the ref, its slot, the enclosing concept,
// the concept the ref belongs to, and the `concept.name` fix).
func TestValidateDependencyTree_UnqualifiedCrossConceptFails(t *testing.T) {
	tree := fstest.MapFS{
		"tour/shapes.memql": &fstest.MapFile{Data: []byte(`
@row
shape tour card {
  row.id
}
`)},
		"guide/queries.memql": &fstest.MapFile{Data: []byte(`
query guide listGuides {
  filter id==args.id
  shape card
}
`)},
	}
	err := validateDependencyTree(tree)
	if err == nil {
		t.Fatal("unqualified cross-concept shape reference must fail")
	}
	msg := err.Error()
	for _, want := range []string{
		`"card"`,         // the offending ref
		"`shape` slot",   // the slot
		`"guide"`,        // the enclosing concept
		`concept "tour"`, // the concept the ref belongs to
		`"tour.card"`,    // the suggested fix
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; full message:\n%s", want, msg)
		}
	}
}

// TestValidateDependencyTree_ActorShapeUniversal proves an @actor-only
// shape (no signature concept) is universal: any concept's query may
// project it without qualification.
func TestValidateDependencyTree_ActorShapeUniversal(t *testing.T) {
	tree := fstest.MapFS{
		"common/shapes.memql": &fstest.MapFile{Data: []byte(`
@actor
shape actorEnvelope {
  actor.userId
}
`)},
		"guide/queries.memql": &fstest.MapFile{Data: []byte(`
use common.shapes.{ actorEnvelope }

query guide whoAmI {
  filter id==args.id
  shape actorEnvelope
}
`)},
	}
	if err := validateDependencyTree(tree); err != nil {
		t.Fatalf("@actor-only shape must be universally referenceable, got: %v", err)
	}
}

// TestValidateDependencyTree_IncludeCrossConceptFails proves a shape
// that `include`s a shape of another concept (unqualified) is
// rejected.
func TestValidateDependencyTree_IncludeCrossConceptFails(t *testing.T) {
	tree := fstest.MapFS{
		"tour/shapes.memql": &fstest.MapFile{Data: []byte(`
@row
shape tour summary {
  row.id
}
`)},
		"guide/shapes.memql": &fstest.MapFile{Data: []byte(`
use tour.shapes.{ summary }

@row
shape guide card {
  include summary
  row.payload.title
}
`)},
	}
	err := validateDependencyTree(tree)
	if err == nil {
		t.Fatal("unqualified cross-concept include must fail")
	}
	if !strings.Contains(err.Error(), `"summary"`) || !strings.Contains(err.Error(), `"tour.summary"`) {
		t.Errorf("include error should name the ref + suggest the qualified form, got:\n%s", err.Error())
	}
}

// TestValidateDependencyTree_NoReferencePathsWalked pins the #2686
// invariant that made the removed `_reference/` skip dead: even a
// SYNTHETIC tree carrying an underscore-prefixed file yields a walked
// set with no such path, because dslfs.WalkMemqlFiles skips it
// structurally -- so the validator never sees one and the skip branch
// could never fire. (The differential proof: the branch was removed and
// this tree still validates identically.)
func TestValidateDependencyTree_NoReferencePathsWalked(t *testing.T) {
	tree := fstest.MapFS{
		"cognition/shapes.memql":    {Data: []byte("shape space spaceCard {\n  ownerUserId\n}\n")},
		"_reference/_shape.memql":   {Data: []byte("this is deliberately invalid DSL {{{")},
		"_reference/nested/x.memql": {Data: []byte("also invalid )))")},
	}
	// The invalid _reference/ files would make validation fail IF they
	// were walked. They are not, so validation succeeds -- which is the
	// whole reason the skip branch was dead.
	if err := validateDependencyTree(tree); err != nil {
		t.Fatalf("validation must not touch _reference/ files (walker skips them): %v", err)
	}
}
