package datasync

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// shopify_empty_answer_test.go -- what a clean answer of nothing does, played
// through the connector AND the runtime's dispatcher, because half of each
// rule is what the dispatcher stamps on the delivery: processed, or failed
// with a next try.
//
// Moved here from the connector's package with the rest of the tests that
// need the runtime (shopify_version_test.go says why).

// shopStamped is the one status write a dispatch left on a delivery.
type shopStamped struct {
	status, lastError, nextAttemptAt string
	attempts                         int
	// counted says the write named the try count at all: a held delivery's
	// does not.
	counted bool
}

// due reports whether the retry pass would ever take the delivery again.
func (s shopStamped) due() bool { return s.status == "failed" && s.nextAttemptAt != "" }

var shopStampArgRE = regexp.MustCompile(`(\w+): ("(?:[^"\\]|\\.)*"|-?\d+)`)

// shopLane is the runtime's dispatcher over the rig's engine, with the rig's
// connector the one bound under its name: a delivery worked through it is
// applied behind the guard and stamped as production stamps it.
type shopLane struct {
	rig *shopRig
	d   *Dispatcher
}

func (r *shopRig) lane() *shopLane {
	r.t.Helper()
	memqlsync.UnbindForTest(shopify.ConnectorName)
	if err := memqlsync.Bind(r.conn); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { memqlsync.UnbindForTest(shopify.ConnectorName) })
	store := NewStore(shopSeam{r.mirror})
	return &shopLane{rig: r, d: NewDispatcher(store, NewApplier(store, NewEngineMirrorWriter(shopSeam{r.mirror})))}
}

// try dispatches req as the staged row it would be after `before`, the stamp
// its last try left (the zero value for a first try), and returns the stamp
// this try leaves.
func (l *shopLane) try(req memqlsync.InboundRequest, before shopStamped) (shopStamped, error) {
	t := l.rig.t
	t.Helper()
	headers, err := json.Marshal(req.Headers)
	if err != nil {
		t.Fatal(err)
	}
	seen := len(l.rig.mirror.callsTo("updateInboundRequestStatus"))
	_, dispatchErr := l.d.DispatchStaged(context.Background(), StagedRequest{
		RequestId: req.RequestId, Source: req.Source, Body: req.Body, HeadersJSON: string(headers),
		ReceivedAt: req.ReceivedAt.UTC().Format(time.RFC3339Nano), VerifiedBy: memqlsync.VerifiedByConnector, SignatureVerified: true,
		Attempts: before.attempts, Status: before.status, LastError: before.lastError,
	})
	stamps := l.rig.mirror.callsTo("updateInboundRequestStatus")[seen:]
	if len(stamps) != 1 {
		t.Fatalf("the delivery was stamped %d times by one try, want 1: %v", len(stamps), stamps)
	}
	out := shopStamped{attempts: before.attempts}
	for _, m := range shopStampArgRE.FindAllStringSubmatch(stamps[0][strings.IndexByte(stamps[0], '('):], -1) {
		value := m[2]
		if strings.HasPrefix(value, `"`) {
			value, _ = strconv.Unquote(value)
		}
		switch m[1] {
		case "status":
			out.status = value
		case "lastError":
			out.lastError = value
		case "nextAttemptAt":
			out.nextAttemptAt = value
		case "attempts":
			out.attempts, _ = strconv.Atoi(value)
			out.counted = true
		}
	}
	return out, dispatchErr
}

