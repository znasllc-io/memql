package datasync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// runners_refusal_test.go -- what a sweep does with a refusal it cannot
// change.
//
// Two shapes reached a live cluster every ten minutes for days: a domain
// the store never granted (ACCESS_DENIED for read_content) and a generated
// query the origin rejects outright (a field that does not exist). Both were
// logged as sweep failures on every tick, because a failed sweep never
// stamped lastReconcileAt and so was due again immediately. Neither is
// something the next tick could fix.

func TestANotGrantedDomainIsSkippedAndSaysWhyOnce(t *testing.T) {
	engine := newFakeEngine()
	c := mirrorConnector()
	c.reconcileErr = memqlsync.NotGranted("shopify", "read_content")
	r := testRunner(engine, newFakeWriter(), c)

	res, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept)
	if err != nil {
		t.Fatalf("a domain the store did not grant surfaced as a sweep failure: %v", err)
	}
	if !res.Skipped {
		t.Fatal("not marked skipped: the sweep would count it as swept")
	}
	writes := engine.callsContaining("mutation upsertSyncState")
	if len(writes) != 1 {
		t.Fatalf("health writes = %d, want exactly 1 -- the row must say why the domain is idle", len(writes))
	}
	if !strings.Contains(writes[0], "not granted") || !strings.Contains(writes[0], "read_content") {
		t.Errorf("the health row does not name the missing scope: %s", writes[0])
	}
	if strings.Contains(writes[0], "lastReconcileAt") {
		t.Errorf("a skipped domain must not claim a finished sweep: %s", writes[0])
	}
	if !strings.Contains(writes[0], "lastAttemptAt") {
		t.Errorf("the attempt was not stamped, so the domain is due again on the next tick: %s", writes[0])
	}

	// The next tick, with the same refusal already on the row, writes
	// nothing: the health timeline is append-only and 22 domains writing
	// an unchanged reason every ten minutes is a table nobody asked for.
	stored := SyncStateID(testMirrorConcept, "shopify", "inbound")
	engine.seed(`query syncStateFor`, []map[string]any{{
		"id":            stored,
		"conceptId":     testMirrorConcept,
		"connector":     "shopify",
		"direction":     "inbound",
		"lastError":     c.reconcileErr.Error(),
		"lastAttemptAt": testNow.Add(-time.Minute).Format(time.RFC3339),
	}})
	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept); err != nil {
		t.Fatal(err)
	}
	if n := engine.countContaining("mutation upsertSyncState"); n != 1 {
		t.Errorf("an unchanged refusal was written again (%d writes)", n)
	}
	// ...and the domain is not due again until its own interval has run,
	// even though nothing was swept.
	if r.ReconcileDue(context.Background(), "shopify", c.domains[0]) {
		t.Error("a not-granted domain attempted a minute ago is due again")
	}
}

func TestAPermanentSweepFailureWaitsForTheDomainsOwnCadence(t *testing.T) {
	engine := newFakeEngine()
	c := mirrorConnector()
	c.reconcileErr = memqlsync.Permanent(errors.New("shopify admin: undefinedField: Field 'id' doesn't exist on type 'ResourcePublication'"))
	r := testRunner(engine, newFakeWriter(), c)

	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept); err == nil {
		t.Fatal("a permanent failure must still be reported: somebody has to fix the query")
	}
	writes := engine.callsContaining("mutation upsertSyncState")
	if len(writes) != 1 {
		t.Fatalf("health writes = %d, want 1", len(writes))
	}
	if !strings.Contains(writes[0], "lastAttemptAt") {
		t.Fatalf("the refused sweep did not stamp the attempt, so the identical refusal repeats on the next tick: %s", writes[0])
	}
	if strings.Contains(writes[0], "lastReconcileAt") {
		t.Fatalf("the refused sweep moved lastReconcileAt, the updated_at watermark: drift between the last real sweep and the refusal would be skipped once the refusal clears: %s", writes[0])
	}
	if !strings.Contains(writes[0], "ResourcePublication") {
		t.Errorf("the health row lost the reason: %s", writes[0])
	}

	// ReconcileDue reads the attempt back: the domain waits its own interval,
	// while `since` keeps pointing at the last sweep that ran.
	engine.seed(`query syncStateFor`, []map[string]any{{
		"id":              SyncStateID(testMirrorConcept, "shopify", "inbound"),
		"conceptId":       testMirrorConcept,
		"connector":       "shopify",
		"direction":       "inbound",
		"lastReconcileAt": testNow.Add(-5 * time.Hour).Format(time.RFC3339),
		"lastAttemptAt":   testNow.Format(time.RFC3339),
		"lastError":       c.reconcileErr.Error(),
	}})
	if r.ReconcileDue(context.Background(), "shopify", c.domains[0]) {
		t.Error("due again immediately after a permanent refusal")
	}
}

func TestATransientSweepFailureStaysDue(t *testing.T) {
	engine := newFakeEngine()
	c := mirrorConnector()
	c.reconcileErr = errors.New("shopify admin: HTTP 502")
	r := testRunner(engine, newFakeWriter(), c)

	if _, err := r.Reconcile(context.Background(), "shopify", testMirrorConcept); err == nil {
		t.Fatal("a transient failure must be reported")
	}
	writes := engine.callsContaining("mutation upsertSyncState")
	if len(writes) != 1 {
		t.Fatalf("health writes = %d, want 1", len(writes))
	}
	if strings.Contains(writes[0], "lastReconcileAt") {
		t.Errorf("a transient failure stamped lastReconcileAt; the next tick should retry it: %s", writes[0])
	}
}
