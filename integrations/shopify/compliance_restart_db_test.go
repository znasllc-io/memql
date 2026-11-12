package shopify

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// compliance_restart_db_test.go -- an erasure starts the store's backfills in
// flight again (ruling 73).
//
// The scrub of customers/redact is one point-in-time update. A backfill whose
// export the origin started before it, streamed after it, would write the
// erased fields back. So once a redaction or a purge has run, every backfill
// of that store that is running or queued is started again from nothing: its
// cursor is dropped, the invocation in flight abandons at its next write, and
// the export the pass starts next is one the origin takes after the erasure.
//
// These hold the connector's half: which rows are restarted, and how. Against
// a real Postgres because the scrub is raw SQL; the sync runtime's half, and
// the two together, are component/datasync's (shopify_redaction_db_test.go).

// redactionHarness is the harness with a database handle and the given sync
// state rows of one store, by domain, answered by id.
func redactionHarness(t *testing.T, db *sql.DB, storeID string, rows map[string]map[string]any) *testHarness {
	t.Helper()
	h := newHarness(t)
	h.conn.WithDatabase(func() *sql.DB { return db })
	byID := map[string]map[string]any{}
	for concept, fields := range rows {
		id := memqlsync.StateID(generated.ConceptID(concept), ConnectorName, string(memqlsync.DirectionInbound), storeID)
		row := map[string]any{"id": id, "conceptId": generated.ConceptID(concept), "connector": ConnectorName, "partitionKey": storeID}
		for k, v := range fields {
			row[k] = v
		}
		byID[id] = row
	}
	h.conn.engine = &stateRowsEngine{fakeEngine: h.engine, byID: byID}
	return h
}

// restartedDomains is the domains a restart was queued for, as store/concept,
// and fails the test for a queue that is not the engine's own restart.
func restartedDomains(t *testing.T, h *testHarness) []string {
	t.Helper()
	for _, q := range h.engine.callsTo("datasyncQueueBackfill") {
		if !strings.Contains(q, "restart: true") {
			t.Errorf("a backfill was queued without the restart flag, which leaves a running one at its old cursor: %s", q)
		}
	}
	return bootQueued(t, h)
}

func TestARedactionStartsTheStoresBackfillsInFlightAgain(t *testing.T) {
	db := openTestDB(t, "customers/redact restarts the backfills in flight")
	if db == nil {
		return
	}
	midFile := inFlight(backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Line: 1500})
	h := redactionHarness(t, db, testStoreID, map[string]map[string]any{
		"order":      {"backfillStatus": "running", "backfillCursor": midFile},
		"customer":   {"backfillStatus": "queued"},
		"product":    {"backfillStatus": "complete", "backfillCursor": completedUnderTheRule},
		"collection": {"backfillStatus": "failed", "backfillCursor": midFile, "lastError": "backfill failed: shopify admin: HTTP 502"},
		"location":   {"backfillStatus": "none"},
	})
	job := ComplianceJob{Topic: TopicRedact, StoreID: testStoreID, CustomerGID: "gid://shopify/Customer/777000111", ReceivedAt: h.now, DueAt: h.now}

	outcome, err := h.conn.runComplianceJob(context.Background(), h.store(t), job)
	if err != nil {
		t.Fatal(err)
	}
	got := restartedDomains(t, h)
	if strings.Join(got, ",") != testStoreID+"/customer,"+testStoreID+"/order" {
		t.Fatalf("restarted %v, want the store's running and queued backfills and no other: customer and order", got)
	}
	if !strings.Contains(outcome, "scrubbed ") || !strings.Contains(outcome, "started 2 backfill(s) again from nothing") {
		t.Errorf("outcome = %q, want what was scrubbed and that two backfills were started again", outcome)
	}
	if audits := h.engine.callsTo("createAuditEvent"); len(audits) != 1 || !strings.Contains(audits[0], "shopify_customer_redacted") {
		t.Errorf("audits = %v, want the redaction recorded", audits)
	}
}

