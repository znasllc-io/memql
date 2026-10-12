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
