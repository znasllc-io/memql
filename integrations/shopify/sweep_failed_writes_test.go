package shopify

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/datasync"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// sweep_failed_writes_test.go -- a sweep whose writes failed is not a clean
// sweep.
//
// heal counts the rows it could not write and goes on to the next. The sweep
// used to drop that count, answer a clean report, and the runtime moved the
// watermark past rows the mirror never took; the verdict then dropped a
// given-up delivery of that domain because it "has a clean sweep", and the
// store read Recording with an order missing.

// unwritable fails every mirror write of the rows it names, as an engine that
// refuses one record does, and lets every other call through.
type unwritable struct {
	memql.IntegrationEngineAccess
	rows map[string]bool
}

func (u *unwritable) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "insert(") {
		for row := range u.rows {
			if strings.Contains(q, `"`+row+`"`) {
				return nil, errString("the mirror refused this row")
			}
		}
	}
	return u.IntegrationEngineAccess.Execute(ctx, q)
}

// refuses makes the rig's mirror refuse the rows of these gids of a concept.
func (r *mirrorRig) refuses(gids ...string) {
	rows := map[string]bool{}
	for _, gid := range gids {
		rows[MirrorRowID(testStoreID, gid)] = true
	}
	r.conn.engine = &unwritable{IntegrationEngineAccess: r.mirror, rows: rows}
}

func TestASweepInWhichAWriteFailedIsNotAnsweredClean(t *testing.T) {
	r := newMirrorRig(t)
	r.now = at("10:10:00")
	order := func(n string) map[string]any {
		return map[string]any{"id": "gid://shopify/Order/" + n, "name": "#100" + n, "updatedAt": "2026-08-23T10:00:00Z"}
	}
	r.admin.reply("ShopifyListOrder", listPageReply("orders", false, "", order("1"), order("2"), order("3")))
	r.refuses("gid://shopify/Order/2")

	report, err := r.conn.reconcileByUpdatedAt(context.Background(), r.store(t), generated.Types["order"], at("09:00:00"))
	if err == nil {
		t.Fatalf("a sweep that could not write an order answered a clean report: %+v", report)
	}
	if memqlsync.IsPermanent(err) || memqlsync.IsNotGranted(err) || memqlsync.IsNotRequested(err) || memqlsync.IsNotImplemented(err) {
		t.Fatalf("err = %v, want an ordinary failure: the next sweep reads the row again", err)
	}
	if !strings.Contains(err.Error(), "1 row") || !strings.Contains(err.Error(), "could not be written") {
		t.Errorf("err = %v, want it to say how many rows could not be written", err)
	}
	// One row's failure does not stop the sweep: the rows after it are healed.
	for _, n := range []string{"1", "3"} {
		if row := r.row("order", "gid://shopify/Order/"+n); row == nil {
			t.Errorf("order %s, listed beside the one that failed, was not written", n)
		}
	}
	if row := r.row("order", "gid://shopify/Order/2"); row != nil {
		t.Fatalf("the refused order was written: %v", row)
	}
	if report.Checked != 3 || report.Healed != 2 {
		t.Errorf("report = %+v, want three checked and two healed", report)
	}
}

func TestARelistInWhichAWriteFailedIsNotAnsweredClean(t *testing.T) {
	spec := generated.Types["giftCard"]
	card := func(n string) map[string]any {
		return map[string]any{"id": "gid://shopify/GiftCard/" + n, "updatedAt": "2026-08-23T10:00:00Z"}
	}
	t.Run("a listed row", func(t *testing.T) {
		r := newMirrorRig(t)
		r.now = at("10:10:00")
		r.admin.reply(spec.ListOp, listPageReply(spec.ListQuery, false, "", card("1"), card("2")))
		r.refuses("gid://shopify/GiftCard/1")
		// A card the origin no longer lists: the pass still retires it.
		r.held("giftCard", "gid://shopify/GiftCard/9", "2026-08-23T09:00:00Z", at("09:30:00"))

		report, err := r.conn.relist(context.Background(), r.store(t), spec)
		if err == nil || !strings.Contains(err.Error(), "could not be written") {
			t.Fatalf("a relist that could not write a card answered %+v, %v", report, err)
		}
		if row := r.row("giftCard", "gid://shopify/GiftCard/2"); row == nil {
			t.Error("the card listed beside the one that failed was not written")
		}
		if row := r.row("giftCard", "gid://shopify/GiftCard/9"); row["deleted"] != true {
			t.Errorf("the card the listing no longer names was not retired: %v", row)
		}
	})
	t.Run("a tombstone", func(t *testing.T) {
		r := newMirrorRig(t)
		r.now = at("10:10:00")
		r.admin.reply(spec.ListOp, listPageReply(spec.ListQuery, false, "", card("1")))
		r.held("giftCard", "gid://shopify/GiftCard/9", "2026-08-23T09:00:00Z", at("09:30:00"))
		r.refuses("gid://shopify/GiftCard/9")

		if report, err := r.conn.relist(context.Background(), r.store(t), spec); err == nil {
			t.Fatalf("a relist that could not retire a card answered a clean report: %+v", report)
		}
		if row := r.row("giftCard", "gid://shopify/GiftCard/9"); row["deleted"] == true {
			t.Fatal("the refused tombstone was written")
		}
	})
}

// THE WATERMARK HOLDS. Through the runtime's own runner: the sweep's failure
// is recorded as the domain's error, and its lastReconcileAt -- what the next
// sweep reads from -- is not moved.
func TestASweepInWhichAWriteFailedLeavesTheWatermarkWhereItWas(t *testing.T) {
	r := newMirrorRig(t)
	r.now = at("10:10:00")
	r.granted("read_orders")
	r.admin.reply("ShopifyListOrder", listPageReply("orders", false, "",
		map[string]any{"id": "gid://shopify/Order/1", "name": "#1001", "updatedAt": "2026-08-23T10:00:00Z"}))
	r.refuses("gid://shopify/Order/1")
	memqlsync.UnbindForTest(ConnectorName)
	if err := memqlsync.Bind(r.conn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { memqlsync.UnbindForTest(ConnectorName) })

	state := datasync.NewStore(anyEngine{r.mirror})
	runner := datasync.NewRunner(state, datasync.NewApplier(state, datasync.NewEngineMirrorWriter(anyEngine{r.mirror})), slog.New(slog.DiscardHandler))
	if _, err := runner.Reconcile(context.Background(), ConnectorName, generated.ConceptID("order"), testStoreID); err == nil {
		t.Fatal("the runner recorded a sweep whose write failed as clean")
	}
	var wrote []string
	for _, q := range r.engine.calls() {
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
