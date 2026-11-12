package datasync

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// shopify_version_test.go -- every mirror write carries the origin's own
// version, and a record's existence is judged by when the origin was read.
//
// The mirror refuses a write older than the row it holds. That rule is only
// as good as the versions on both sides, and for every versioned type they
// were a clock of this engine's: no generated document selected updatedAt, so
// a delivery and a sweep stamped the time they read the object, a bulk page
// the time its line was streamed, and a relist its tombstones the time they
// were written -- through a heal that did not ask the guard at all. These are
// the races that made it wrong, each played through the real connector, the
// runtime's own applier and a mirror both write into (shopify_mirror_rig_test.go).
//
// These tests were written beside the connector and moved here, to the root
// module: the integrations module cannot import the runtime whose applier
// they need. What a test used to reach inside the connector for it now asks
// through the connector's public entry points -- a delivery through Apply, a
// sweep or a relist through ReconcilePartition, an export through
// BackfillStore -- so each plays the path production plays.

// cursorAtLine is an export's cursor moved to a line: where a batch that
// stopped part-way resumes.
func cursorAtLine(t *testing.T, cursor string, line int) string {
	t.Helper()
	var c map[string]any
	if err := json.Unmarshal([]byte(cursor), &c); err != nil {
		t.Fatalf("the export's cursor %q is not what a batch resumes from: %v", cursor, err)
	}
	c["line"] = line
	raw, _ := json.Marshal(c)
	return string(raw)
}