// AN ORDER OLDER THAN SIXTY DAYS IS NOT RETIRED BY THE NOTICE THAT TOUCHES IT.
// Without read_all_orders the origin hides such an order and answers a clean
// null for it, so a late refund's update notice would be answered nothing, and
// nothing brings the order back: the sweep and the export list the same sixty
// days. For order data under such a grant an unannounced empty answer writes
// nothing and is not asked again; the delivery is processed.
func TestAnEmptyAnswerDoesNotRetireOrderDataTheGrantCannotReadPastSixtyDays(t *testing.T) {
	const gid = "gid://shopify/Order/1"
	for _, tc := range []struct {
		name   string
		scopes []string
	}{
		{"a grant without read_all_orders", []string{"read_orders", "read_products"}},
		{"a store whose grant was never recorded", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newShopRig(t)
			if tc.scopes != nil {
				r.granted(tc.scopes...)
			}
			l := r.lane()
			r.now = clockAt("10:00:00")
			r.held("order", gid, "2026-08-23T09:00:00Z", clockAt("09:30:00"))
			before := r.row("order", gid)["syncedAt"]
			r.orderFetches(nil)
			req := shopNotice("orders/updated", gid, "wh-late-refund", clockAt("09:59:00"))

			first, err := l.try(req, shopStamped{})
			if err != nil {
				t.Fatalf("the first empty answer failed the delivery: %v", err)
			}
			if first.status != "processed" || first.due() {
				t.Fatalf("the first empty answer left the delivery %+v, want processed and not due again", first)
			}
			// And were it asked again all the same, a second clean null in a
			// row retires nothing either.
			r.conn.ForgetInbound(context.Background(), req)
			writes, err := r.conn.Apply(context.Background(), shopRetried(req))
			if err != nil || len(writes) != 0 {
				t.Fatalf("a second empty answer in a row: writes %v, err %v; want nothing", writes, err)
			}
			if row := r.row("order", gid); row["deleted"] == true || row["syncedAt"] != before {
				t.Errorf("the held order is %v, want it live and untouched", row)
			}
			if n := r.mirrorWrites(); n != 0 {
				t.Errorf("%d rows were written, want none", n)
			}
			if n := r.origin.countOp("ShopifyFetchOrder"); n != 2 {
				t.Errorf("the order was fetched %d times, want once for each try", n)
			}
		})
	}
}

// WITH read_all_orders THE ORIGIN HIDES NOTHING, so two clean empty answers in
// a row are a deletion, as for every other kind.
func TestAnEmptyAnswerStillRetiresAnOrderWhenTheGrantReadsAllOrders(t *testing.T) {
	const gid = "gid://shopify/Order/1"
	r := newShopRig(t)
	r.granted("read_orders", "read_all_orders")
	l := r.lane()
	r.now = clockAt("10:00:00")
	r.held("order", gid, "2026-08-23T09:00:00Z", clockAt("09:30:00"))
	r.orderFetches(nil)
	req := shopNotice("orders/updated", gid, "wh-1", clockAt("09:59:00"))

	first, err := l.try(req, shopStamped{})
	if err == nil || !first.due() || !memqlsync.AnsweredNothingBefore(first.lastError) {
		t.Fatalf("the first empty answer left %+v (err %v), want a failure due again that names the empty answer", first, err)
	}
	if r.row("order", gid)["deleted"] == true {
		t.Fatal("one empty answer retired the order")
	}
	r.now = clockAt("10:01:00")
	second, err := l.try(req, first)
	if err != nil || second.status != "processed" || second.due() {
		t.Fatalf("the second empty answer left %+v (err %v), want processed", second, err)
	}
	if r.row("order", gid)["deleted"] != true {
		t.Error("two clean empty answers in a row did not retire the order under a grant that reads every order")
	}
}

// A KIND THE ORDERS PERMISSION DOES NOT GOVERN keeps the confirmation: a held
// product is retired by the second clean empty answer in a row, whatever the
// grant says about orders.
func TestAnEmptyAnswerStillRetiresAProductOnTheSecondAnswer(t *testing.T) {
	const gid = "gid://shopify/Product/1"
	r := newShopRig(t)
	r.granted("read_orders", "read_products")
	l := r.lane()
	r.now = clockAt("10:00:00")
	r.held("product", gid, "2026-08-23T09:00:00Z", clockAt("09:30:00"))
	r.origin.reply("ShopifyFetchProduct", map[string]any{"node": nil})
	req := shopNotice("products/update", gid, "wh-1", clockAt("09:59:00"))

	first, err := l.try(req, shopStamped{})
	if err == nil || !first.due() {
		t.Fatalf("the first empty answer left %+v (err %v), want a failure due again", first, err)
	}
	if r.row("product", gid)["deleted"] == true {
		t.Fatal("one empty answer retired the product")
	}
	r.now = clockAt("10:01:00")
	if second, err := l.try(req, first); err != nil || second.status != "processed" {
		t.Fatalf("the second empty answer left %+v (err %v), want processed", second, err)
	}
	if r.row("product", gid)["deleted"] != true {
		t.Error("two clean empty answers in a row did not retire the product")
	}
}

