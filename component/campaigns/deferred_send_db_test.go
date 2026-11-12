package campaigns

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func TestDeferredSingleSendReplaysOnIndependentReplicaWithoutDuplicateMail(t *testing.T) {
	n := newDigestFixture(t).newEmptyNewsletter(t, "deferred-hop")
	recipient := n.suffix + "-recipient"
	if err := n.store.AddRecipient(n.owner, recipient, n.audience, "deferred@example.test", "Deferred Person", "manual", n.org, "subscribed", nil); err != nil {
		t.Fatal(err)
	}
	n.worker.limiter = newRateLimiter(1, n.worker.now)
	if !n.worker.allowSend() {
		t.Fatal("new limiter empty")
	}
	args := map[string]any{"requestId": n.suffix + "-send", "templateId": n.suffix + "-tpl", "recipientId": recipient, "senderIdentityId": n.suffix + "-sender"}
	result, err := n.worker.handleSendToRecipient(n.owner, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	if decodeResult(t, result)["deferred"] != true || n.sender.count() != 0 {
		t.Fatal("initial call did not defer")
	}
	e2, db2 := digestDBEngine(t)
	other := newGatedWorker(t, e2, db2, n.sender)
	later := time.Now().Add(2 * time.Minute)
	for _, w := range []*Worker{n.worker, other} {
		w.now = func() time.Time { return later }
		w.limiter = newRateLimiter(1000, w.now)
	}
	var wg sync.WaitGroup
	for _, w := range []*Worker{n.worker, other} {
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			w.retryDeferredSends(context.Background(), w.systemActorContext(context.Background()))
		}(w)
	}
	wg.Wait()
	other.retryDeferredSends(context.Background(), other.systemActorContext(context.Background()))
	if n.sender.count() != 1 {
		t.Fatalf("provider sends=%d want1", n.sender.count())
	}
	result, err = other.handleSendToRecipient(n.owner, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	if decodeResult(t, result)["replayed"] != true || n.sender.count() != 1 {
		t.Fatal("receipt did not survive replica hop")
	}
}

// A terminal verdict must remain writable even when a send is no longer
// authorized, without changing whose delivery it was or the address it named.
func TestDeferredSingleSendTerminalVerdictsPreserveAttribution(t *testing.T) {
	f := newDigestFixture(t)
	for _, mode := range []string{"removed", "revoked", "uncertain", "completed"} {
		t.Run(mode, func(t *testing.T) {
			n := f.newEmptyNewsletter(t, "deferred-"+mode)
			recipient := n.suffix + "-recipient"
			if err := n.store.AddRecipient(n.owner, recipient, n.audience, "terminal@example.test", "Person", "manual", n.org, "subscribed", nil); err != nil {
				t.Fatal(err)
			}
			n.worker.limiter = newRateLimiter(1, n.worker.now)
			n.worker.allowSend()
			args := map[string]any{"requestId": n.suffix + "-request", "templateId": n.suffix + "-tpl", "recipientId": recipient, "senderIdentityId": n.suffix + "-sender"}
			nodes, err := n.worker.handleSendToRecipient(n.owner, args, 0)
			if err != nil || decodeResult(t, nodes)["deferred"] != true {
				t.Fatalf("defer %v %v", nodes, err)
			}
			switch mode {
			case "removed":
				if _, err := n.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept='v1:campaigns:recipient' AND id=?`, "v1:campaigns:recipient:"+recipient); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				n.worker.store.engine = refusedDeferredAuthority{n.worker.store.engine}
			case "uncertain", "completed":
				receipt, _, err := singleSendReceiptFor(n.owner, args, singleSend{deferWhenLimited: true})
				if err != nil {
					t.Fatal(err)
				}
				receipt.accountID = n.org
				status, result := "attempting", map[string]any{}
				if mode == "completed" {
					status = "complete"
					result["sent"] = true
				}
				if err = n.worker.saveSingleSendReceipt(n.owner, receipt, status, result); err != nil {
					t.Fatal(err)
				}
			}
			later := time.Now().Add(2 * time.Minute)
			n.worker.now = func() time.Time { return later }
			n.worker.limiter = newRateLimiter(1000, n.worker.now)
			n.worker.retryDeferredSends(context.Background(), n.system)
			row, err := n.worker.newsletterRow(n.system, "deferredSingleSendById", "deliveryId", singleDeliveryRowID(str(args, "requestId"), recipient))
			if err != nil {
				t.Fatal(err)
			}
			want := "failed"
			if mode == "completed" {
				want = "sent"
			}
			if str(row, "status") != want || want == "failed" && str(row, "lastError") == "" {
				t.Fatalf("pending work not finished: %v", row)
			}
			if bare(str(row, "ownerUserId")) != n.user || !sameOrganization(str(row, "accountId"), n.org) || bare(str(row, "recipientId")) != recipient {
				t.Fatalf("attribution changed: %v", row)
			}
			n.worker.retryDeferredSends(context.Background(), n.system)
			if n.sender.count() != 0 {
				t.Fatal("terminal deferred request sent mail")
			}
			// A public caller cannot forge this server verdict.
			if _, err = n.engine.Execute(n.owner, call("mutation", "settleDeferredSingleSend", arg{"status", "failed"}, arg{"skipReason", ""}, arg{"attempts", 0}, arg{"deliveryId", str(row, "id")}, arg{"lastError", "forged"}, arg{"versionTime", time.Now().Format(time.RFC3339Nano)})); err == nil {
				t.Fatal("client wrote retry verdict")
			}
		})
	}
}

type refusedDeferredAuthority struct{ Engine }

func (refusedDeferredAuthority) OrganizationCapable(context.Context, string, string, string) bool {
	return false
}

// orderLane is a tenant whose site serves a store with one mirrored order.
type orderLane struct {
	*digestNewsletter
	store, orderGid string
}

func newOrderLane(t *testing.T, name string) *orderLane {
	t.Helper()
	n := newDigestFixture(t).newEmptyNewsletter(t, name)
	o := &orderLane{digestNewsletter: n, store: n.suffix + "-store", orderGid: "gid://shopify/Order/" + n.suffix}
	site := n.suffix + "-shopsite"
	n.execute(t, n.system, fmt.Sprintf(`insert("v1:shopify:store", id=%s, payload={"domain":"acme-widgets.myshopify.com"})`, langparser.QuoteString(o.store)))
	n.execute(t, n.system, fmt.Sprintf(`insert("v1:platform:site", id=%s, payload={"ownerUserId":%s,"accountId":%s,"title":"Shop","hostname":%s,"status":"live","kind":"shopify_storefront","bundleRef":%s,"binding":{"storeId":%s}})`,
		langparser.QuoteString(site), langparser.QuoteString(n.user), langparser.QuoteString(n.org), langparser.QuoteString("shop-"+n.suffix+".example.test"),
		langparser.QuoteString("blob://sites/"+site+"/v1/"), langparser.QuoteString(o.store)))
	// The mirror is written by its connector alone, as the reviews pack's seedMirror writes it.
	n.execute(t, auth.ContextWithConnectorActor(context.Background(), "shopify"), fmt.Sprintf(`insert("v1:shopify:order", id=%s, payload={"storeId":%s,"gid":%s,"email":"buyer@example.com","name":"#1009","updatedAt":%[4]s,"syncedAt":%[4]s})`,
		langparser.QuoteString(n.suffix+"-order"), langparser.QuoteString(o.store), langparser.QuoteString(o.orderGid), langparser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano))))
	return o
}

func (o *orderLane) args() map[string]any {
	return map[string]any{"storeId": o.store, "orderGid": o.orderGid, "accountId": o.org, "templateId": o.suffix + "-tpl",
		"requestId": o.suffix + "-order-send", "senderIdentityId": o.suffix + "-sender"}
}

// send calls as the reviews pack's job does: internal origin, as the merchant.
func (o *orderLane) send(t *testing.T) map[string]any {
	t.Helper()
	nodes, err := o.worker.handleSendOrderMessage(auth.ContextWithInternalOrigin(o.owner), o.args(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return decodeResult(t, nodes)
}

// deliveryRows is every stored version of the order message's delivery row.
func (o *orderLane) deliveryRows(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := o.db.QueryContext(context.Background(), `SELECT payload::text FROM "MemoryNodes" WHERE concept = 'v1:campaigns:delivery' AND payload->>'requestId' = ? ORDER BY "createdAt"`, o.suffix+"-order-send")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

// An order message's fields and sender are its caller's and are stored nowhere,
// so a replay could not send it whole (G6, as decided when it was tested). An
// order send that meets the rate limit is answered as limited, writes no pending
// row, and nothing replays it; the caller sends it again.
func TestARateLimitedOrderSendIsAnsweredAsLimitedAndNotQueued(t *testing.T) {
	o := newOrderLane(t, "deferred-order")
	o.worker.limiter = newRateLimiter(1, o.worker.now)
	if !o.worker.allowSend() {
		t.Fatal("new limiter empty")
	}
	got := o.send(t)
	if got["limited"] != true || got["sent"] != false || got["skipped"] != false || got["deferred"] != nil || got["reason"] != orderLimitedReason || o.sender.count() != 0 {
		t.Fatalf("a limited order send answered %v", got)
	}
	if rows := o.deliveryRows(t); len(rows) != 0 {
		t.Fatalf("a limited order send wrote %d ledger rows: %v", len(rows), rows)
	}
	later := time.Now().Add(2 * time.Minute)
	o.worker.now = func() time.Time { return later }
	o.worker.limiter = newRateLimiter(1000, o.worker.now)
	o.worker.retryDeferredSends(context.Background(), o.system)
	if o.sender.count() != 0 {
		t.Fatal("the deferred replay picked up a limited order send")
	}
	// The caller sends it again, under the same request id, and it goes.
	if got := o.send(t); got["sent"] != true || o.sender.count() != 1 || o.sender.sent[0].To != "buyer@example.com" {
		t.Fatalf("the caller's second call: %v, %d sent", got, o.sender.count())
	}
}

// A sent order message is recorded: its ledger row names the order (the
// organization guard admits a row about a person with no recipient row only so),
// and a second call under the same request id sends nothing.
func TestASentOrderMessageLeavesOneDeliveryRow(t *testing.T) {
	o := newOrderLane(t, "order-ledger")
	if got := o.send(t); got["sent"] != true || o.sender.count() != 1 {
		t.Fatalf("the order send: %v", got)
	}
	rows := o.deliveryRows(t)
	if len(rows) != 1 {
		t.Fatalf("%d ledger rows for one order message: %v", len(rows), rows)
	}
	row := rows[0]
	if str(row, "status") != "sent" || str(row, "rowConcept") != "v1:shopify:order" || str(row, "rowId") != o.orderGid || str(row, "rowKind") != "" ||
		bare(str(row, "recipientId")) != orderRecipientID(o.store, o.orderGid) || !sameOrganization(str(row, "accountId"), o.org) || bare(str(row, "ownerUserId")) != o.user {
		t.Fatalf("the ledger row: %v", row)
	}
	if got := o.send(t); got["replayed"] != true || o.sender.count() != 1 || len(o.deliveryRows(t)) != 1 {
		t.Fatalf("a second call with the same request id: %v, %d sent", got, o.sender.count())
	}
}
