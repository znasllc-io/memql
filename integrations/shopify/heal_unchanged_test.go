package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// heal_unchanged_test.go -- what a reconciliation pass counts as unchanged
// (mirrorHolds). The real-engine half is heal_unchanged_db_test.go.

// A re-list counts as drift only the rows it had to write. A row the mirror
// already holds is read, compared and left alone; an ORIGIN version that moved
// is a write even when every mirrored field is the same, because updatedAt on
// a versioned type is Shopify's own and the apply guard reads it.
func TestARelistWritesAndCountsOnlyChangedRows(t *testing.T) {
	h := newHarness(t)
	card := func(updatedAt string) map[string]any {
		return map[string]any{"id": "gid://shopify/GiftCard/1", "note": "still here", "enabled": true, "updatedAt": updatedAt}
	}
	spec := generated.Types["giftCard"]
	rowID := MirrorRowID(testStoreID, "gid://shopify/GiftCard/1")
	read := fmt.Sprintf("concept==%s && id==%s", generated.ConceptID("giftCard"), langparser.QuoteString(rowID))
	inserts := func() int {
		n := 0
		for _, q := range h.engine.calls() {
			if strings.HasPrefix(q, `insert("v1:shopify:giftCard"`) {
				n++
			}
		}
		return n
	}
	relist := func(updatedAt string) memqlsync.ReconcileReport {
		t.Helper()
		h.admin.reply("ShopifyListGiftCard", listPageReply("giftCards", false, "", card(updatedAt)))
		report, err := h.conn.Reconcile(context.Background(), "v1:shopify:giftCard", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}

	// Nothing stored yet: the row is written and is drift.
	if r := relist("2026-08-23T11:00:00Z"); r.Checked != 1 || r.Healed != 1 || r.Drifted != 1 || inserts() != 1 {
		t.Fatalf("first pass: report %+v, %d insert(s)", r, inserts())
	}

	// The mirror now holds exactly that row, as of the first pass.
	stored := mapObject(spec, testStoreID, card("2026-08-23T11:00:00Z"), "", h.now)[0].Payload
	row := map[string]any{"id": rowID}
	for k, v := range stored {
		row[k] = v
	}
	h.engine.setRows(read, []map[string]any{row})

	// An hour later the origin says the same thing: read, compared, no write.
	h.now = h.now.Add(time.Hour)
	if r := relist("2026-08-23T11:00:00Z"); r.Checked != 1 || r.Healed != 0 || r.Drifted != 0 || inserts() != 1 {
		t.Fatalf("an unchanged row was healed: report %+v, %d insert(s)", r, inserts())
	}
	if len(h.engine.callsTo(read)) == 0 {
		t.Fatalf("the pass did not read the stored row; calls: %v", h.engine.calls())
	}

	// The origin's own version moved: a write, even with every field equal.
	h.now = h.now.Add(time.Hour)
	if r := relist("2026-08-23T12:00:00Z"); r.Healed != 1 || r.Drifted != 1 || inserts() != 2 {
		t.Fatalf("a moved origin version must be written: report %+v, %d insert(s)", r, inserts())
	}
}

func TestMirrorPayloadHoldsIgnoresOnlyThePassStamps(t *testing.T) {
	first := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)

	// An unversioned type: updatedAt is the fetch time, like syncedAt.
	rule := generated.Types["quantityRule"]
	ruleObj := map[string]any{"productVariant": map[string]any{"id": "gid://shopify/ProductVariant/7"}, "minimum": json.Number("2"), "isDefault": false}
	stored := mapObject(rule, testStoreID, ruleObj, "gid://shopify/PriceList/1", first)[0]
	again := mapObject(rule, testStoreID, ruleObj, "gid://shopify/PriceList/1", later)[0]
	if !fetchTimeVersion(again) || !mirrorPayloadHolds(roundTripped(t, stored.Payload), again.Payload, fetchTimeVersion(again)) {
		t.Fatal("an unversioned row that changed nothing but its pass stamps must hold")
	}
	changed := mapObject(rule, testStoreID, map[string]any{"productVariant": map[string]any{"id": "gid://shopify/ProductVariant/7"}, "minimum": json.Number("3"), "isDefault": false}, "gid://shopify/PriceList/1", later)[0]
	if mirrorPayloadHolds(roundTripped(t, stored.Payload), changed.Payload, fetchTimeVersion(changed)) {
		t.Fatal("a changed minimum must not hold")
	}

	// A versioned type whose object carries its version: compared as data.
	card := generated.Types["giftCard"]
	withVersion := func(v string) memqlsync.MirrorWrite {
		return mapObject(card, testStoreID, map[string]any{"id": "gid://shopify/GiftCard/1", "note": "n", "updatedAt": v}, "", later)[0]
	}
	cardStored := roundTripped(t, mapObject(card, testStoreID, map[string]any{"id": "gid://shopify/GiftCard/1", "note": "n", "updatedAt": "2026-08-23T11:00:00Z"}, "", first)[0].Payload)
	if w := withVersion("2026-08-23T11:00:00Z"); fetchTimeVersion(w) || !mirrorPayloadHolds(cardStored, w.Payload, false) {
		t.Fatal("the same origin version a pass later must hold")
	}
	if w := withVersion("2026-08-23T12:00:00Z"); mirrorPayloadHolds(cardStored, w.Payload, fetchTimeVersion(w)) {
		t.Fatal("a moved origin version must not hold")
	}
	// The same type with no version on the object falls back to the fetch
	// time, which is not compared.
	noVersion := mapObject(card, testStoreID, map[string]any{"id": "gid://shopify/GiftCard/1", "note": "n"}, "", later)[0]
	if !fetchTimeVersion(noVersion) {
		t.Fatal("a fetch-time updatedAt on a versioned type must be recognised as one")
	}

	// A row that is not stored never holds.
	if mirrorPayloadHolds(nil, again.Payload, true) {
		t.Fatal("a row that is not stored cannot hold")
	}
}

