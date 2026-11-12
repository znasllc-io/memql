package datasync

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// shopify_version_db_test.go -- the rule that every mirror write carries the
// origin's version, against a real engine over a real Postgres: what an
// upgraded instance finds already in its mirror, and the one re-read that
// heals it.
//
// Moved here from the connector's package with the rest of the tests that
// need the runtime (shopify_version_test.go says why). A state of the origin
// read at a chosen time used to be built by reaching into the connector's
// mapper; it is now what the connector makes of it itself: a delivery fetched
// at that time, a sweep's page listed at it.

// oldRuleRow writes a product row as an earlier build wrote it: updatedAt and
// syncedAt both the time it was read, and nothing saying where the version
// came from.
func oldRuleRow(t *testing.T, r *holdRig, gid, title, readAt string) {
	t.Helper()
	err := NewEngineMirrorWriter(engineSeam{r.eng}).WriteMirror(
		auth.ContextWithConnectorActor(context.Background(), shopify.ConnectorName), shopify.ConnectorName, memqlsync.MirrorWrite{
			Concept: generated.ConceptID("product"), RowId: shopify.MirrorRowID(r.storeID, gid),
			Payload: map[string]any{"storeId": r.storeID, "gid": gid, "title": title, "updatedAt": readAt, "syncedAt": readAt, "deleted": false},
		})
	if err != nil {
		t.Fatalf("write the old row: %v", err)
	}
}

// delivered works a products/update notice whose fetch finds this state of the
// product, read at `read`.
func (r *holdRig) delivered(name, gid, title, updatedAt string, read time.Time) DispatchResult {
	r.t.Helper()
	r.clock = func() time.Time { return read }
	r.origin.reply("ShopifyFetchProduct", productNode(gid, title, updatedAt))
	return r.dispatch(r.stage(name, "products/update", gid))
}

// A ROW WRITTEN BEFORE THIS RULE holds a read time where the version should
// be, and no versionSource. Compared against the origin's updatedAt -- which
// is earlier than the time the row was read -- it would refuse every honest
// write until the origin changed the record again. It holds no version
// instead, so the origin's state replaces it on either writer: the runtime's
// applier and a sweep's heal. Once replaced, the row is guarded as any other.
func TestARowWrittenBeforeThisRuleIsReplacedByTheOriginsState(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	gid := func(n int) string { return fmt.Sprintf("gid://shopify/Product/%d", 13000+n) }
	read := time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC)

	// The applier: the origin's 11:00 state, read at 12:30, over a row read at
	// 12:00 under the old rule.
	oldRuleRow(t, r, gid(1), "Stale", "2026-11-11T12:00:00Z")
	if res := r.delivered("fresh", gid(1), "Fresh", "2026-11-11T11:00:00Z", read); res.Applied != 1 || r.title(gid(1)) != "Fresh" {
		t.Fatalf("applied %d (stale %d), title %q: the origin's 11:00 state must replace a row read at 12:00 under the old rule", res.Applied, res.Stale, r.title(gid(1)))
	}
	if res := r.delivered("older", gid(1), "Older", "2026-11-11T10:00:00Z", read); res.Stale != 1 || r.title(gid(1)) != "Fresh" {
		t.Errorf("stale %d, title %q: once replaced, the row refuses an older state", res.Stale, r.title(gid(1)))
	}

	// A sweep's heal.
	oldRuleRow(t, r, gid(2), "Stale", "2026-11-11T12:00:00Z")
	sweep := func(title, updatedAt string) memqlsync.ReconcileReport {
		t.Helper()
		r.origin.reply("ShopifyListProduct", originListPage("products", false, "", map[string]any{"id": gid(2), "title": title, "updatedAt": updatedAt}))
		report, err := r.conn.ReconcilePartition(context.Background(), r.storeID, generated.ConceptID("product"), time.Time{})
		if err != nil {
			t.Fatalf("the sweep: %v", err)
		}
		return report
	}
	if report := sweep("Fresh", "2026-11-11T11:00:00Z"); report.Healed != 1 || r.title(gid(2)) != "Fresh" {
		t.Fatalf("healed %d, title %q: a sweep must replace a row read under the old rule", report.Healed, r.title(gid(2)))
	}
	if report := sweep("Older", "2026-11-11T10:00:00Z"); report.Healed != 0 || r.title(gid(2)) != "Fresh" {
		t.Errorf("healed %d, title %q: a sweep must refuse an older state, as the applier does", report.Healed, r.title(gid(2)))
	}
}