func TestAWriteCarriesTheOriginsUpdatedAt(t *testing.T) {
	const origin = "2026-08-23T11:00:00Z"
	check := func(t *testing.T, path, version string, row map[string]any) {
		t.Helper()
		if version != origin {
			t.Errorf("%s: the write's version is %s, want the origin's updatedAt %s", path, version, origin)
		}
		if row == nil || shopStringAt(row, "updatedAt") != origin || shopStringAt(row, "versionSource") != memqlsync.VersionFromOrigin {
			t.Errorf("%s: the row holds updatedAt %q from %q, want the origin's %s", path, row["updatedAt"], row["versionSource"], origin)
		}
	}
	t.Run("delivery", func(t *testing.T) {
		r := newShopRig(t)
		r.origin.reply("ShopifyFetchProduct", map[string]any{"node": map[string]any{"__typename": "Product", "id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": origin}})
		writes, err := r.conn.Apply(context.Background(), shopNotice("products/update", "gid://shopify/Product/1", "wh-1", clockAt("11:00:00")))
		if err != nil || len(writes) == 0 {
			t.Fatalf("Apply = %v, %v", writes, err)
		}
		r.apply(writes)
		check(t, "a delivery", writes[0].Version, r.row("product", "gid://shopify/Product/1"))
	})
	t.Run("sweep", func(t *testing.T) {
		r := newShopRig(t)
		r.origin.reply("ShopifyListProduct", originListPage("products", false, "", map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": origin}))
		if _, err := r.sweep("product", clockAt("10:00:00")); err != nil {
			t.Fatal(err)
		}
		row := r.row("product", "gid://shopify/Product/1")
		check(t, "a sweep", shopStringAt(row, "updatedAt"), row)
	})
	t.Run("relist", func(t *testing.T) {
		r := newShopRig(t)
		r.origin.reply("ShopifyListOnlineStoreTheme", originListPage("themes", false, "", map[string]any{"id": "gid://shopify/OnlineStoreTheme/1", "name": "Dawn", "updatedAt": origin}))
		if _, err := r.sweep("onlineStoreTheme", time.Time{}); err != nil {
			t.Fatal(err)
		}
		row := r.row("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/1")
		check(t, "a relist", shopStringAt(row, "updatedAt"), row)
	})
	t.Run("bulk line", func(t *testing.T) {
		r := newShopRig(t)
		cursor := r.exportStarted("product")
		_, page := r.exportStreamed("product", cursor, map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": origin})
		if len(page.Writes) != 1 {
			t.Fatalf("the export wrote %d rows, want 1", len(page.Writes))
		}
		check(t, "a bulk line", page.Writes[0].Version, r.row("product", "gid://shopify/Product/1"))
	})
}

// T1: the export's snapshot. T2: a delivery records the order as it is now,
// with two of the line. T3: the export's file is streamed, holding T1's order
// with one. The row stays T2's, and both of the snapshot's rows are counted
// stale -- before, the line was stamped T3, newer than everything, and the
// order and its line went back to T1 until the origin changed them again.
func TestAnOlderBulkSnapshotDoesNotReplaceANewerDelivery(t *testing.T) {
	r := newShopRig(t)
	r.now = clockAt("10:00:30")
	cursor := r.exportStarted("order") // the snapshot is taken at T1 = 10:00

	r.now = clockAt("10:05:10")
	r.orderFetches(shopOrderNode("2026-08-23T10:05:00Z", 2))
	r.deliver(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-t2", clockAt("10:05:00")))

	r.now = clockAt("10:10:00")
	res, _ := r.exportStreamed("order", cursor, shopOrderLines("2026-08-23T10:00:00Z", 1)...)

	order, line := r.row("order", "gid://shopify/Order/1"), r.row("orderLineItem", "gid://shopify/LineItem/1")
	if got := shopStringAt(order, "updatedAt"); got != "2026-08-23T10:05:00Z" {
		t.Errorf("the order holds updatedAt %s, want the delivery's 10:05: an older snapshot replaced it", got)
	}
	if got, _ := line["quantity"].(float64); got != 2 {
		t.Errorf("the line holds quantity %v, want the delivery's 2: an older snapshot replaced it", line["quantity"])
	}
	if res.Applied != 0 || res.Stale != 2 {
		t.Errorf("the snapshot applied %d and was stale %d, want 0 and 2 (the order and its line)", res.Applied, res.Stale)
	}
}

// The order is deleted after the export's snapshot and before its file is
// streamed. The tombstone carries the origin's event time; the snapshot's
// order carries its own older updatedAt, so it does not bring the order back.
func TestABulkSnapshotDoesNotBringBackADeletedRecord(t *testing.T) {
	r := newShopRig(t)
	r.now = clockAt("09:00:05")
	r.orderFetches(shopOrderNode("2026-08-23T08:59:00Z", 1))
	r.deliver(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-0", clockAt("08:59:00")))

	r.now = clockAt("10:00:30")
	cursor := r.exportStarted("order")

	r.now = clockAt("10:05:02")
	r.deliver(shopNotice("orders/delete", "gid://shopify/Order/1", "wh-del", clockAt("10:05:00")))
	if r.row("order", "gid://shopify/Order/1")["deleted"] != true {
		t.Fatal("the delete did not retire the order, so this test proves nothing")
	}

	r.now = clockAt("10:10:00")
	r.exportStreamed("order", cursor, shopOrderLines("2026-08-23T08:59:00Z", 1)...)
	if r.row("order", "gid://shopify/Order/1")["deleted"] != true {
		t.Error("an export whose snapshot predates the delete brought the deleted order back")
	}
}

// A sweep lists the product as it was; while its page is out, a delivery
// records the product as it is now. The sweep's heal refuses the older state
// by the applier's own comparison instead of writing it over the delivery's.
func TestASweepDoesNotReplaceWhatADeliveryWroteMeanwhile(t *testing.T) {
	r := newShopRig(t)
	r.now = clockAt("10:05:30")
	r.origin.reply("ShopifyFetchProduct", map[string]any{"node": map[string]any{"__typename": "Product", "id": "gid://shopify/Product/1", "title": "Two", "updatedAt": "2026-08-23T10:05:00Z"}})
	r.origin.replyWith("ShopifyListProduct", func(map[string]any) any {
		// The page holds the product as the origin listed it; the delivery
		// lands before the sweep writes it.
		r.deliverMeanwhile(shopNotice("products/update", "gid://shopify/Product/1", "wh-meanwhile", clockAt("10:05:00")))
		return originListPage("products", false, "", map[string]any{"id": "gid://shopify/Product/1", "title": "One", "updatedAt": "2026-08-23T10:00:00Z"})
	})
	if _, err := r.sweep("product", clockAt("09:00:00")); err != nil {
		t.Fatal(err)
	}
	row := r.row("product", "gid://shopify/Product/1")
	if shopStringAt(row, "title") != "Two" || shopStringAt(row, "updatedAt") != "2026-08-23T10:05:00Z" {
		t.Errorf("the product holds %q at %s, want the delivery's \"Two\" at 10:05: the sweep wrote an older state over it", row["title"], row["updatedAt"])
	}
}

// A relist retires what its listing did not name. A theme a delivery wrote
// after the listing began is not in the listing and not gone: the relist's
// tombstone carries the time its listing began, older than that theme's
// version, and is refused. A theme that was there before the listing began
// and is not in it is retired as before.
func TestARelistDoesNotRetireWhatADeliveryWroteAfterItBegan(t *testing.T) {
	r := newShopRig(t)
	r.now = clockAt("10:00:00")
	r.put("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/1", map[string]any{"name": "Dawn", "updatedAt": "2026-08-23T08:00:00Z", "versionSource": "origin", "deleted": false})
	r.put("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/3", map[string]any{"name": "Gone", "updatedAt": "2026-08-23T08:00:00Z", "versionSource": "origin", "deleted": false})
	r.origin.reply("ShopifyFetchOnlineStoreTheme", map[string]any{"node": map[string]any{"__typename": "OnlineStoreTheme", "id": "gid://shopify/OnlineStoreTheme/2", "name": "New", "updatedAt": "2026-08-23T10:00:20Z"}})
	r.origin.replyWith("ShopifyListOnlineStoreTheme", func(map[string]any) any {
		r.deliverMeanwhile(shopNotice("themes/create", "gid://shopify/OnlineStoreTheme/2", "wh-new", clockAt("10:00:20")))
		return originListPage("themes", false, "", map[string]any{"id": "gid://shopify/OnlineStoreTheme/1", "name": "Dawn", "updatedAt": "2026-08-23T08:00:00Z"})
	})
	if _, err := r.sweep("onlineStoreTheme", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if row := r.row("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/2"); row == nil || row["deleted"] == true {
		t.Errorf("the relist retired the theme a delivery wrote after its listing began: %v", row)
	}
	if row := r.row("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/3"); row["deleted"] != true {
		t.Errorf("the relist kept a theme its listing no longer names: %v", row)
	}
	if row := r.row("onlineStoreTheme", "gid://shopify/OnlineStoreTheme/1"); row["deleted"] == true {
		t.Errorf("the relist retired a theme its listing names: %v", row)
	}
}

// A delete retried after the record was written back by a page whose
// snapshot predates the delete, and an update retried after the record was
// deleted whose fetch now finds nothing: both retire the record.
func TestARetriedDeleteStillRetiresTheRecord(t *testing.T) {
	t.Run("a delete retried after an older snapshot was applied", func(t *testing.T) {
		r := newShopRig(t)
		r.now = clockAt("09:00:05")
		r.orderFetches(shopOrderNode("2026-08-23T08:59:00Z", 1))
		r.deliver(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-0", clockAt("08:59:00")))
		r.now = clockAt("10:00:30")
		cursor := r.exportStarted("order")

		// The delete arrives at 10:05 and its write fails; the runtime tries
		// it again later.
		r.now = clockAt("10:05:02")
		del := shopNotice("orders/delete", "gid://shopify/Order/1", "wh-del", clockAt("10:05:00"))
		if _, err := r.conn.Apply(context.Background(), del); err != nil {
			t.Fatal(err)
		}
		r.conn.ForgetInbound(context.Background(), del)

		r.now = clockAt("10:10:00")
		r.exportStreamed("order", cursor, shopOrderLines("2026-08-23T08:59:00Z", 1)...)

		r.now = clockAt("10:11:00")
		r.deliver(del)
		if row := r.row("order", "gid://shopify/Order/1"); row["deleted"] != true {
			t.Errorf("the retried delete left the deleted order live: updatedAt %v", row["updatedAt"])
		}
	})
	t.Run("an update retried after the record was deleted", func(t *testing.T) {
		r := newShopRig(t)
		r.readsAllOrders()
		// A (10:00) fails and waits for its retry; B (10:01) lands.
		r.now = clockAt("10:01:05")
		r.orderFetches(shopOrderNode("2026-08-23T10:01:00Z", 1))
		r.deliver(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-b", clockAt("10:01:00")))

		// The order is deleted at 10:02 and the delete delivery is lost. A's
		// retry fetches nothing, which is asked again; the next retry still
		// finds nothing, and that is a deletion.
		r.now = clockAt("10:03:00")
		r.orderFetches(nil)
		a := shopNotice("orders/updated", "gid://shopify/Order/1", "wh-a", clockAt("10:00:00"))
		a.Attempts = 1
		if _, err := r.conn.Apply(context.Background(), a); !memqlsync.IsAnsweredNothing(err) {
			t.Fatalf("A's first empty answer: err = %v, want it asked again", err)
		}
		r.now = clockAt("10:08:00")
		a.Attempts, a.AnsweredNothingBefore = 2, true
		r.deliver(a)
		if row := r.row("order", "gid://shopify/Order/1"); row["deleted"] != true {
			t.Errorf("an update whose fetch found nothing left the deleted order live: updatedAt %v", row["updatedAt"])
		}
	})
}

// THE SCRIPTED SHOPIFY ANSWERS ONLY WHAT IT WAS ASKED. The fault these tests
// were written for lived for months behind a green suite because every fake
// handed updatedAt back whether the document selected it or not. The
// connector's own fake has this guard (integrations/shopify); the one the
// composition tests use is a second fake and needs its own: a reply or a bulk
// line that carries a version the document does not select is a failure of
// the test that scripted it, and the field is not in the answer.
func TestTheScriptedShopifyRefusesAVersionTheDocumentDidNotSelect(t *testing.T) {
	r := newShopRig(t)
	var told []string
	r.origin.mu.Lock()
	r.origin.unselected = func(message string) { told = append(told, message) }
	r.origin.mu.Unlock()
	ctx := context.Background()
	client := shopify.NewAdminClient()
	ask := func(document string) map[string]any {
		t.Helper()
		resp, err := client.Do(ctx, r.store(), "shpat_test", document, "Probe", nil)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := resp.DataMap()
		shop, _ := data["shop"].(map[string]any)
		return shop
	}

	r.origin.reply("Probe", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1", "updatedAt": "2026-08-23T11:00:00Z"}})
	shop := ask("query Probe {\n  shop {\n    id\n  }\n}")
	if len(told) != 1 || !strings.Contains(told[0], "updatedAt") {
		t.Errorf("a reply carrying an unselected updatedAt was told %v, want one failure naming it", told)
	}
	if shop["updatedAt"] != nil {
		t.Errorf("the unselected updatedAt was answered: %v", shop)
	}

	told = nil
	if shop := ask("query Probe {\n  shop {\n    id\n    updatedAt\n  }\n}"); len(told) != 0 || shop["updatedAt"] != "2026-08-23T11:00:00Z" {
		t.Errorf("a selected updatedAt: told %v, answered %v", told, shop)
	}

	// Selected for a nested object only: the parent's is still unselected.
	told = nil
	if shop := ask("query Probe {\n  shop {\n    id\n    owner {\n      updatedAt\n    }\n  }\n}"); len(told) != 1 || shop["updatedAt"] != nil {
		t.Errorf("an updatedAt selected only for a nested object: told %v, answered %v", told, shop)
	}

	told = nil
	r.origin.mu.Lock()
	r.origin.bulkDocuments = append(r.origin.bulkDocuments, "query ShopifyBulkProbe {\n  things {\n    edges {\n      node {\n        id\n      }\n    }\n  }\n}")
	r.origin.mu.Unlock()
	link := r.origin.serveJSONL("/download/probe.jsonl", originBulkLines(map[string]any{"id": "gid://shopify/Thing/1", "updatedAt": "2026-08-23T11:00:00Z"}))
	got, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	var line map[string]any
	_ = json.NewDecoder(got.Body).Decode(&line)
	if len(told) != 1 || line["updatedAt"] != nil {
		t.Errorf("a bulk line carrying an unselected updatedAt: told %v, served %v", told, line)
	}
}

// ---- a record's existence is judged by when the origin was read ----

// Only a clean answer is a deletion: `node: null` and no error at all, here
// on the delivery's retry (a first clean answer is asked again,
// TestAnUnannouncedDeletionIsConfirmedBeforeItIsBelieved). Every
// other way a fetch can come back without the object -- the origin refusing
// the node itself (how Shopify refuses protected customer data on
// node(id:)), a denial left after the prune limit, a reply with no data or
// no node, an error that is not a denial -- retires nothing, and is a
// refusal or a failure the retry lane and the store's reasons already know.
func TestEveryAnswerOfAFetchIsADeletionOnlyWhenClean(t *testing.T) {
	protected := shopify.GraphQLError{
		Message:    "This app is not approved to access the Order object. See https://shopify.dev/docs/apps/launch/protected-customer-data for more details.",
		Extensions: map[string]any{"code": "ACCESS_DENIED"},
		Path:       []any{"node"},
	}
	deniedName := shopify.GraphQLError{
		Message:    "Access denied for name field. Required access: `read_publications` access scope.",
		Extensions: map[string]any{"code": "ACCESS_DENIED"},
		Path:       []any{"node", "name"},
	}
	cases := []struct {
		name      string
		reply     any
		errs      []shopify.GraphQLError
		retired   bool
		refused   bool // not granted, and never tried again
		retryable bool
	}{
		{name: "clean: node null, no error", reply: map[string]any{"node": nil}, retired: true},
		{name: "the node refused (protected data)", reply: map[string]any{"node": nil}, errs: []shopify.GraphQLError{protected}, refused: true},
		{name: "a denied field still nulling the node after the prune limit", reply: map[string]any{"node": nil}, errs: []shopify.GraphQLError{deniedName}, refused: true},
		{name: "data null, no error", reply: nil, retryable: true},
		{name: "no node in the data, no error", reply: map[string]any{}, retryable: true},
		{name: "an error that is not a denial", reply: map[string]any{"node": nil},
			errs: []shopify.GraphQLError{{Message: "Internal error", Extensions: map[string]any{"code": "INTERNAL_SERVER_ERROR"}}}, retryable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newShopRig(t)
			r.readsAllOrders()
			r.now = clockAt("10:00:00")
			r.held("order", "gid://shopify/Order/1", "2026-08-23T09:00:00Z", clockAt("09:30:00"))
			r.origin.reply("ShopifyFetchOrder", tc.reply)
			if tc.errs != nil {
				r.origin.fail("ShopifyFetchOrder", tc.errs...)
			}
			writes, err := r.conn.Apply(context.Background(), shopRetried(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-1", clockAt("09:59:00"))))
			if err == nil {
				r.apply(writes)
			}
			retired := r.row("order", "gid://shopify/Order/1")["deleted"] == true
			if retired != tc.retired {
				t.Fatalf("retired = %t (err %v), want %t", retired, err, tc.retired)
			}
			switch {
			case tc.refused && !(memqlsync.IsNotGranted(err) && memqlsync.IsPermanent(err)):
				t.Errorf("err = %v, want a refusal (not granted, not retried)", err)
			case tc.retryable && (err == nil || memqlsync.IsPermanent(err)):
				t.Errorf("err = %v, want a failure the retry lane tries again", err)
			case tc.retired && err != nil:
				t.Errorf("err = %v on a clean answer", err)
			}
		})
	}
}

// A create delivery can arrive before its record is readable. An empty fetch
// for a record the mirror does not hold is asked once more, and has nothing to
// retire, so it writes nothing on either try; the record arrives with the
// next sweep that lists it.
func TestAnEmptyFetchForARecordNotHeldWritesNothing(t *testing.T) {
	r := newShopRig(t)
	r.now = clockAt("10:00:00")
	r.orderFetches(nil)
	create := shopNotice("orders/create", "gid://shopify/Order/1", "wh-create", clockAt("10:00:00"))
	if writes, err := r.conn.Apply(context.Background(), create); !memqlsync.IsAnsweredNothing(err) || len(writes) != 0 {
		t.Fatalf("the first empty answer: writes %v, err %v; want it asked once more", writes, err)
	}
	writes, err := r.conn.Apply(context.Background(), shopRetried(create))
	if err != nil {
		t.Fatal(err)
	}
	r.apply(writes)
	if row := r.row("order", "gid://shopify/Order/1"); row != nil {
		t.Fatalf("an empty fetch for a record the mirror never held wrote %v", row)
	}
	r.now = clockAt("10:10:00")
	r.origin.reply("ShopifyListOrder", originListPage("orders", false, "", map[string]any{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": "2026-08-23T10:00:00Z"}))
	if _, err := r.sweep("order", clockAt("09:00:00")); err != nil {
		t.Fatal(err)
	}
	if row := r.row("order", "gid://shopify/Order/1"); row == nil || row["deleted"] == true {
		t.Errorf("the sweep did not write the record: %v", row)
	}
}

// A clean empty fetch at F can still be wrong (a record briefly unreadable).
// The tombstone it writes is a statement about F, so the next read of the
// origin after F that finds the record puts it back -- although its updatedAt
// has not moved, and is older than F.
func TestARecordWronglySeenAsGoneComesBackOnTheNextRead(t *testing.T) {
	for _, path := range []string{"sweep", "delivery"} {
		t.Run(path, func(t *testing.T) {
			r := newShopRig(t)
			r.readsAllOrders()
			r.now = clockAt("09:30:00")
			r.held("order", "gid://shopify/Order/1", "2026-08-23T09:00:00Z", clockAt("09:30:00"))
			r.now = clockAt("10:00:00")
			r.orderFetches(nil)
			r.deliver(shopRetried(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-f", clockAt("09:59:00"))))
			if r.row("order", "gid://shopify/Order/1")["deleted"] != true {
				t.Fatal("the clean empty fetch did not retire the held order, so this test proves nothing")
			}
			r.now = clockAt("10:05:00")
			switch path {
			case "sweep":
				r.origin.reply("ShopifyListOrder", originListPage("orders", false, "", map[string]any{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": "2026-08-23T09:00:00Z"}))
				if _, err := r.sweep("order", clockAt("08:00:00")); err != nil {
					t.Fatal(err)
				}
			case "delivery":
				r.orderFetches(shopOrderNode("2026-08-23T09:00:00Z", 1))
				r.deliver(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-back", clockAt("10:04:00")))
			}
			if row := r.row("order", "gid://shopify/Order/1"); row["deleted"] == true {
				t.Errorf("a %s that read the order after the empty fetch left it retired", path)
			}
		})
	}
}

// A bulk line is a statement about the moment its export was started. One
// started before a tombstone's time does not bring the record back -- even
// when its updatedAt is the very second of the tombstone -- whether the
// tombstone came from a delete topic or from a clean empty fetch.
func TestABulkLineExportedBeforeATombstoneDoesNotBringTheRecordBack(t *testing.T) {
	for _, kind := range []string{"delete topic", "empty fetch"} {
		t.Run(kind, func(t *testing.T) {
			r := newShopRig(t)
			r.readsAllOrders()
			r.now = clockAt("09:30:00")
			r.held("order", "gid://shopify/Order/1", "2026-08-23T09:00:00Z", clockAt("09:30:00"))
			r.now = clockAt("10:00:30")
			cursor := r.exportStarted("order")
			// Updated and deleted inside one second, 10:05:00.
			r.now = clockAt("10:05:00")
			switch kind {
			case "delete topic":
				r.deliver(shopNotice("orders/delete", "gid://shopify/Order/1", "wh-del", clockAt("10:04:58")))
			case "empty fetch":
				r.orderFetches(nil)
				r.deliver(shopRetried(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-upd", clockAt("10:04:58"))))
			}
			r.now = clockAt("10:10:00")
			r.exportStreamed("order", cursor, shopOrderLines("2026-08-23T10:05:00Z", 1)...)
			if r.row("order", "gid://shopify/Order/1")["deleted"] != true {
				t.Error("a line from an export started before the tombstone brought the order back")
			}
		})
	}
}

// One started after the tombstone's time read the origin after it, and the
// record in it is there: it comes back, though its updatedAt is older.
func TestABulkLineExportedAfterATombstoneBringsTheRecordBack(t *testing.T) {
	r := newShopRig(t)
	r.readsAllOrders()
	r.now = clockAt("09:30:00")
	r.held("order", "gid://shopify/Order/1", "2026-08-23T09:00:00Z", clockAt("09:30:00"))
	r.now = clockAt("10:00:00")
	r.orderFetches(nil)
	r.deliver(shopRetried(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-f", clockAt("09:59:00"))))
	if r.row("order", "gid://shopify/Order/1")["deleted"] != true {
		t.Fatal("the clean empty fetch did not retire the held order, so this test proves nothing")
	}
	r.now = clockAt("10:30:00")
	cursor := r.exportStarted("order")
	r.now = clockAt("10:40:00")
	r.exportStreamed("order", cursor, shopOrderLines("2026-08-23T09:00:00Z", 1)...)
	if r.row("order", "gid://shopify/Order/1")["deleted"] == true {
		t.Error("a line from an export started after the tombstone left the order retired")
	}
}

// A version that is a read time and one that is the origin's are never
// compared. A reverse fulfilment order publishes no version and its return is
// versioned by createdAt, which never moves, so it hands nothing down: read
// directly by its own topic it holds a read time, and a later read of it
// under its return is applied. A bulk line whose parent line was not seen
// holds a read time, and the parent's delivery is applied over it.
func TestVersionsOfDifferentKindsAreNeverCompared(t *testing.T) {
	t.Run("a reverse fulfilment order, then its return", func(t *testing.T) {
		r := newShopRig(t)
		const rfo = "gid://shopify/ReverseFulfillmentOrder/1"
		r.now = clockAt("10:00:00")
		r.origin.reply("ShopifyFetchReverseFulfillmentOrder", map[string]any{"node": map[string]any{"__typename": "ReverseFulfillmentOrder", "id": rfo, "status": "OPEN"}})
		r.deliver(shopNotice("reverse_fulfillment_orders/dispose", rfo, "wh-rfo", clockAt("10:00:00")))
		r.now = clockAt("10:05:00")
		r.origin.reply("ShopifyFetchShopifyReturn", map[string]any{"node": map[string]any{
			"__typename": "Return", "id": "gid://shopify/Return/1", "createdAt": "2026-08-20T00:00:00Z", "order": map[string]any{"id": "gid://shopify/Order/1"},
			"reverseFulfillmentOrders": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
				"nodes": []any{map[string]any{"id": rfo, "status": "CLOSED"}}},
		}})
		res := r.deliver(shopNotice("returns/update", "gid://shopify/Return/1", "wh-ret", clockAt("10:05:00")))
		if row := r.row("reverseFulfillmentOrder", rfo); shopStringAt(row, "status") != "CLOSED" {
			t.Errorf("the reverse fulfilment order holds %v (stale %d), want the later read's CLOSED", row["status"], res.Stale)
		}
	})
	t.Run("a bulk line without its parent, then the parent's delivery", func(t *testing.T) {
		r := newShopRig(t)
		// The export was started a minute ago, and its batch resumes at a line
		// whose parent line is not the last one before it: the line is read
		// with no version of its parent's.
		r.now = clockAt("09:59:00")
		cursor := cursorAtLine(t, r.exportStarted("order"), 1)
		r.now = clockAt("10:00:00")
		page := r.exportRead("order", cursor,
			map[string]any{"id": "gid://shopify/Order/2", "name": "#1002", "updatedAt": "2026-08-23T09:00:00Z"},
			map[string]any{"id": "gid://shopify/LineItem/1", "name": "Tee", "quantity": 1, "__parentId": "gid://shopify/Order/1"},
		)
		r.apply(page.Writes)
		line := r.row("orderLineItem", "gid://shopify/LineItem/1")
		if line == nil || shopStringAt(line, "versionSource") == memqlsync.VersionFromOrigin {
			t.Fatalf("the line with no parent seen is %v; it must be written, and not with an origin version, or this test proves nothing", line)
		}
		r.now = clockAt("10:05:00")
		r.orderFetches(shopOrderNode("2026-08-23T09:00:00Z", 3))
		r.deliver(shopNotice("orders/updated", "gid://shopify/Order/1", "wh-parent", clockAt("10:05:00")))
		if line := r.row("orderLineItem", "gid://shopify/LineItem/1"); line["quantity"] != float64(3) {
			t.Errorf("the line holds quantity %v, want its order's delivery's 3", line["quantity"])
		}
	})
}

// A DELETION THE ORIGIN DID NOT ANNOUNCE IS CONFIRMED BEFORE IT IS BELIEVED.
// One clean empty answer on an update topic retires nothing: on a domain swept
// by updated_at, a record wrongly retired by it has no scheduled read that
// brings it back. The delivery fails as worth another try, and the retry that
// still finds nothing retires the record; one that finds it writes it. A
// delete topic is the origin's announcement and retires at once.
func TestAnUnannouncedDeletionIsConfirmedBeforeItIsBelieved(t *testing.T) {
	const gid = "gid://shopify/Order/1"
	setup := func(t *testing.T) *shopRig {
		r := newShopRig(t)
		r.readsAllOrders()
		r.now = clockAt("10:00:00")
		r.held("order", gid, "2026-08-23T09:00:00Z", clockAt("09:30:00"))
		return r
	}
	t.Run("the first try", func(t *testing.T) {
		r := setup(t)
		r.orderFetches(nil)
		writes, err := r.conn.Apply(context.Background(), shopNotice("orders/updated", gid, "wh-1", clockAt("09:59:00")))
		if err == nil || memqlsync.IsPermanent(err) || memqlsync.IsNotGranted(err) {
			t.Fatalf("err = %v (writes %v), want a failure worth another try", err, writes)
		}
		if !strings.Contains(err.Error(), "asked once more before that answer is believed") {
			t.Errorf("err = %v, want the plain reason", err)
		}
		if r.row("order", gid)["deleted"] == true {
			t.Error("one clean empty answer retired the held record")
		}
	})
	t.Run("the retry, still nothing", func(t *testing.T) {
		r := setup(t)
		r.orderFetches(nil)
		r.now = clockAt("10:01:00")
		r.deliver(shopRetried(shopNotice("orders/updated", gid, "wh-1", clockAt("09:59:00"))))
		if r.row("order", gid)["deleted"] != true {
			t.Error("the retry's clean empty answer did not retire the record")
		}
	})
	t.Run("the retry finds the record", func(t *testing.T) {
		r := setup(t)
		r.orderFetches(shopOrderNode("2026-08-23T10:00:30Z", 2))
		r.now = clockAt("10:01:00")
		r.deliver(shopRetried(shopNotice("orders/updated", gid, "wh-1", clockAt("09:59:00"))))
		if row := r.row("order", gid); row["deleted"] == true || shopStringAt(row, "updatedAt") != "2026-08-23T10:00:30Z" {
			t.Errorf("the retry that found the record left %v", row)
		}
	})
	t.Run("a delete topic", func(t *testing.T) {
		r := setup(t)
		r.deliver(shopNotice("orders/delete", gid, "wh-del", clockAt("09:59:58")))
		if r.row("order", gid)["deleted"] != true {
			t.Error("a delete topic did not retire the record on its first try")
		}
	})
}

// A DELETE TOPIC'S TOMBSTONE allows for the clocks of two nodes: its receipt
// is taken on the node that received the webhook, the row's read on the node
// that fetched. It is refused only by a read made more than
// AnnouncedRetireAllowance after its receipt. A receipt an earlier build
// staged at whole seconds counts as the end of its second.
func TestTheDeleteTopicsTombstoneAllowsForClocksApart(t *testing.T) {
	const gid = "gid://shopify/Order/1"
	sub := func(clock string) time.Time {
		d, err := time.ParseDuration(strings.TrimPrefix(clock[8:], "+"))
		if err != nil {
			panic(err)
		}
		return clockAt(clock[:8]).Add(d)
	}
	cases := []struct {
		name          string
		read, receipt time.Time
		retired       bool
	}{
		{"a read inside the receipt's second, before it", sub("10:00:00+300ms"), sub("10:00:00+700ms"), true},
		{"a whole-second receipt from the earlier build, read later in that second", sub("10:00:00+300ms"), clockAt("10:00:00"), true},
		{"a receipt two seconds before the read", clockAt("10:00:02"), clockAt("10:00:00"), true},
		{"a row read ninety seconds after the receipt", clockAt("10:01:30"), clockAt("10:00:00"), false},
		// Inside the allowance only because a whole-second receipt counts as
		// the end of its second: 60.5 s after its start, 59.5 s after its end.
		{"a whole-second receipt, read sixty and a half seconds after its start", sub("10:01:00+500ms"), clockAt("10:00:00"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newShopRig(t)
			r.now = clockAt("10:02:00")
			r.held("order", gid, "2026-08-23T09:00:00Z", tc.read)
			req := shopNotice("orders/delete", gid, "wh-del", clockAt("09:59:00"))
			req.ReceivedAt = tc.receipt
			r.deliver(req)
			if retired := r.row("order", gid)["deleted"] == true; retired != tc.retired {
				t.Errorf("retired = %t, want %t", retired, tc.retired)
			}
		})
	}
}

// liveOrder is an order as the origin answers it with no lines: one mirror
// write, the order's own.
func liveOrder(gid, updatedAt string) map[string]any {
	return map[string]any{"__typename": "Order", "id": gid, "name": "#1001", "updatedAt": updatedAt}
}

// BETWEEN TWO LIVE WRITES OF THE SAME ORIGIN VERSION THE LATER READ WINS. A
// read of the same state made before the one the row holds would move the
// row's read time back, and a tombstone must be after the row's last read.
// The earlier read is a delivery fetched at 10:01, of a row read at 10:05.
func TestEqualVersionsTheLaterReadWins(t *testing.T) {
	r := newShopRig(t)
	const gid = "gid://shopify/Order/1"
	r.held("order", gid, "2026-08-23T10:00:00Z", clockAt("10:05:00"))
	before := shopStringAt(r.row("order", gid), "syncedAt")
	r.now = clockAt("10:01:00")
	r.orderFetches(liveOrder(gid, "2026-08-23T10:00:00Z"))
	res := r.deliver(shopNotice("orders/updated", gid, "wh-earlier-read", clockAt("10:00:58")))
	if res.Stale != 1 || res.Applied != 0 || shopStringAt(r.row("order", gid), "syncedAt") != before {
		t.Errorf("applied %d, stale %d, syncedAt %v (was %s): an equal version read earlier must be refused", res.Applied, res.Stale, r.row("order", gid)["syncedAt"], before)
	}
}

// A LIVE ROW FROM BEFORE THE RULE refuses no live write, but keeps its
// syncedAt as its read time against a tombstone: one older than it is
// refused, a newer one retires it. The tombstone is the one this engine
// concludes from its own reads: the second clean empty answer in a row,
// fetched at that time.
func TestARowFromBeforeTheRuleKeepsItsReadTimeForATombstone(t *testing.T) {
	const gid = "gid://shopify/Order/1"
	for _, tc := range []struct {
		name    string
		at      time.Time
		retired bool
	}{
		{"a tombstone older than its sync time", clockAt("09:59:00"), false},
		{"a newer one", clockAt("10:01:00"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newShopRig(t)
			r.readsAllOrders()
			r.put("order", gid, map[string]any{"name": "#1001", "updatedAt": "2026-08-23T10:00:00Z", "syncedAt": "2026-08-23T10:00:00Z", "deleted": false})
			r.now = tc.at
			r.orderFetches(nil)
			res := r.deliver(shopRetried(shopNotice("orders/updated", gid, "wh-1", tc.at.Add(-time.Minute))))
			if res.Applied+res.Stale != 1 {
				t.Fatalf("the second empty answer produced %d writes, want the one tombstone, or this test proves nothing", res.Applied+res.Stale)
			}
			if retired := r.row("order", gid)["deleted"] == true; retired != tc.retired {
				t.Errorf("retired = %t, want %t", retired, tc.retired)
			}
		})
	}
}

// "SAYS SO TWICE" IS TWO CLEAN EMPTY ANSWERS IN A ROW, not a second try. A
// first try that failed for an ordinary reason (a throttle) makes the next
// try the record's first empty answer, which is asked again. The dispatcher
// says whether the previous try ended in an answer of nothing
// (AnsweredNothingBefore); this plays its part, as
// TestTheNextTryIsToldWhenTheLastOneAnsweredNothing pins it.
func TestTwoCleanEmptyAnswersInARowAreADeletion(t *testing.T) {
	const gid = "gid://shopify/Order/1"
	run := func(t *testing.T, steps ...string) (retiredOn int, row map[string]any) {
		t.Helper()
		r := newShopRig(t)
		r.readsAllOrders()
		r.now = clockAt("10:00:00")
		r.held("order", gid, "2026-08-23T09:00:00Z", clockAt("09:30:00"))
		attempts, nothingBefore := 0, false
		for i, step := range steps {
			r.now = clockAt("10:00:00").Add(time.Duration(i+1) * time.Minute)
			if step == "requeue" {
				// An operator puts the given-up delivery back in line: no
				// tries counted, and the count of answers starts again.
				attempts, nothingBefore = 0, false
				continue
			}
			r.origin.httpStatus("ShopifyFetchOrder", 0)
			switch step {
			case "empty":
				r.orderFetches(nil)
			case "throttle":
				r.origin.httpStatus("ShopifyFetchOrder", http.StatusServiceUnavailable)
			case "found":
				r.orderFetches(shopOrderNode("2026-08-23T09:00:00Z", 2))
			}
			req := shopNotice("orders/updated", gid, "wh-1", clockAt("09:59:00"))
			req.Attempts, req.AnsweredNothingBefore = attempts, nothingBefore
			writes, err := r.conn.Apply(context.Background(), req)
			if err == nil {
				r.apply(writes)
			} else if memqlsync.IsPermanent(err) {
				t.Fatalf("try %d (%s): a permanent failure %v, want one worth another try", i+1, step, err)
			}
			if retiredOn == 0 && r.row("order", gid)["deleted"] == true {
				retiredOn = len(steps[:i+1])
			}
			attempts, nothingBefore = attempts+1, memqlsync.IsAnsweredNothing(err)
		}
		return retiredOn, r.row("order", gid)
	}
	for _, tc := range []struct {
		steps []string
		want  int // the step that retired the record, 0 for none
	}{
		{[]string{"empty", "empty"}, 2},
		{[]string{"throttle", "empty"}, 0},
		{[]string{"throttle", "empty", "empty"}, 3},
		{[]string{"empty", "throttle", "empty"}, 0},
		{[]string{"empty", "found"}, 0},
		{[]string{"empty", "requeue", "empty"}, 0},
	} {
		t.Run(strings.Join(tc.steps, ","), func(t *testing.T) {
			got, row := run(t, tc.steps...)
			if got != tc.want {
				t.Errorf("retired on step %d, want %d (row %v)", got, tc.want, row)
			}
			if tc.steps[len(tc.steps)-1] == "found" && (row["deleted"] == true || row["name"] != "#1001") {
				t.Errorf("the record found was not written: %v", row)
			}
		})
	}
}

// AN ANNOUNCED DELETION IS AS HARD TO UNDO AS IT IS TO REFUSE. A read made
// just before a delete, on a node whose clock runs ahead, carries a time after
// the delete's receipt: against an announced tombstone a live write applies
// only when it was read more than the allowance after it. A tombstone this
// engine concluded from its own reads keeps the strict rule, so a record
// wrongly seen as gone comes back on the next read.
//
// The announced tombstone is a delete notice received at `receipt`. The one
// this engine concluded is the second clean empty answer in a row, fetched at
// `receipt`. The live write after it is a delivery that finds the order,
// fetched `after` later.
func TestAnAnnouncedDeletionIsAsHardToUndoAsToRefuse(t *testing.T) {
	const gid = "gid://shopify/Order/1"
	receipt := clockAt("10:00:00").Add(400 * time.Millisecond)
	for _, tc := range []struct {
		name      string
		announced bool
		after     time.Duration
		back      bool
	}{
		{"announced, a read two seconds after", true, 2 * time.Second, false},
		{"announced, a read ninety seconds after", true, 90 * time.Second, true},
		{"not announced, a read two seconds after", false, 2 * time.Second, true},
		{"not announced, a read ninety seconds after", false, 90 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newShopRig(t)
			r.readsAllOrders()
			r.now = clockAt("10:05:00")
			r.held("order", gid, "2026-08-23T09:00:00Z", clockAt("09:30:00"))
			if tc.announced {
				del := shopNotice("orders/delete", gid, "wh-del", clockAt("09:59:59"))
				del.ReceivedAt = receipt
				r.deliver(del)
			} else {
				r.now = receipt
				r.orderFetches(nil)
				r.deliver(shopRetried(shopNotice("orders/updated", gid, "wh-gone", clockAt("09:59:59"))))
			}
			tomb := r.row("order", gid)
			if tomb["deleted"] != true {
				t.Fatal("the tombstone did not land, so this test proves nothing")
			}
			if announced := tomb["versionSource"] == memqlsync.VersionFromAnnouncement; announced != tc.announced {
				t.Fatalf("the tombstone is %v, which is not the kind this case is about", tomb)
			}
			r.now = receipt.Add(tc.after)
			r.orderFetches(liveOrder(gid, "2026-08-23T09:00:00Z"))
			r.deliver(shopNotice("orders/updated", gid, "wh-live", clockAt("09:59:00")))
			if back := r.row("order", gid)["deleted"] != true; back != tc.back {
				t.Errorf("came back = %t, want %t", back, tc.back)
			}
		})
	}
	t.Run("a bulk line whose export started before an announced tombstone", func(t *testing.T) {
		r := newShopRig(t)
		r.now = clockAt("09:59:00")
		r.held("order", gid, "2026-08-23T09:00:00Z", clockAt("09:30:00"))
		cursor := r.exportStarted("order")
		r.now = clockAt("10:05:00")
		del := shopNotice("orders/delete", gid, "wh-del", clockAt("09:59:59"))
		del.ReceivedAt = receipt
		r.deliver(del)
		r.now = clockAt("10:10:00")
		r.exportStreamed("order", cursor, shopOrderLines("2026-08-23T09:00:00Z", 1)...)
		if r.row("order", gid)["deleted"] != true {
			t.Error("a line from an export started before the announced deletion brought the order back")
		}
	})
}
