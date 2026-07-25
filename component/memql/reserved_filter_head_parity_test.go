package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslimports"
)

// TestReservedFilterHeadsMatchEngine pins the lint-side copy of the reserved
// filter-head set (dslimports, memql#2781) against the engine's own
// reservedFilterHead.
//
// The two cannot share one definition: dslimports is consumed BY this package,
// so importing back would cycle. A drift-pin is the same device #991 used when
// the annotation registry had two copies.
//
// The pin is DERIVED, not hardcoded. A first cut iterated a hand-written list
// of the nine namespaces and passed happily when either side GAINED a head --
// decorative, since a gain is the direction that actually breaks things: a
// head the engine reserves but the lint side does not gets reported as an
// undeclared property (red CI on legal DSL), and one the lint side admits but
// the engine payload-prefixes silently swallows real typos under it. Probing a
// candidate space that spans both namespaces and ordinary property spellings
// is what makes both directions detectable.
func TestReservedFilterHeadsMatchEngine(t *testing.T) {
	candidates := []string{
		// Engine namespaces.
		"row", "payload", "actor", "args", "now", "config", "trace", "meta", "provenance",
		// Row intrinsics -- engine-reserved, so never payload-prefixed.
		"id", "concept", "type", "createdAt", "createdBy", "schema", "partition",
		// Ordinary payload properties -- classified by neither side.
		"status", "ownerUserId", "region", "kind", "name", "rowCount", "rows",
		"parent", "aliasOf", "session", "settings", "credentials", "source",
	}

	for _, name := range candidates {
		engine := reservedFilterHead(name)
		lint := dslimports.IsReservedFilterHead(name) || dslimports.IsFilterRowIntrinsic(name)

		switch {
		case engine && !lint:
			t.Errorf("engine reserves %q but the lint side does not -- a legitimate filter head would be reported as an undeclared property", name)
		case lint && !engine:
			t.Errorf("the lint side admits %q but the engine payload-prefixes it -- typos under that head would go unreported", name)
		}
	}
}

// TestFilterRowIntrinsicsExcludeWriteOnlyNames guards the specific confusion
// that made `parent` / `aliasOf` unreportable: both are legitimate INSERT
// targets (relationship templates, dslimports lane 3) but ordinary payload
// properties in a filter. Sharing one map across the read and write sides put
// them on the read allow-list, so `filter parent == args.x` against a concept
// declaring no `parent` could never be flagged.
func TestFilterRowIntrinsicsExcludeWriteOnlyNames(t *testing.T) {
	for _, name := range []string{"parent", "aliasOf"} {
		if reservedFilterHead(name) {
			t.Fatalf("test is stale: the engine now reserves %q in filters", name)
		}
		if dslimports.IsFilterRowIntrinsic(name) {
			t.Errorf("%q is a write-side relationship template, not a filter row intrinsic -- admitting it hides real typos", name)
		}
	}
}

// TestFilterHeadCaseInsensitivity -- the engine lower-cases every head it
// classifies, so `createdat` and `Row.id` are legal filters. The lint side
// matched case-sensitively at first and reported both as undeclared
// properties.
func TestFilterHeadCaseInsensitivity(t *testing.T) {
	for _, name := range []string{"createdat", "CreatedAt", "CREATEDBY", "Row", "ACTOR", "Args", "ID"} {
		if !reservedFilterHead(name) {
			t.Fatalf("test is stale: the engine no longer reserves %q", name)
		}
		if !dslimports.IsReservedFilterHead(name) && !dslimports.IsFilterRowIntrinsic(name) {
			t.Errorf("lint side rejects %q on case alone; the engine accepts it", name)
		}
	}
}
