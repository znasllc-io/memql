package datasync

import (
	"regexp"
	"testing"
)

// syncstate_id_test.go -- the health row's id is one the engine will store.
//
// The engine refuses a short id carrying a colon (identifiers.md), and the
// first SyncStateID spelled the concept id -- "v1:shopify:blog|..." -- into
// it. Nothing asserted the id's SHAPE, only that it was deterministic, so a
// row that could never be written passed every test.
func TestSyncStateIDIsABareSlugTheEngineAccepts(t *testing.T) {
	slug := regexp.MustCompile(`^[a-z0-9]+$`)
	a := SyncStateID("v1:shopify:blog", "shopify", "inbound")
	if !slug.MatchString(a) {
		t.Fatalf("SyncStateID = %q; a short id must be a bare slug (no colons, no separators the engine rejects)", a)
	}
	if b := SyncStateID("v1:shopify:blog", "shopify", "inbound"); b != a {
		t.Fatalf("not deterministic: %q then %q", a, b)
	}
	for _, other := range [][3]string{
		{"v1:shopify:article", "shopify", "inbound"},
		{"v1:shopify:blog", "shopify", "outbound"},
		{"v1:shopify:blog", "acme", "inbound"},
	} {
		if SyncStateID(other[0], other[1], other[2]) == a {
			t.Errorf("%v collides with (v1:shopify:blog, shopify, inbound)", other)
		}
	}
}
