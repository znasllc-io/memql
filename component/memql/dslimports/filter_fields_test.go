package dslimports

import (
	"strings"
	"testing"
	"testing/fstest"
)

// filterFieldTree builds a minimal one-domain tree: a concept with two
// declared properties, a trait, and whatever queries the caller supplies.
func filterFieldTree(queries string) fstest.MapFS {
	return fstest.MapFS{
		"lab/concepts.memql": &fstest.MapFile{Data: []byte(
			"/// A lab widget.\nconcept widget {\n" +
				"  region       string  @description(\"Region.\")\n" +
				"  ownerUserId  string  @description(\"Owner.\")\n" +
				"  settings     object  @description(\"Nested settings.\")\n" +
				"}\n")},
		"lab/specs.memql": &fstest.MapFile{Data: []byte(
			"/// Matches active rows.\ntrait labIsActive {\n  return active == true\n}\n")},
		"lab/queries.memql": &fstest.MapFile{Data: []byte(queries)},
	}
}

func filterFieldErrs(t *testing.T, queries string) []error {
	t.Helper()
	tree, err := Load(filterFieldTree(queries))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var out []error
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "filter") {
			out = append(out, e)
		}
	}
	return out
}

// TestFilterFieldsRejectsUndeclaredProperty is the defect memql#2781 reports:
// a misspelled payload property in a filter compiles to a JSONB path that
// cannot exist, so the query returns zero rows forever with no error at
// parse, load, lint, or boot.
func TestFilterFieldsRejectsUndeclaredProperty(t *testing.T) {
	errs := filterFieldErrs(t, `use lab.concepts.{ widget }

/// Widgets owned by the caller -- with a typo'd property.
query widget widgetsOwned {
  filter  ownerUserid == actor.userId
}
`)
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 filter-field error, got %d: %v", len(errs), errs)
	}
	msg := errs[0].Error()
	for _, want := range []string{"widgetsOwned", "ownerUserid", "widget"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must name %q to be actionable, got: %s", want, msg)
		}
	}
}

// TestFilterFieldsAcceptsLegitimateHeads guards against over-reach. Every one
// of these is a legal filter head and must NOT be flagged:
//
//   - a declared payload property, bare (epic #2292);
//   - a nested path into a declared object property;
//   - the `row.` intrinsic namespace (memql#2779);
//   - the reserved engine heads on the RHS (actor / args / now / config);
//   - a trait called bare as a predicate -- the parser emits these as
//     SpecReferenceExpr, not as a field reference, which is what makes this
//     lane tractable at all;
//   - a `when(args.x) { ... }` guard's inner predicate.
func TestFilterFieldsAcceptsLegitimateHeads(t *testing.T) {
	errs := filterFieldErrs(t, `use lab.concepts.{ widget }
use lab.specs.{ labIsActive }

/// Every legal filter head in one predicate.
query widget widgetsLegit {
  args {
    region  string
  }
  filter  ownerUserId == actor.userId && settings.theme == "dark" && row.id != "" && labIsActive && when(args.region) { region == args.region }
}
`)
	if len(errs) != 0 {
		t.Fatalf("expected no filter-field errors, got %d: %v", len(errs), errs)
	}
}

// TestFilterFieldsCatchesTypoInsideGuardAndDisjunct -- boolean shape must not
// hide an undeclared property. A `when()` guard body and an `||` disjunct are
// both real places a typo lands.
func TestFilterFieldsCatchesTypoInsideGuardAndDisjunct(t *testing.T) {
	errs := filterFieldErrs(t, `use lab.concepts.{ widget }

/// Typo buried inside a guard.
query widget widgetsGuarded {
  args {
    region  string
  }
  filter  when(args.region) { regionn == args.region }
}

/// Typo buried inside a disjunct.
query widget widgetsDisjunct {
  filter  (ownerUserId == actor.userId || regoin == "emea") && region != ""
}
`)
	if len(errs) != 2 {
		t.Fatalf("expected 2 filter-field errors (guard + disjunct), got %d: %v", len(errs), errs)
	}
}