// A DELETE NOTICE IS THE ORIGIN'S ANNOUNCEMENT and retires an order at once,
// under any grant: the rule above is about an answer of nothing, not about a
// deletion the origin said.
func TestADeleteNoticeRetiresAnOrderAtOnceUnderAnyGrant(t *testing.T) {
	const gid = "gid://shopify/Order/1"
	r := newShopRig(t)
	r.granted("read_orders", "read_products")
	l := r.lane()
	r.now = clockAt("10:00:00")
	r.held("order", gid, "2026-08-23T09:00:00Z", clockAt("09:30:00"))

	got, err := l.try(shopNotice("orders/delete", gid, "wh-del", clockAt("09:59:58")), shopStamped{})
	if err != nil || got.status != "processed" {
		t.Fatalf("the delete notice left %+v (err %v), want processed", got, err)
	}
	if row := r.row("order", gid); row["deleted"] != true || row["versionSource"] != memqlsync.VersionFromAnnouncement {
		t.Errorf("the order is %v, want it retired as announced", row)
	}
	if n := len(r.origin.seen()); n != 0 {
		t.Errorf("a delete notice asked the origin %d times", n)
	}
}

// AN EMPTY ANSWER FOR A RECORD ALREADY RETIRED WRITES NOTHING. A late update
// notice for a record the mirror holds as retired used to ask twice and then
// write a new tombstone, at a later time and unannounced, over the announced
// one. The record is already gone here: nothing is written and nothing is
// asked again.
func TestAnEmptyAnswerForARecordAlreadyRetiredWritesNothing(t *testing.T) {
	const gid = "gid://shopify/Product/1"
	r := newShopRig(t)
	l := r.lane()
	r.now = clockAt("10:00:00")
	r.held("product", gid, "2026-08-23T09:00:00Z", clockAt("09:30:00"))
	del := shopNotice("products/delete", gid, "wh-del", clockAt("09:59:58"))
	if got, err := l.try(del, shopStamped{}); err != nil || got.status != "processed" {
		t.Fatalf("the delete notice left %+v (err %v)", got, err)
	}
	retired := map[string]any{}
	for k, v := range r.row("product", gid) {
		retired[k] = v
	}
	if retired["deleted"] != true || retired["versionSource"] != memqlsync.VersionFromAnnouncement {
		t.Fatalf("the product is %v, want it retired as announced, or this test proves nothing", retired)
	}
	written := r.mirrorWrites()

	r.now = clockAt("10:05:00")
	r.origin.reply("ShopifyFetchProduct", map[string]any{"node": nil})
	late := shopNotice("products/update", gid, "wh-late", clockAt("09:59:50"))
	got, err := l.try(late, shopStamped{})
	if err != nil || got.status != "processed" || got.due() {
		t.Fatalf("the late notice left %+v (err %v), want processed and not asked again", got, err)
	}
	r.conn.ForgetInbound(context.Background(), late)
	if writes, err := r.conn.Apply(context.Background(), shopRetried(late)); err != nil || len(writes) != 0 {
		t.Fatalf("asked again all the same: writes %v, err %v; want nothing", writes, err)
	}
	if n := r.mirrorWrites(); n != written {
		t.Errorf("%d rows were written over the retired record, want none", n-written)
	}
	now := r.row("product", gid)
	for _, field := range []string{"versionSource", "syncedAt", "versionReadAt", "updatedAt"} {
		if now[field] != retired[field] {
			t.Errorf("%s moved from %v to %v: the announced deletion lost its standing", field, retired[field], now[field])
		}
	}
}

