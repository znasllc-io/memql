package datasync

import (
	"context"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// A connector that has not implemented outbound delivery must not age
// its queue into the dead-letter ceiling.
//
// The claim increments the attempt count before every delivery attempt,
// including the one that turns out to be unimplemented -- so without the
// restore, the counter creeps by one per poll against a capability
// nobody has written. Some hours later the entry sits at the ceiling,
// and the first transient failure after the connector finally implements
// Propagate dead-letters it immediately. The ceiling is meant to bound
// FAILING DELIVERIES, not waiting.
func TestAnUnimplementedPropagateDoesNotAgeTheQueueTowardTheCeiling(t *testing.T) {
	engine := newFakeEngine()
	engine.seed(`query outboxPending`, []map[string]any{outboxRow("e1", "shopify", 3, "")})
	c := &fakeConnector{
		name: "shopify",
		propagateResults: []error{
			memqlsync.NotImplemented("shopify", "Propagate"),
		},
	}
	w := testWorker(engine, &alwaysClaim{}, c, testNow)

	w.DrainOnce(context.Background())

	claims := engine.callsContaining("markOutboxDelivering")
	if len(claims) != 2 {
		t.Fatalf("expected the claim and then the restore, got %d call(s): %v", len(claims), claims)
	}
	if !strings.Contains(claims[0], "attempts: 4") {
		t.Errorf("the claim did not count the attempt: %q", claims[0])
	}
	if !strings.Contains(claims[1], "attempts: 3") {
		t.Errorf("the attempt count was not restored to what it was: %q -- the queue would creep to the ceiling while waiting for a capability to be written", claims[1])
	}
	if engine.countContaining("markOutboxDead") != 0 {
		t.Error("an entry was dead-lettered for an unimplemented capability")
	}
}