// TestFilterFieldsSkipsImportedConstructCompared pins the false positive the
// existing suite caught (TestVerify_ImportsOnlyTargetSkipped): an imported
// construct COMPARED to a value parses as a comparison LHS, shape-identical to
// a payload property. Skipping in-scope construct names is what keeps the lane
// off a file the engine loads happily.
func TestFilterFieldsSkipsImportedConstructCompared(t *testing.T) {
	errs := filterFieldErrs(t, `use lab.concepts.{ widget }
use lab.specs.{ labIsActive }

/// Compares an imported construct rather than calling it bare.
query widget widgetsCompared {
  filter  ownerUserId == actor.userId && labIsActive == true
}
`)
	if len(errs) != 0 {
		t.Fatalf("an imported construct compared to a value is not a payload property; got %d: %v", len(errs), errs)
	}
}

// TestFilterFieldsSkipsSameDomainSpecWithoutImport pins the tree's real
// convention: a same-domain spec/trait is called with NO import
// (dsl/forge/queries.memql does exactly this for forgeDeveloper /
// forgeApprover). Resolving construct scope through imports alone would report
// such a name as an undeclared property the moment anyone COMPARES it rather
// than calling it bare.
func TestFilterFieldsSkipsSameDomainSpecWithoutImport(t *testing.T) {
	errs := filterFieldErrs(t, `use lab.concepts.{ widget }

/// Compares a same-domain trait that is deliberately NOT imported.
query widget widgetsLocalSpec {
  filter  ownerUserId == actor.userId && labIsActive == true
}
`)
	if len(errs) != 0 {
		t.Fatalf("a same-domain spec/trait needs no import to be in scope; got %d: %v", len(errs), errs)
	}
}

// TestFilterFieldsWalksRelationshipExpr pins memql#2795-review F2. The engine's
// rewriteFilterFieldRefs walks RelationshipExpression, so a bare property under
// childOf(...) IS payload-prefixed -- omitting the node from the lint walker
// left the lane blind to the exact defect it exists to catch.
func TestFilterFieldsWalksRelationshipExpr(t *testing.T) {
	errs := filterFieldErrs(t, `use lab.concepts.{ widget }

/// Typo hidden under a relationship wrapper.
query widget widgetsRelated {
  filter  region != "" && childOf(bogusUnderRel == "x")
}
`)
	if len(errs) != 1 {
		t.Fatalf("expected the typo under childOf() to be reported, got %d: %v", len(errs), errs)
	}
}

// TestFilterFieldsRejectsWriteOnlyIntrinsics pins F3. `parent` / `aliasOf` are
// insert-side relationship templates; in a FILTER the engine payload-prefixes
// them like any property, so sharing lane 3's rowIntrinsics made them
// unreportable.
func TestFilterFieldsRejectsWriteOnlyIntrinsics(t *testing.T) {
	errs := filterFieldErrs(t, `use lab.concepts.{ widget }

/// Filters on a write-side template the concept does not declare.
query widget widgetsParent {
  filter  parent == "x"
}
`)
	if len(errs) != 1 {
		t.Fatalf("expected `parent` to be reported as undeclared, got %d: %v", len(errs), errs)
	}
}

// TestFilterFieldsAcceptsMixedCaseHeads pins F5. The engine lower-cases every
// head it classifies, so these are all legal filters; matching
// case-sensitively reported them as undeclared properties.
func TestFilterFieldsAcceptsMixedCaseHeads(t *testing.T) {
	errs := filterFieldErrs(t, `use lab.concepts.{ widget }

/// Intrinsics and namespaces spelled with non-canonical case.
query widget widgetsMixedCase {
  filter  createdat != "" && Row.id != "" && region != ""
}
`)
	if len(errs) != 0 {
		t.Fatalf("mixed-case heads are legal to the engine; got %d: %v", len(errs), errs)
	}
}

// TestFilterFieldsSkipsDuplicateQueryNames pins F6. With two queries sharing a
// name in one file, the name->concept map cannot say which binding belongs to
// which AST node; a last-wins guess reported a real property of one as
// undeclared on the other's concept.
func TestFilterFieldsSkipsDuplicateQueryNames(t *testing.T) {
	tree, err := Load(fstest.MapFS{
		"lab/concepts.memql": &fstest.MapFile{Data: []byte(dupConceptsSrc)},
		"lab/queries.memql": &fstest.MapFile{Data: []byte(`use lab.concepts.{ widget, gadget }

/// First.
query widget dup {
  filter  region == "x"
}

/// Second, same name.
query gadget dup {
  filter  size == "y"
}
`)},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "filter compares field") {
			t.Errorf("must not guess a concept for duplicate query names, got: %v", e)
		}
	}
}

