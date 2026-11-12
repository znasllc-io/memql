package datasync

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// shopify_unplaced_db_test.go -- a delivery whose connector cannot be told at
// its first dispatch, through a real engine, the real connector and the
// runtime's own dispatcher and retry pass.
//
// The connector cannot tell whose a delivery is while it cannot read its
// store list. The fakes say the dispatcher stamps such a row; only the real
// engine says the stamped row is one the retry pass's read answers, that the
// connector's own claim check is the one that errors, and that the delivery
// is worked once the list can be read.

// A DELIVERY STAGED WHILE ITS STORE CANNOT BE TOLD IS NOT LOST. It is failed
// with the fixed sentence, due in a minute, and no try is counted. While the
// list stays unreadable the retry pass leaves it exactly as it is and says so
// once; when the list can be read the pass dispatches it, the origin is asked,
// and the record is written.
func TestADeliveryStagedWhileItsStoreCannotBeToldIsPlacedByTheRetryPass(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const gid = "gid://shopify/Product/8401"
	r.origin.reply("ShopifyFetchProduct", map[string]any{"node": map[string]any{
		"__typename": "Product", "id": gid, "title": "Arrived during the outage", "updatedAt": r.at.Add(-time.Minute).Format(time.RFC3339)}})
	log := &recorded{}
	r.d.WithLogger(slog.New(log))

	r.storeListAway(true)
	id := r.stage("unplaced", "products/update", gid)
	staged := time.Now().UTC()
	res, err := r.dispatchErr(id)
	if err == nil || !strings.Contains(err.Error(), unplacedReason) || res.Handled {
		t.Fatalf("the first dispatch = %+v, %v; want a failure that says the connector could not be told", res, err)
	}
	row := r.delivery(id)
	if stringField(row, "status") != "failed" || stringField(row, "lastError") != unplacedReason || intField(row, "attempts") != 0 {
		t.Fatalf("the delivery is %v; want failed, the fixed sentence as its reason, and no try counted", row)
	}
	next, perr := time.Parse(time.RFC3339, stringField(row, "nextAttemptAt"))
	if perr != nil || next.Before(staged.Add(50*time.Second)) || next.After(staged.Add(70*time.Second)) {
		t.Fatalf("the delivery is due at %q (%v), want one minute after its dispatch at %s", stringField(row, "nextAttemptAt"), perr, staged.Format(time.RFC3339))
	}
	if n := r.origin.countOp("ShopifyFetchProduct"); n != 0 {
		t.Fatalf("the origin was asked %d times for a delivery nobody could place", n)
	}

	// Two minutes on the delivery is due, and the list is still away: the pass
	// leaves the row as it was and says it once.
	r.d.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	written := r.versions(id)
	if _, err := r.d.RetryDue(r.opCtx()); err != nil {
		t.Fatalf("the retry pass: %v", err)
	}
	if got := r.versions(id); got != written {
		t.Fatalf("a pass that could not tell either wrote the delivery %d more times", got-written)
	}
	if after := r.delivery(id); stringField(after, "lastError") != unplacedReason || intField(after, "attempts") != 0 || stringField(after, "nextAttemptAt") != stringField(row, "nextAttemptAt") {
		t.Fatalf("a pass that could not tell either left the delivery %v", after)
	}
	if got := log.waiting()[r.source]; len(got) != 1 || got[0] != 1 {
		t.Fatalf("the pass warned of the source as %v, want once, counting its one delivery", got)
	}

	// The list can be read again: the next pass dispatches the delivery.
	r.storeListAway(false)
	r.retryPass()
	if row := r.delivery(id); stringField(row, "status") != "processed" || intField(row, "attempts") != 1 {
		t.Fatalf("after the pass that could tell, the delivery is %v; want processed at its one counted try", row)
	}
	if rec := r.record(gid); rec == nil || rec["title"] != "Arrived during the outage" || rec["deleted"] == true {
		t.Fatalf("the record the delivery announced is %v, want it written", rec)
	}
}

// DEFINITELY NOBODY'S IS NOT STAMPED. The connector reads its list and the
// source names no store of it: the row stays as it was staged, for whatever
// else serves that door.
func TestADeliveryOfASourceThatIsDefinitelyNobodysStaysAsItWasStaged(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	id := r.stageOn(r.source+"-nobody", "delivery", "products/update", "gid://shopify/Product/8501")
	res, err := r.dispatchErr(id)
	if err != nil || res.Handled {
		t.Fatalf("the dispatch = %+v, %v; want not handled and no error", res, err)
	}
	if row := r.delivery(id); stringField(row, "status") != "received" || stringField(row, "lastError") != "" {
		t.Fatalf("the delivery is %v, want as it was staged", row)
	}
	if n := r.versions(id); n != 1 {
		t.Fatalf("the delivery was written %d times, want only its staging", n)
	}
}
