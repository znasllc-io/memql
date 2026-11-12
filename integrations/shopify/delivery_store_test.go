package shopify

import (
	"context"
	"fmt"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// delivery_store_test.go -- which store a delivery belongs to has THREE
// answers.
//
// The store, nobody's, and "could not tell": the store list could not be
// read. The third used to read as the second -- a registry read that failed
// answered "no such store" -- so a delivery that arrived while the list was
// unreadable was answered with nothing to write and stamped processed. A
// privacy request is delivered once: stamped processed, it was never worked.

// worthAnotherTry is the runtime's own question of a delivery's failure.
func worthAnotherTry(err error) bool {
	return err != nil && !memqlsync.IsPermanent(err) && !memqlsync.IsNotGranted(err) &&
		!memqlsync.IsNotRequested(err) && !memqlsync.IsNotImplemented(err) && !memqlsync.IsRecordingPaused(err)
}

func redactRequest(shop string) memqlsync.InboundRequest {
	return memqlsync.InboundRequest{
		RequestId: "inb-redact", Source: ConnectorName,
		Body:    []byte(fmt.Sprintf(`{"shop_domain":%q,"customer":{"id":991},"orders_to_redact":[]}`, shop)),
		Headers: map[string]string{strings.ToLower(HeaderShopDomain): shop, strings.ToLower(HeaderTopic): TopicRedact},
	}
}

func TestADeliveryWhoseStoreCouldNotBeToldIsFailedToBeTriedAgain(t *testing.T) {
	unreadable := errString("the store list could not be read")

	t.Run("a store's own source", func(t *testing.T) {
		h := newHarness(t)
		h.engine.fail["stores"] = unreadable
		for _, topic := range []string{"products/delete", "products/update"} {
			writes, err := h.conn.Apply(context.Background(), delivery(topic, "gid://shopify/Product/1"))
			if !worthAnotherTry(err) || len(writes) != 0 {
				t.Fatalf("%s while the store list is unreadable: writes %v, err %v; want a failure worth another try", topic, writes, err)
			}
			if !strings.Contains(err.Error(), "could not tell which store") {
				t.Errorf("err = %v, want it to say the store could not be told", err)
			}
		}
		if n := len(h.admin.seen()); n != 0 {
			t.Errorf("the origin was asked %d times", n)
		}
	})

	t.Run("the app-level source, a privacy request", func(t *testing.T) {
		h := managedWebhookHarness(t)
		h.engine.fail["stores"] = unreadable
		writes, err := h.conn.Apply(context.Background(), redactRequest("acme.myshopify.com"))
		if !worthAnotherTry(err) || len(writes) != 0 {
			t.Fatalf("a privacy request while the store list is unreadable: writes %v, err %v; want a failure worth another try", writes, err)
		}
		if n := len(h.engine.callsTo("queueComplianceJob")); n != 0 {
			t.Errorf("%d privacy jobs were queued for a store nobody could name", n)
		}
		// The list is readable again: the same delivery is worked.
		delete(h.engine.fail, "stores")
		h.engine.setRows("stores", []map[string]any{{"id": "acme", "domain": "acme.myshopify.com", "appClientId": "managed-app", "status": StatusLive}})
		if _, err := h.conn.Apply(context.Background(), redactRequest("acme.myshopify.com")); err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if n := len(h.engine.callsTo("queueComplianceJob")); n != 1 {
			t.Errorf("the retried privacy request queued %d jobs, want 1", n)
		}
	})

	t.Run("the app-level source, an uninstall", func(t *testing.T) {
		h := uninstallHarness(t)
		h.engine.fail["stores"] = unreadable
		writes, err := h.conn.Apply(context.Background(), uninstallDelivery(ConnectorName, shopBody(uninstallShop)))
		if !worthAnotherTry(err) || len(writes) != 0 {
			t.Fatalf("an uninstall while the store list is unreadable: writes %v, err %v; want a failure worth another try", writes, err)
		}
		if n := len(h.engine.callsTo("markStoreUninstalled")); n != 0 {
			t.Errorf("a store nobody could name was disconnected (%d writes)", n)
		}
	})
}

// A SOURCE THAT IS DEFINITELY NOBODY'S IS STILL A NO-OP: the list was read and
// names no such store, so there is nothing to try again -- an operator may
// have removed the store while Shopify still had a subscription.
func TestADeliveryThatIsDefinitelyNobodysIsStillANoOp(t *testing.T) {
	h := newHarness(t)
	gone := delivery("products/update", "gid://shopify/Product/1")
	gone.Source = "shopify-nobody"
	if writes, err := h.conn.Apply(context.Background(), gone); err != nil || len(writes) != 0 {
		t.Errorf("a removed store's delivery: writes %v, err %v; want nothing and no error", writes, err)
	}

	m := managedWebhookHarness(t)
	m.engine.setRows("stores", []map[string]any{{"id": "acme", "domain": "acme.myshopify.com", "appClientId": "managed-app", "status": StatusLive}})
	for name, req := range map[string]memqlsync.InboundRequest{
		"a shop this cluster does not hold": redactRequest("other.myshopify.com"),
		"a topic the app-level endpoint does not serve": func() memqlsync.InboundRequest {
			r := redactRequest("acme.myshopify.com")
			r.Headers[strings.ToLower(HeaderTopic)] = "products/update"
			return r
		}(),
	} {
		if writes, err := m.conn.Apply(context.Background(), req); err != nil || len(writes) != 0 {
			t.Errorf("%s: writes %v, err %v; want nothing and no error", name, writes, err)
		}
	}
	if n := len(m.engine.callsTo("queueComplianceJob")); n != 0 {
		t.Errorf("%d privacy jobs were queued for deliveries that are nobody's", n)
	}
}

// AND A STORE'S OWN DELIVERY IS WORKED: the third answer, beside the two
// above. The three-valued read and the two-valued one agree wherever the list
// can be read.
func TestADeliveryOfAStoreThatIsFoundIsWorked(t *testing.T) {
	h := newHarness(t)
	writes, err := h.conn.Apply(context.Background(), delivery("products/delete", "gid://shopify/Product/1"))
	if err != nil || len(writes) != 1 || !writes[0].Retire {
		t.Fatalf("writes %v, err %v; want the delete notice's one tombstone", writes, err)
	}
	req := delivery("products/delete", "gid://shopify/Product/1")
	store, found, err := h.conn.deliveryStore(context.Background(), req)
	if err != nil || !found || store.ID != testStoreID {
		t.Fatalf("deliveryStore = %q, %t, %v", store.ID, found, err)
	}
	if two, ok := h.conn.StoreFor(context.Background(), req); !ok || two.ID != store.ID {
		t.Fatalf("StoreFor = %q, %t; want the same store", two.ID, ok)
	}
	h.engine.fail["stores"] = errString("unreadable")
	if _, ok := h.conn.StoreFor(context.Background(), req); ok {
		t.Fatal("StoreFor answered a store while the list was unreadable")
	}
	if claimed, err := h.conn.ClaimsInboundSource(context.Background(), req.Source); err == nil || claimed {
		t.Fatalf("ClaimsInboundSource = %t, %v; want it to say it could not tell", claimed, err)
	}
}

// Through the runtime -- an app-level delivery whose store could not be told
// is stamped failed with a next try, never processed -- is
// TestAnUnreadableStoreListLeavesAPrivacyRequestDueAgain in the root module
// (component/datasync/shopify_empty_answer_test.go), with the dispatcher it
// needs.
