package shopify

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// relist_tombstone_test.go -- what a full re-list may tombstone.
//
// Tombstoning is an argument from absence, and it is valid only for a
// listing that returns everything the mirror may hold. A domain whose origin
// listing is windowed or filtered is healed and never tombstoned, and a walk
// that ends in an error tombstones nothing.

func retiredCalls(h *testHarness) []string {
	var out []string
	for _, call := range h.engine.calls() {
		if strings.Contains(call, `"deleted":true`) {
			out = append(out, call)
		}
	}
	return out
}

func TestARelistOfAPartialListingTombstonesNothing(t *testing.T) {
	if len(relistListsPartially) == 0 {
		t.Fatal("no domain is named as listed partially")
	}
	for concept := range relistListsPartially {
		spec := generated.Types[concept]
		if spec == nil || spec.Reconcile != generated.ReconcileFullRelist {
			t.Errorf("%s is named as listed partially but is not a full re-list domain", concept)
			continue
		}
		h := newHarness(t)
		h.admin.reply(spec.ListOp, listPageReply(spec.ListQuery, false, "",
			map[string]any{"id": "gid://shopify/" + spec.GraphQLType + "/1"}))
		h.engine.setRows(spec.ForStoreFn, []map[string]any{
			{"id": "shp2", "gid": "gid://shopify/" + spec.GraphQLType + "/2"},
		})
		if _, err := h.conn.relist(context.Background(), h.store(t), spec); err != nil {
			t.Fatalf("%s: %v", concept, err)
		}
		if retired := retiredCalls(h); len(retired) != 0 {
			t.Errorf("%s: a domain whose listing is partial retired a row:\n%s", concept, retired[0])
		}
		if reads := h.engine.callsTo(spec.ForStoreFn); len(reads) != 0 {
			t.Errorf("%s: the live rows were read for a pass that may not tombstone", concept)
		}
	}
}

func TestARelistThatEndsInAnErrorTombstonesNothing(t *testing.T) {
	h := newHarness(t)
	h.admin.graphQLError("ShopifyListGiftCard", "INTERNAL_SERVER_ERROR", "the listing failed")
	h.engine.setRows("shopifyGiftCardForStore", []map[string]any{
		{"id": "shp2", "gid": "gid://shopify/GiftCard/2"},
	})
	if _, err := h.conn.relist(context.Background(), h.store(t), generated.Types["giftCard"]); err == nil {
		t.Fatal("a listing that failed was reported as a clean pass")
	}
	if retired := retiredCalls(h); len(retired) != 0 {
		t.Errorf("a walk that ended in an error retired a row:\n%s", retired[0])
	}
	if reads := h.engine.callsTo("shopifyGiftCardForStore"); len(reads) != 0 {
		t.Error("the live rows were read after the walk failed")
	}
}

// A tombstone is its store's: the row id, the payload's storeId and the
// write's partition all name the store whose pass retired it.
func TestATombstoneCarriesItsStoresPartition(t *testing.T) {
	w := tombstone("giftCard", testStoreID, "gid://shopify/GiftCard/2", newHarness(t).now)
	if w.Partition != testStoreID || w.Payload["storeId"] != testStoreID || w.RowId != MirrorRowID(testStoreID, "gid://shopify/GiftCard/2") || !w.Retire {
		t.Errorf("tombstone = %+v, want store %s's retirement on its partition", w, testStoreID)
	}
}