func TestARedactionWithNothingRunningQueuesNothing(t *testing.T) {
	db := openTestDB(t, "customers/redact with no backfill in flight")
	if db == nil {
		return
	}
	h := redactionHarness(t, db, testStoreID, map[string]map[string]any{
		"order":      {"backfillStatus": "complete", "backfillCursor": completedUnderTheRule},
		"customer":   {"backfillStatus": "complete", "backfillCursor": completedUnderTheRule},
		"collection": {"backfillStatus": "failed", "lastError": "backfill failed: shopify admin: HTTP 502"},
	})
	job := ComplianceJob{Topic: TopicRedact, StoreID: testStoreID, CustomerGID: "gid://shopify/Customer/777000112", ReceivedAt: h.now, DueAt: h.now}

	outcome, err := h.conn.runComplianceJob(context.Background(), h.store(t), job)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.engine.callsTo("datasyncQueueBackfill"); len(got) != 0 {
		t.Errorf("a redaction with no backfill running or queued queued %v", got)
	}
	if strings.Contains(outcome, "again") {
		t.Errorf("outcome = %q, want only what was scrubbed", outcome)
	}
}

// A restart that cannot be queued fails the job with the reason: the scrub
// ran, and an export begun before it may still be applied.
func TestARedactionWhoseRestartFailsIsNotRecordedDone(t *testing.T) {
	db := openTestDB(t, "customers/redact whose restart fails")
	if db == nil {
		return
	}
	h := redactionHarness(t, db, testStoreID, map[string]map[string]any{"order": {"backfillStatus": "running", "backfillCursor": "c-1"}})
	h.engine.fail["datasyncQueueBackfill"] = errString("no connector bound")
	job := ComplianceJob{Topic: TopicRedact, StoreID: testStoreID, CustomerGID: "gid://shopify/Customer/777000113", ReceivedAt: h.now, DueAt: h.now}

	_, err := h.conn.runComplianceJob(context.Background(), h.store(t), job)
	if err == nil || !strings.Contains(err.Error(), "scrubbed") || !strings.Contains(err.Error(), "order") || !strings.Contains(err.Error(), "no connector bound") {
		t.Fatalf("err = %v, want the job failed, saying the scrub ran and which backfill could not be started again", err)
	}
	if audits := h.engine.callsTo("createAuditEvent"); len(audits) != 1 || !strings.Contains(audits[0], "shopify_customer_redacted") {
		t.Errorf("audits = %v, want the redaction that did run recorded", audits)
	}
}

// The purge deletes the store's sync state with its mirror, so it leaves no
// backfill to start again. Should a row be there all the same once it is done
// -- written by a pass that was in flight -- it is started again like any
// other.
func TestAPurgeStartsAgainABackfillStillInFlightOnceItIsDone(t *testing.T) {
	db := openTestDB(t, "shop/redact restarts a backfill still in flight")
	if db == nil {
		return
	}
	// A store of this test's own: the purge deletes every row of the store it
	// is given, and the database is shared.
	const purged = "dbtest-acme-restart"
	h := redactionHarness(t, db, purged, map[string]map[string]any{"order": {"backfillStatus": "running", "backfillCursor": "c-1"}})
	job := ComplianceJob{Topic: TopicShopRedact, StoreID: purged, ReceivedAt: h.now, DueAt: h.now}

	outcome, err := h.conn.runComplianceJob(context.Background(), Store{ID: purged, Domain: "acme-widgets.myshopify.com"}, job)
	if err != nil {
		t.Fatal(err)
	}
	if got := restartedDomains(t, h); strings.Join(got, ",") != purged+"/order" {
		t.Errorf("restarted %v, want the one backfill still in flight", got)
	}
	if !strings.Contains(outcome, "purged ") || !strings.Contains(outcome, "started 1 backfill(s) again from nothing") {
		t.Errorf("outcome = %q, want what was purged and that one backfill was started again", outcome)
	}
}
