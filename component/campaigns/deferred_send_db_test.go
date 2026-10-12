package campaigns

import (
	"context"
	"sync"
	"testing"
	"time"
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
