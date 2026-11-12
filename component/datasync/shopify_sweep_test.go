package datasync

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// shopify_sweep_test.go -- a sweep whose writes failed, through the runtime's
// own runner.
//
// The connector's sweep counts the rows it could not write and answers an
// error (the connector's own tests hold that). What the runner does with the
// error is the other half: it is what keeps the watermark where it was, so
// the next sweep reads the same rows again, and it is why the verdict does
// not read the domain as swept clean. Moved here from the connector's package
// because it needs the runner.

// THE WATERMARK HOLDS. The sweep's failure is recorded as the domain's error,
// and its lastReconcileAt -- what the next sweep reads from -- is not moved.
func TestASweepInWhichAWriteFailedLeavesTheWatermarkWhereItWas(t *testing.T) {
	r := newShopRig(t)
	r.now = clockAt("10:10:00")
	r.granted("read_orders")
	r.origin.reply("ShopifyListOrder", originListPage("orders", false, "",
		map[string]any{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": "2026-08-23T10:00:00Z"}))
	r.refuses("gid://shopify/Order/1")
	memqlsync.UnbindForTest(shopify.ConnectorName)
	if err := memqlsync.Bind(r.conn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(shopify.ConnectorName) })

	state := NewStore(shopSeam{r.mirror})
	runner := NewRunner(state, NewApplier(state, NewEngineMirrorWriter(shopSeam{r.mirror})), slog.New(slog.DiscardHandler))
	if _, err := runner.Reconcile(context.Background(), shopify.ConnectorName, generated.ConceptID("order"), shopRigStore); err == nil {
		t.Fatal("the runner recorded a sweep whose write failed as clean")
	}
	if n := r.origin.countOp("ShopifyListOrder"); n != 1 {
		t.Fatalf("the origin's orders were listed %d times, want once: the sweep must have run", n)
	}
	if row := r.row("order", "gid://shopify/Order/1"); row != nil {
		t.Fatalf("the refused order was written: %v", row)
	}
	var wrote []string
	for _, q := range r.mirror.sent() {
		if strings.HasPrefix(q, "mutation upsertSyncState(") || strings.HasPrefix(q, "mutation patchSyncState(") {
			wrote = append(wrote, q)
		}
	}
	if len(wrote) == 0 {
		t.Fatal("the failed sweep recorded nothing on the domain's row")
	}
	for _, q := range wrote {
		if strings.Contains(q, "lastReconcileAt") {
			t.Errorf("the watermark moved for a sweep whose write failed:\n%s", q)
		}
	}
	if last := wrote[len(wrote)-1]; !strings.Contains(last, "could not be written") {
		t.Errorf("the domain's row does not carry the failure:\n%s", last)
	}
}
