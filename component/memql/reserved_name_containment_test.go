package memql

import (
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestReservedPayloadFieldsSupersetOfFilterHeads pins the containment that
// memql#3613 restored: every name the plan parser treats as an engine
// namespace at the head of a filter path must ALSO be refused as a concept's
// top-level payload property.
//
// The two lists are halves of one contract, and they had drifted by eight
// names -- `row`, `actor`, `args`, `now`, `config`, `trace`, `meta`,
// `provenance`. Each was declarable on a concept, and the resulting field was
// unreachable by any filter that named it: the parser resolves the head to the
// namespace and never payload-prefixes it. Two of the eight did worse than
// being unreachable. `provenance` compiled to `provenance->>'<leaf>'`, the
// row's own engine-stamped column, and the in-process post-filter agreed on
// that same wrong field -- so the predicate matched the wrong rows with no
// error anywhere. `actor.userId == args.v` compiled to `? = ?` over the
// caller's own envelope and a caller-supplied argument, which const-folds to
// true whenever a caller passes their own id: the predicate contributes
// nothing and the query degenerates to every row of the concept.
//
// The assertion is DERIVED from the filter side rather than written out. A
// hardcoded list passes when reservedFilterHead GAINS a head, which is exactly
// the direction that reopens the gap.
func TestReservedPayloadFieldsSupersetOfFilterHeads(t *testing.T) {
	heads := make([]string, 0, len(reservedFilterHeadNames)+len(intrinsicFieldRegistry))
	for name := range reservedFilterHeadNames {
		heads = append(heads, name)
	}
	for name := range intrinsicFieldRegistry {
		heads = append(heads, name)
	}
	if len(heads) == 0 {
		t.Fatal("no filter heads found, so this gate measures nothing")
	}

	for _, name := range heads {
		if !memorynodes.IsReservedPayloadField(name) {
			t.Errorf("reservedFilterHead reserves %q but a concept may still declare it as a payload property.\n\n"+
				"Add it to reservedPayloadFields in component/database/memory-nodes/constants.go. "+
				"Left as-is, a concept declaring %q registers with the field intact and every filter "+
				"naming it bare addresses the engine namespace instead of the payload -- silently for "+
				"`provenance` (wrong rows, no error) and dangerously for `actor` (the predicate "+
				"const-folds away and the query returns everything).", name, name)
		}
	}
}

// TestReservedFilterHeadStillReadsItsOwnList guards the refactor that made the
// containment test possible: reservedFilterHead used to carry its namespaces
// in a switch, and lifting them into reservedFilterHeadNames is only safe if
// the function still classifies every one of them. A map nobody consults would
// leave the derived test above measuring a list with no behaviour behind it.
func TestReservedFilterHeadStillReadsItsOwnList(t *testing.T) {
	for name := range reservedFilterHeadNames {
		if !reservedFilterHead(name) {
			t.Errorf("reservedFilterHeadNames lists %q but reservedFilterHead(%q) = false", name, name)
		}
		// The engine lower-cases every head it classifies, so the mixed-case
		// spelling has to travel with it -- otherwise `Actor.userId` slips
		// past the namespace check and gets payload-prefixed.
		if upper := strings.ToUpper(name); !reservedFilterHead(upper) {
			t.Errorf("reservedFilterHead(%q) = false; head classification must be case-insensitive", upper)
		}
	}
}
