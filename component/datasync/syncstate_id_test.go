package datasync

import (
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

// syncstate_id_test.go -- the health row's id is one the engine will store.
//
// The engine refuses a short id carrying a colon (identifiers.md), and the
// first SyncStateID spelled the concept id -- "v1:shopify:blog|..." -- into
// it. Nothing asserted the id's SHAPE, only that it was deterministic, so a
// row that could never be written passed every test.
func TestSyncStateIDIsABareSlugTheEngineAccepts(t *testing.T) {
	slug := regexp.MustCompile(`^[a-z0-9]+$`)
	a := SyncStateID("v1:shopify:blog", "shopify", "inbound", "")
	if !slug.MatchString(a) {
		t.Fatalf("SyncStateID = %q; a short id must be a bare slug (no colons, no separators the engine rejects)", a)
	}
	if b := SyncStateID("v1:shopify:blog", "shopify", "inbound", ""); b != a {
		t.Fatalf("not deterministic: %q then %q", a, b)
	}
	for _, other := range [][4]string{
		{"v1:shopify:article", "shopify", "inbound", ""},
		{"v1:shopify:blog", "shopify", "outbound", ""},
		{"v1:shopify:blog", "acme", "inbound", ""},
		{"v1:shopify:blog", "shopify", "inbound", "acme-widgets.myshopify.com"},
	} {
		if SyncStateID(other[0], other[1], other[2], other[3]) == a {
			t.Errorf("%v collides with (v1:shopify:blog, shopify, inbound)", other)
		}
	}
	if p := SyncStateID("v1:shopify:blog", "shopify", "inbound", "acme-widgets.myshopify.com"); !slug.MatchString(p) {
		t.Fatalf("a partitioned SyncStateID = %q is not a bare slug", p)
	}
}

// Every row written before partitions existed keeps its id: an empty
// partition derives from the same three-part string as before, so the
// history of that id stays one timeline.
func TestAnEmptyPartitionKeepsTheRowIdItAlwaysHad(t *testing.T) {
	want := string(id.NewUntracked().FromString("v1:shopify:product|shopify|inbound"))
	if got := SyncStateID("v1:shopify:product", "shopify", "inbound", ""); got != want {
		t.Fatalf("SyncStateID with no partition = %q, want the three-part id %q every stored row already has", got, want)
	}
	// Whitespace around a partition is not a partition.
	if got := SyncStateID("v1:shopify:product", "shopify", "inbound", "  "); got != want {
		t.Fatalf("a blank partition derived %q, want %q", got, want)
	}
	// The partition is appended as a FOURTH part.
	four := string(id.NewUntracked().FromString("v1:shopify:product|shopify|inbound|a"))
	if got := SyncStateID("v1:shopify:product", "shopify", "inbound", "a"); got != four {
		t.Fatalf("SyncStateID with partition a = %q, want the four-part id %q", got, four)
	}
}

// Two tenants of one connector are two rows: two ids, and each write names
// its own partition.
func TestTwoPartitionsOfOneDomainAreTwoRows(t *testing.T) {
	a := SyncStateID(testMirrorConcept, "shopify", "inbound", "a")
	b := SyncStateID(testMirrorConcept, "shopify", "inbound", "b")
	if a == b {
		t.Fatalf("partitions a and b share the id %q", a)
	}
	engine := newFakeEngine()
	store := NewStore(engine)
	for _, part := range []string{"a", "b"} {
		if err := store.WriteSyncState(OperatorContext(nil), SyncState{
			ConceptID: testMirrorConcept, Connector: "shopify", Direction: "inbound", Partition: part,
		}); err != nil {
			t.Fatal(err)
		}
	}
	writes := engine.callsContaining("mutation upsertSyncState")
	if len(writes) != 2 {
		t.Fatalf("writes = %d, want 2", len(writes))
	}
	for i, pair := range [][2]string{{a, "a"}, {b, "b"}} {
		if !strings.Contains(writes[i], `stateId: "`+pair[0]+`"`) || !strings.Contains(writes[i], `partitionKey: "`+pair[1]+`"`) {
			t.Errorf("write %d does not carry partition %s's own id and key: %s", i, pair[1], writes[i])
		}
	}
}