// A CREATE NOTICE CAN ARRIVE BEFORE ITS RECORD IS READABLE. An empty answer
// for a record the mirror does not hold is asked once more: the first is the
// failure worth another try, and the second writes nothing and is processed.
// Order data is no exception -- a new order is inside the sixty days.
func TestAnEmptyAnswerForARecordNotHeldIsAskedOnceMore(t *testing.T) {
	for _, tc := range []struct {
		name, concept, topic, fetch, gid string
	}{
		{"an order", "order", "orders/create", "ShopifyFetchOrder", "gid://shopify/Order/1"},
		{"a product", "product", "products/create", "ShopifyFetchProduct", "gid://shopify/Product/1"},
	} {
		t.Run(tc.name+", still not readable", func(t *testing.T) {
			r := newShopRig(t)
			r.granted("read_orders", "read_products")
			l := r.lane()
			r.now = clockAt("10:00:00")
			r.origin.reply(tc.fetch, map[string]any{"node": nil})
			req := shopNotice(tc.topic, tc.gid, "wh-create", clockAt("10:00:00"))

			first, err := l.try(req, shopStamped{})
			if err == nil || !first.due() || !memqlsync.AnsweredNothingBefore(first.lastError) {
				t.Fatalf("the first empty answer left %+v (err %v), want a failure due again", first, err)
			}
			if first.attempts != 1 {
				t.Errorf("the first try counted %d tries, want 1", first.attempts)
			}
			r.now = clockAt("10:01:00")
			second, err := l.try(req, first)
			if err != nil || second.status != "processed" || second.due() {
				t.Fatalf("the second empty answer left %+v (err %v), want processed", second, err)
			}
			if row := r.row(tc.concept, tc.gid); row != nil {
				t.Errorf("an empty answer for a record never held wrote %v", row)
			}
			if n := r.origin.countOp(tc.fetch); n != 2 {
				t.Errorf("the record was fetched %d times, want twice", n)
			}
		})
	}
	t.Run("an order, readable on the second try", func(t *testing.T) {
		const gid = "gid://shopify/Order/1"
		r := newShopRig(t)
		r.granted("read_orders", "read_products")
		l := r.lane()
		r.now = clockAt("10:00:00")
		r.orderFetches(nil)
		req := shopNotice("orders/create", gid, "wh-create", clockAt("10:00:00"))
		first, err := l.try(req, shopStamped{})
		if err == nil || !first.due() {
			t.Fatalf("the first empty answer left %+v (err %v), want a failure due again", first, err)
		}
		r.now = clockAt("10:01:00")
		r.orderFetches(shopOrderNode("2026-08-23T10:00:00Z", 1))
		if second, err := l.try(req, first); err != nil || second.status != "processed" {
			t.Fatalf("the second try left %+v (err %v), want processed", second, err)
		}
		if row := r.row("order", gid); row == nil || row["deleted"] == true || row["name"] != "#1001" {
			t.Errorf("the order the second try found is %v, want it written", row)
		}
	})
}

// THROUGH THE RUNTIME: an app-level delivery whose store could not be told is
// stamped failed with a next try, never processed. A privacy request is
// delivered once: answered "nothing to write" while the store list could not
// be read, it was stamped processed and never worked.
func TestAnUnreadableStoreListLeavesAPrivacyRequestDueAgain(t *testing.T) {
	r := newShopRig(t)
	r.managedApp()
	l := r.lane()
	r.storeListAway(true)
	const shop = "acme-widgets.myshopify.com"
	req := memqlsync.InboundRequest{
		RequestId: "inb-redact", Source: shopify.ConnectorName,
		Body:       []byte(fmt.Sprintf(`{"shop_domain":%q,"customer":{"id":991},"orders_to_redact":[]}`, shop)),
		Headers:    map[string]string{"x-shopify-shop-domain": shop, "x-shopify-topic": "customers/redact"},
		ReceivedAt: clockAt("10:00:00"),
	}

	got, err := l.try(req, shopStamped{})
	if err == nil || got.status != "failed" || !got.due() || got.attempts != 1 {
		t.Fatalf("the privacy request was left %+v (err %v), want failed and due again", got, err)
	}
	if !strings.Contains(got.lastError, "could not tell which store") {
		t.Errorf("the delivery's reason is %q, want it to say the store could not be told", got.lastError)
	}
}
