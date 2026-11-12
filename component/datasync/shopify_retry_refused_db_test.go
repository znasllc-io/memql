package datasync

import (
	"context"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// shopify_retry_refused_db_test.go -- datasyncRetryRefused with the real
// Shopify connector as the one who says what its refusals are, through a real
// engine. Composed here because the integrations module cannot import the
// runtime (shopify_partitions_db_test.go).

func TestTheShopifyConnectorsRefusalsAreRetriedByTheirKind(t *testing.T) {
	f := newPartitionsFixture(t)
	if f == nil {
		return
	}
	store := f.storeIDs[0]
	f.connect(t, store)
	ctx := OperatorContext(context.Background())
	if _, ok := any(f.conn).(RefusalRetrier); !ok {
		t.Fatal("the Shopify connector does not say what its refusals are")
	}
	scopeRefusal := memqlsync.NotGranted(shopify.ConnectorName, "read_customers").Error()
	protected := memqlsync.ErrNotGranted.Error() + ": shopify: Customer object is not approved to access protected-customer-data"
	write := func(concept string, st SyncState) {
		t.Helper()
		st.ConceptID, st.Connector, st.Direction, st.Partition = concept, shopify.ConnectorName, "inbound", store
		if st.BackfillStatus == "" {
			st.BackfillStatus = "none"
		}
		if err := f.store.WriteSyncState(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	read := func(concept string) SyncState {
		t.Helper()
		st, err := f.store.SyncStateFor(ctx, concept, shopify.ConnectorName, "inbound", store)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	old := time.Now().UTC().Add(-7 * time.Hour).Truncate(time.Second)
	recent := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	// Segments: no backfill, refused by the sweep.
	write("v1:shopify:segment", SyncState{LastReconcileAt: old, LastAttemptAt: recent, LastError: scopeRefusal})
	// Customers: a backfill refused while the sweep reads them.
	write("v1:shopify:customer", SyncState{BackfillStatus: "failed", LastReconcileAt: old, LastError: "backfill refused: " + scopeRefusal})
	// Orders: the protected-data refusal, which no grant answers.
	write("v1:shopify:order", SyncState{LastAttemptAt: recent, LastError: protected})
	untouched := read("v1:shopify:order")

	for concept, want := range map[string]RetryRefusedResult{
		"v1:shopify:segment":  {Retried: true, Action: "due"},
		"v1:shopify:customer": {Retried: true, Action: "queued"},
		"v1:shopify:order":    {},
	} {
		got, err := f.runner.RetryRefused(context.Background(), shopify.ConnectorName, concept, store)
		if err != nil {
			t.Fatalf("RetryRefused(%s): %v", concept, err)
		}
		if got != want {
			t.Errorf("RetryRefused(%s) = %+v, want %+v", concept, got, want)
		}
	}
	if st := read("v1:shopify:segment"); st.LastError != "" || !st.LastAttemptAt.IsZero() || !st.LastReconcileAt.Equal(old) {
		t.Errorf("segments after the retry: %+v; want no refusal, no attempt, the old sweep kept", st)
	}
	if st := read("v1:shopify:customer"); st.BackfillStatus != "queued" || st.LastError != "" {
		t.Errorf("customers after the retry: %+v; want queued, no refusal", st)
	}
	if st := read("v1:shopify:order"); st != untouched {
		t.Errorf("the protected-data refusal was written:\n  before %+v\n  after  %+v", untouched, st)
	}
	// A partitioned connector's retry names one store.
	if _, err := f.runner.RetryRefused(context.Background(), shopify.ConnectorName, "v1:shopify:segment", ""); err == nil {
		t.Error("a retry naming no store was answered")
	}
}