// A DOMAIN READ BEFORE THIS RULE IS READ ONCE MORE. A complete backfill that
// began before every bulk line carried the origin's version may have put an
// older snapshot over a newer delivery, so the boot pass queues it again from
// the start, once: not again after it completes, nor at a second boot. A
// backfill running at boot is left alone by the queue -- and because its early
// pages were written under the old rule, it begins again by itself at its
// next step.
//
// The boot pass is the node's own (shopify.Bootstrap), and the queue it asks
// for is the runtime's builtin, registered on the engine as on a node.
func TestADomainReadBeforeThisRuleIsQueuedOnceMore(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	ctx := context.Background()
	seed := func(concept, status, cursor string) {
		t.Helper()
		if err := r.state.PatchSyncState(OperatorContext(ctx), generated.ConceptID(concept), shopify.ConnectorName, string(memqlsync.DirectionInbound), r.storeID,
			func(SyncState) (SyncPatch, bool) {
				return SyncPatch{"backfillStatus": status, "backfillCursor": cursor}, true
			}); err != nil {
			t.Fatalf("seed %s: %v", concept, err)
		}
	}
	runs := func() int { return r.origin.countOp("ShopifyBulkRun") }
	// One export per domain, each with its own file.
	exports := map[string]string{
		"gid://shopify/BulkOperation/1": r.origin.serveJSONL("/download/customers.jsonl", originBulkLines(
			map[string]any{"id": "gid://shopify/Customer/13001", "firstName": "Ada", "updatedAt": "2026-11-11T09:00:00Z"})),
		"gid://shopify/BulkOperation/2": r.origin.serveJSONL("/download/products-again.jsonl", originBulkLines(
			map[string]any{"id": "gid://shopify/Product/13001", "title": "Shirt", "updatedAt": "2026-11-11T09:00:00Z"})),
	}
	productExport := regexp.MustCompile(`query ShopifyBulkProduct\b`)
	r.origin.replyWith("ShopifyBulkRun", func(vars map[string]any) any {
		if document, _ := vars["query"].(string); productExport.MatchString(document) {
			return originStartedOperation("gid://shopify/BulkOperation/2")
		}
		return originStartedOperation("gid://shopify/BulkOperation/1")
	})
	r.origin.replyWith("ShopifyBulkStatus", func(vars map[string]any) any {
		id, _ := vars["id"].(string)
		return originOperationStatus(id, "COMPLETED", exports[id])
	})

	// The customers were read whole before the rule; the products are being
	// read at boot, from a cursor the old rule wrote.
	seed("customer", "complete", "")
	oldCursor := fmt.Sprintf(`{"op":"gid://shopify/BulkOperation/7","url":%q}`, r.origin.serveJSONL("/download/products.jsonl", ""))
	seed("product", "running", oldCursor)

	boot := func() { shopify.Bootstrap(ctx, r.boot, slog.New(slog.DiscardHandler)) }
	boot()
	if st := r.syncState("customer"); st.BackfillStatus != "queued" || st.BackfillCursor != "" {
		t.Fatalf("customer = %s at %q, want queued from the start: its last backfill began before the rule", st.BackfillStatus, st.BackfillCursor)
	}
	if st := r.syncState("product"); st.BackfillStatus != "running" || st.BackfillCursor != oldCursor {
		t.Fatalf("product = %s at %q, want the running backfill left as it was", st.BackfillStatus, st.BackfillCursor)
	}

	before := runs()
	r.backfillToItsEnd("customer")
	if got := runs() - before; got != 1 {
		t.Errorf("the customers' re-read started %d exports, want 1", got)
	}

	// The running one begins again by itself: one more export, read whole.
	before = runs()
	r.backfillToItsEnd("product")
	if got := runs() - before; got != 1 {
		t.Errorf("the products' backfill begun under the old rule started %d exports, want 1 more read", got)
	}

	// Read under the rule, both stay complete: a second boot queues neither.
	boot()
	for _, concept := range []string{"customer", "product"} {
		if st := r.syncState(concept); st.BackfillStatus != "complete" {
			t.Errorf("a second boot set %s to %s, want it left complete: it was read under the rule", concept, st.BackfillStatus)
		}
	}
}

// THE MARKER MUST NOT OUTLIVE THE WRITE THAT SET IT. During a rolling
// upgrade a node on the earlier build writes a row with its read time as
// updatedAt and no versionSource; the engine's insert read-merges an omitted
// field from the stored row (component/memql/executor_mutation.go, the
// ENGINE-LEVEL READ-MERGE), so the stored versionSource survives beside a
// version that is not the origin's. The marker counts only beside the read
// time of the write that set it, so that row reads as written before the
// rule, and the origin's state replaces it.
func TestAnOlderBuildsWriteLeavesNoMarkerBehind(t *testing.T) {
	r := newHoldRig(t)
	if r == nil {
		return
	}
	const gid = "gid://shopify/Product/13101"

	// Marked: written under the rule.
	if res := r.delivered("marked", gid, "Marked", "2026-11-11T11:00:00Z", time.Date(2026, 11, 11, 11, 30, 0, 0, time.UTC)); res.Applied != 1 {
		t.Fatalf("the marked write applied %d", res.Applied)
	}
	if row := r.record(gid); row["versionSource"] != memqlsync.VersionFromOrigin {
		t.Fatalf("the marked row is %v, want it to carry the marker, or this test proves nothing", row)
	}
	// The earlier build's write: its read time as updatedAt and syncedAt, no marker.
	oldRuleRow(t, r, gid, "Older build", "2026-11-11T12:00:00Z")
	// The origin's state, older than that read time: applied.
	res := r.delivered("origin", gid, "Origin", "2026-11-11T11:45:00Z", time.Date(2026, 11, 11, 12, 30, 0, 0, time.UTC))
	if res.Applied != 1 || r.title(gid) != "Origin" {
		t.Errorf("applied %d (stale %d), title %q: a row an earlier build rewrote must not keep the marker the rule set", res.Applied, res.Stale, r.title(gid))
	}
}
