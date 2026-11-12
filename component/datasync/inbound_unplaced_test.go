package datasync

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// inbound_unplaced_test.go -- a delivery whose connector cannot be told at the
// first dispatch.
//
// The dispatcher's first question is which connector serves the row's source.
// A connector that cannot read its tenants answers "not mine" to the routing
// question, and the dispatcher used to believe it: the row was "not handled",
// stayed `received`, and nothing reads a received row again. The first
// judgement is three-valued now, as the retry pass's is: served, definitely
// nobody's, and could not tell -- which is failed, due again in a minute, the
// try not counted, so the retry pass places it when somebody can.

// unplacedDispatcher is a dispatcher whose routing answers "not served" and
// whose claim check answers as told.
func unplacedDispatcher(engine Engine, claimed bool, err error) *Dispatcher {
	store := NewStore(engine)
	d := NewDispatcher(store, NewApplier(store, newFakeWriter()))
	d.lookup = func(context.Context, string) (memqlsync.Connector, bool) { return nil, false }
	d.claimed = func(context.Context, string) (memqlsync.Connector, bool, error) { return nil, claimed, err }
	d.now = func() time.Time { return testNow }
	return d
}

func receivedRow(id, source string) StagedRequest {
	return StagedRequest{
		RequestId: id, Source: source, Body: []byte(`{"admin_graphql_api_id":"gid://shopify/Product/1"}`),
		HeadersJSON: `{"x-shopify-topic":"products/update"}`, VerifiedBy: memqlsync.VerifiedByConnector,
		SignatureVerified: true, Status: "received",
	}
}

// COULD NOT TELL IS NOT "NOBODY'S". The row is stamped failed with the fixed
// sentence, due again in one minute, and its try is not counted: the retry
// pass leaves it uncounted for as long as nobody can tell, dispatches it when
// somebody can, and settles it when the source is definitely nobody's.
func TestADeliveryWhoseConnectorCannotBeToldAtTheFirstDispatchIsFailedAndDueAgain(t *testing.T) {
	for name, tc := range map[string]struct {
		claimed bool
		err     error
		tries   int
	}{
		"the connector could not read its tenants":      {false, errors.New(`sync: connector "shopify" could not say whether it claims source "shopify-acme": the store list is away`), 0},
		"a connector claims it and could not route it":  {true, nil, 0},
		"a row put back in line and then not placeable": {false, errors.New("the store list is away"), 2},
	} {
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			d := unplacedDispatcher(engine, tc.claimed, tc.err)
			row := receivedRow("req-1", "shopify-acme")
			row.Attempts = tc.tries
			res, err := d.DispatchStaged(context.Background(), row)
			if err == nil || !strings.Contains(err.Error(), unplacedReason) {
				t.Fatalf("DispatchStaged = %+v, %v; want a failure that says the connector could not be told", res, err)
			}
			stamp := onlyStamp(t, engine)
			if stamp["status"] != "failed" {
				t.Fatalf("the row was stamped %v, want failed", stamp["status"])
			}
			if stamp["lastError"] != unplacedReason {
				t.Errorf("lastError = %q, want exactly the fixed sentence %q: it names no store and no source", stamp["lastError"], unplacedReason)
			}
			if want := testNow.Add(time.Minute).Format(time.RFC3339); stamp["nextAttemptAt"] != want {
				t.Errorf("nextAttemptAt = %v, want one minute on, %s", stamp["nextAttemptAt"], want)
			}
			if got, named := stamp["attempts"]; !named || intField(stamp, "attempts") != tc.tries {
				t.Errorf("attempts = %v (named %t), want the %d it had: this try is not counted", got, named, tc.tries)
			}
		})
	}
}

// The sentence names nobody: it is read by an operator on the delivery.
func TestTheUnplacedReasonNamesNoStore(t *testing.T) {
	if unplacedReason != "datasync: could not tell which connector serves this delivery's source; it is tried again" {
		t.Fatalf("the reason is %q", unplacedReason)
	}
}

// DEFINITELY NOBODY'S STAYS THE NO-OP IT IS: /inbound/{source} is a shared
// door, and a row no connector claims belongs to something else.
func TestADeliveryThatIsDefinitelyNobodysIsNotStampedAtTheFirstDispatch(t *testing.T) {
	engine := newFakeEngine()
	d := unplacedDispatcher(engine, false, nil)
	res, err := d.DispatchStaged(context.Background(), receivedRow("req-1", "billing-webhooks"))
	if err != nil || res.Handled {
		t.Fatalf("DispatchStaged = %+v, %v; want not handled and no error", res, err)
	}
	if calls := engine.callsContaining(""); len(calls) != 0 {
		t.Fatalf("a row that is nobody's reached the engine: %v", calls)
	}
}

// The event path hands the failure to the automation's step, so an operator
// sees it there as well as on the row.
func TestTheEventPathReportsADeliveryItCouldNotPlace(t *testing.T) {
	engine := newFakeEngine()
	stageInbound(engine, stagedInbound("shopify-acme"))
	i := &Integration{dispatcher: unplacedDispatcher(engine, false, errors.New("the store list is away")), logger: discardLogger()}
	nodes, err := dispatchByID(i, "req")
	if err == nil || !strings.Contains(err.Error(), unplacedReason) || len(nodes) != 0 {
		t.Fatalf("dispatchInbound = %v, %v; want the failure", nodes, err)
	}
	if stamp := onlyStamp(t, engine); stamp["status"] != "failed" || stamp["lastError"] != unplacedReason {
		t.Fatalf("the row was stamped %v", stamp)
	}
}

// STILL UNABLE, THE ROW IS LEFT AS IT WAS, AND SAID ONCE. The retry pass finds
// the unplaced rows due, asks once for their source, writes nothing, and
// warns once with how many wait.
func TestTheRetryPassLeavesAnUnplacedRowAsItWasWhileNobodyCanTell(t *testing.T) {
	engine := newFakeEngine()
	rows := make([]map[string]any, 0, 3)
	for _, id := range []string{"req-a", "req-b", "req-c"} {
		row := stagedInbound("shopify-acme")
		row["id"], row["status"], row["attempts"], row["lastError"] = id, "failed", 0, unplacedReason
		row["nextAttemptAt"] = testNow.Add(-time.Second).Format(time.RFC3339)
		row["createdAt"] = testNow.Add(-time.Minute).Format(time.RFC3339Nano)
		rows = append(rows, row)
	}
	engine.seed("query inboundRequestsDueForRetry", rows)
	d := unplacedDispatcher(engine, false, errors.New("the store list is away"))
	log := &recorded{}
	d.WithLogger(slog.New(log))

	res, err := d.RetryDue(context.Background())
	if err != nil || res != (RetryResult{}) {
		t.Fatalf("RetryDue = %+v, %v; want nothing tried", res, err)
	}
	if writes := engine.callsContaining("mutation "); len(writes) != 0 {
		t.Fatalf("rows nobody could place were written: %v", writes)
	}
	if got := log.waiting()["shopify-acme"]; len(got) != 1 || got[0] != 3 {
		t.Fatalf("the source was warned of as %v, want one warning counting its 3 deliveries", got)
	}
}
