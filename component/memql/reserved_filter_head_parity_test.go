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
// the annotation registry had two copies -- if the engine gains or loses a
// reserved head, this fails and names it, instead of the lint lane quietly
// flagging a legitimate namespace as an undeclared property (or missing a real
// typo).
func TestReservedFilterHeadsMatchEngine(t *testing.T) {
	// Every name the engine reserves must be reserved lint-side too.
	// `partition` and `schema` are deliberately excluded from this direction:
	// the engine lists them so they are not payload-prefixed, but they are
	// row INTRINSICS, and the lint lane admits them through rowIntrinsics
	// rather than through the namespace set.
	engineHeads := []string{
		"row", "payload", "actor", "args", "now", "config", "trace", "meta", "provenance",
	}
	for _, name := range engineHeads {
		if !reservedFilterHead(name) {
			t.Fatalf("test is stale: engine no longer reserves %q", name)
		}
		if !dslimports.IsReservedFilterHead(name) {
			t.Errorf("engine reserves %q but the dslimports lint copy does not -- a legitimate filter head will be reported as an undeclared property", name)
		}
	}

	// And nothing the lint side admits may be a name the engine treats as a
	// payload property, which would silently swallow a real typo.
	for _, name := range engineHeads {
		if dslimports.IsReservedFilterHead(name) && !reservedFilterHead(name) {
			t.Errorf("dslimports reserves %q but the engine payload-prefixes it -- typos under that head would go unreported", name)
		}
	}

	// Negative control: an ordinary payload property is reserved by neither.
	for _, name := range []string{"status", "ownerUserId", "rowCount"} {
		if reservedFilterHead(name) || dslimports.IsReservedFilterHead(name) {
			t.Errorf("%q must not be a reserved filter head on either side", name)
		}
	}
}
