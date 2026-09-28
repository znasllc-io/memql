package shopify

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// privacy_refusal_db_test.go -- the refused-privacy count, against a real
// engine over a real Postgres (memql#5707 residual).
//
// The unit tests in privacy_migration_test.go record the MemQL text and parse
// none of it, so they cannot say whether the audit write is ADMITTED under the
// connector's operator identity, whether auditEventsByTarget's cluster-owner
// gate admits that identity reading it back, or whether a re-dispatched
// refusal collapses onto one row. Each of those failing would read as "no
// privacy delivery was refused" -- the one answer this figure must never give
// wrongly -- so this boots the real thing.

func TestARefusedPrivacyDeliveryIsCountedFromTheRealAuditTrail(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	const storeID, otherStoreID = "dbtest-privacy-refusal", "dbtest-privacy-other"
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' IN ($1, $2)`,
			storeID, otherStoreID)
	})
	conn := NewConnector(eng, slog.New(slog.NewTextHandler(io.Discard, nil)), NewStoreRegistry(eng, nil), NewAdminClient())
	store := Store{ID: storeID, Domain: storeID + ".myshopify.com"}
	req := memqlsync.InboundRequest{
		RequestId: "inb-dbtest-privacy", Source: "shopify-" + storeID, Topic: TopicRedact,
		ReceivedAt: time.Now().UTC(),
	}

	// Refused twice for ONE staged delivery: one lost request, one count.
	for i := 0; i < 2; i++ {
		if err := conn.refusePrivacyOnStoreURL(context.Background(), store, TopicRedact, req); err == nil {
			t.Fatal("refusePrivacyOnStoreURL returned no error; the dispatcher would stamp the row processed")
		}
	}
	got, err := conn.privacyDeliveries(context.Background(), store)
	if err != nil {
		t.Fatalf("privacyDeliveries: %v", err)
	}
	if got["refused"] != 1 || got["lastRefusedAt"] == "" || got["capped"] != false {
		t.Fatalf("privacyDeliveries = %v; want refused=1 with a time, uncapped -- the audit write or its read-back "+
			"was refused, or a re-dispatch counted twice", got)
	}

	// A second delivery counts; another store's trail does not.
	req.RequestId = "inb-dbtest-privacy-2"
	_ = conn.refusePrivacyOnStoreURL(context.Background(), store, TopicShopRedact, req)
	if got, _ := conn.privacyDeliveries(context.Background(), store); got["refused"] != 2 {
		t.Fatalf("after a second refused delivery, privacyDeliveries = %v; want refused=2", got)
	}
	other, err := conn.privacyDeliveries(context.Background(), Store{ID: otherStoreID})
	if err != nil || other["refused"] != 0 {
		t.Fatalf("another store's privacyDeliveries = %v, %v; want a measured zero -- the count is not scoped to its store", other, err)
	}
}