func TestSameMirrorValueIsJSONEqualityWithTheEnginesUnsetRule(t *testing.T) {
	for _, tc := range []struct {
		name            string
		stored, written any
		same            bool
	}{
		{"a stored float is the decoded number", float64(10), json.Number("10.0"), true},
		{"a 64-bit id that lost digits is a change", float64(12345678901234567), json.Number("12345678901234567"), false},
		{"absent and empty string are one unset value", nil, "", true},
		{"absent and an empty list are one unset value", nil, []any{}, true},
		{"empty string is not a value", "x", "", false},
		{"one instant in two spellings", "2026-08-23T11:00:00Z", "2026-08-23T11:00:00.000Z", true},
		{"two instants", "2026-08-23T11:00:00Z", "2026-08-23T11:00:01Z", false},
		{"false is a value, not unset", nil, false, false},
		{"a nested object is compared key for key", map[string]any{"amount": "9.50", "currencyCode": "USD"}, map[string]any{"amount": "9.50", "currencyCode": "USD"}, true},
		{"a stored key the write would drop is a change", map[string]any{"amount": "9.50", "note": "x"}, map[string]any{"amount": "9.50"}, false},
		{"an unset stored key the write omits is not", map[string]any{"amount": "9.50", "note": ""}, map[string]any{"amount": "9.50"}, true},
		{"lists compare in order", []any{"a", "b"}, []any{"b", "a"}, false},
	} {
		if got := sameMirrorValue(tc.stored, tc.written); got != tc.same {
			t.Errorf("%s: sameMirrorValue(%#v, %#v) = %v, want %v", tc.name, tc.stored, tc.written, got, tc.same)
		}
	}
}

// roundTripped is a payload as the engine hands it back: through JSON, with
// every number a float64, the way a protobuf Struct decodes it.
func roundTripped(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