// TestFilterFieldsSkipsUnresolvableConcept keeps the lane conservative, the
// house rule for this file: a query whose concept the tree cannot resolve
// (supplied externally via MEMQL_DSL_PATH, or reported by lane 2) must not
// produce speculative field errors on top.
func TestFilterFieldsSkipsUnresolvableConcept(t *testing.T) {
	tree, err := Load(fstest.MapFS{
		"lab/queries.memql": &fstest.MapFile{Data: []byte(`use cognition.concepts.{ space }

/// Bound to a concept this tree does not carry.
query space spacesExternal {
  filter  whateverField == "x"
}
`)},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "filter") {
			t.Errorf("must not report filter-field errors for an unresolvable concept, got: %v", e)
		}
	}
}

// dupConceptsSrc declares two concepts with disjoint properties, so a
// wrong-concept diagnostic is unmistakable: `region` belongs to widget only,
// `size` to gadget only.
const dupConceptsSrc = `/// A widget.
concept widget {
  region  string  @description("Region.")
}

/// A gadget.
concept gadget {
  size  string  @description("Size.")
}
`

// TestFilterFieldsExplicitImportWinsOverAmbientDomain pins memql#2795-review
// F1. The same-domain fallback exists for an unimported ambiguous short name,
// but it must not override an EXPLICIT import -- boot resolves the import
// first (concept_resolver.go consults namespaceHintForName before the ambient
// domain).
//
// The failure it prevents is the documented product-bundle case: a file
// importing an engine concept from a namespace absent from the linted root,
// while a same-named concept happens to exist in its own domain. Reporting the
// engine concept's properties as undeclared is red CI on legal DSL.
func TestFilterFieldsExplicitImportWinsOverAmbientDomain(t *testing.T) {
	tree, err := Load(fstest.MapFS{
		"acme/concepts.memql": &fstest.MapFile{Data: []byte(
			"/// A local request, unrelated to the imported one.\nconcept request {\n  localProp  string  @description(\"Local.\")\n}\n")},
		"acme/queries.memql": &fstest.MapFile{Data: []byte(`use cognition.concepts.{ request }

/// Bound to the IMPORTED request, whose namespace this root does not carry.
query request findRequest {
  filter  channel == args.channel
}
`)},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "filter compares field") {
			t.Errorf("an explicit import must win over the ambient domain; got: %v", e)
		}
	}
}

// TestFilterFieldsReportsDuplicateQueryNames pins F2. Skipping a duplicate
// silently left BOTH queries' filters unchecked with no trace, and the
// coverage guard -- which exists precisely to make silent skips impossible --
// counted them as covered because it derived the name map independently.
func TestFilterFieldsReportsDuplicateQueryNames(t *testing.T) {
	tree, err := Load(fstest.MapFS{
		"lab/concepts.memql": &fstest.MapFile{Data: []byte(dupConceptsSrc)},
		"lab/queries.memql": &fstest.MapFile{Data: []byte(`use lab.concepts.{ widget, gadget }

/// First.
query widget dup {
  filter  region == "x"
}

/// Second, same name.
query gadget dup {
  filter  size == "y"
}
`)},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	reported := false
	for _, e := range tree.VerifyReferentialIntegrity() {
		if strings.Contains(e.Error(), "declared more than once") {
			reported = true
		}
		if strings.Contains(e.Error(), "filter compares field") {
			t.Errorf("must not guess a concept for a duplicate name; got: %v", e)
		}
	}
	if !reported {
		t.Error("a duplicate query name must be reported, not silently skipped")
	}

	// And the coverage probe must agree that those queries are NOT covered.
	total, skipped := tree.QueryFilterCoverage()
	if total != 2 || len(skipped) != 2 {
		t.Errorf("coverage must count both duplicates as skipped; got total=%d skipped=%v", total, skipped)
	}
}
