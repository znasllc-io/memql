package shopify

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// grant_recorded_test.go -- what a recorded grant changes about lateness, and
// a sweep that never refuses from a grant it has not just read.
//
// A connect that records a grant stamps when on the store's health
// (grantRecordedAt). A domain row with no attempt on record -- a domain the
// runtime retried after the connect, waiting for its check -- is measured
// from the later of its last clean sweep and that time; every other row as
// before.

// waitingRow is a domain the runtime retried: no refusal, no attempt, its last
// clean sweep three days ago.
func waitingRow(r map[string]any) {
	r["lastError"] = ""
	r["lastAttemptAt"] = ""
	r["lastReconcileAt"] = verdictNow.Add(-72 * time.Hour).Format(time.RFC3339)
}

func failingFor(v Verdict, concept string) bool {
	for _, r := range v.Reasons {
		if r.Code == ReasonCheckFailing && slices.Equal(r.Concepts, []string{concept}) {
			return true
		}
	}
	return false
}

func TestARetriedDomainWaitsForItsCheckAfterTheGrantIsRecorded(t *testing.T) {
	store := verdictStore()
	rows := setRow(cleanRows(store), "segment", waitingRow)
	in := verdictInputOf(store, rows)
	in.GrantRecordedAt = verdictNow.Add(-10 * time.Minute)
	v := computeVerdict(in)
	if len(v.Reasons) != 0 || v.State == VerdictNeedsAttention {
		t.Errorf("straight after the connect: state %q, reasons %v; a retried domain waits for its check", v.State, reasonCodes(v))
	}
	if !v.GrantRecordedAt.Equal(in.GrantRecordedAt) {
		t.Errorf("the verdict carries grantRecordedAt %v, want %v", v.GrantRecordedAt, in.GrantRecordedAt)
	}
	if got := verdictPayload(v)["grantRecordedAt"]; got != in.GrantRecordedAt.Format(time.RFC3339) {
		t.Errorf("the reply's grantRecordedAt = %v", got)
	}
}

func TestARetriedBackfillReadsStarting(t *testing.T) {
	store := verdictStore()
	for name, edit := range map[string]func(map[string]any){
		"never swept clean": func(r map[string]any) {
			r["backfillStatus"], r["lastError"], r["lastAttemptAt"], r["lastReconcileAt"] = "queued", "", "", ""
		},
		"swept clean long ago": func(r map[string]any) {
			waitingRow(r)
			r["backfillStatus"] = "queued"
		},
	} {
		in := verdictInputOf(store, setRow(cleanRows(store), "customer", edit))
		in.GrantRecordedAt = verdictNow.Add(-10 * time.Minute)
		if v := computeVerdict(in); v.State != VerdictStarting || len(v.Reasons) != 0 {
			t.Errorf("%s: state %q, reasons %v; want starting while the backfill is queued", name, v.State, reasonCodes(v))
		}
	}
}

func TestTheAllowanceRunsFromTheRecordedGrant(t *testing.T) {
	store := verdictStore()
	in := verdictInputOf(store, setRow(cleanRows(store), "segment", waitingRow))
	// Segments are checked every six hours: twice that, and a minute, since
	// the approval with no pass having run.
	in.GrantRecordedAt = verdictNow.Add(-12*time.Hour - time.Minute)
	if v := computeVerdict(in); !failingFor(v, "segment") {
		t.Errorf("reasons %v; past the allowance from the approval the segments' check is failing", reasonCodes(v))
	}
}

func TestADomainWithAnAttemptOnRecordIsJudgedAsBefore(t *testing.T) {
	store := verdictStore()
	rows := setRow(cleanRows(store), "segment", func(r map[string]any) {
		waitingRow(r)
		r["lastAttemptAt"] = verdictNow.Add(-3 * time.Hour).Format(time.RFC3339)
	})
	for _, grantAt := range []time.Time{{}, verdictNow, verdictNow.Add(-time.Minute)} {
		in := verdictInputOf(store, rows)
		in.GrantRecordedAt = grantAt
		if v := computeVerdict(in); !failingFor(v, "segment") {
			t.Errorf("grant recorded %v: reasons %v; a domain with an attempt on record is flagged as before", grantAt, reasonCodes(v))
		}
	}
}

func TestAStoreRowWithoutTheKeyIsJudgedAsToday(t *testing.T) {
	store := verdictStore()
	if at := grantRecordedAtOf(store); !at.IsZero() {
		t.Fatalf("a row without the key reads %v", at)
	}
	in := verdictInputOf(store, setRow(cleanRows(store), "segment", waitingRow))
	if v := computeVerdict(in); !failingFor(v, "segment") {
		t.Errorf("reasons %v; with no grant time the retried row reads as it does today", reasonCodes(v))
	}
	// The key read back from a row.
	store.Health = map[string]any{grantRecordedKey: "2026-11-11T11:50:00Z"}
	if at := grantRecordedAtOf(store); !at.Equal(time.Date(2026, 11, 11, 11, 50, 0, 0, time.UTC)) {
		t.Errorf("grantRecordedAtOf = %v", at)
	}
}

func TestAnOwnerWhoApprovesInsideTheWaitIsNotMailed(t *testing.T) {
	store := verdictStore()
	opened := verdictNow.Add(-35 * time.Minute)
	// The episode the older approval opened, not yet mailed.
	troubled := computeVerdict(verdictInputOf(store, refusedAtTheOrigin(cleanRows(store), "segment", "read_customers")))
	troubled.Since = opened
	previous := recordedAs(troubled)
	// The owner approved ten minutes in; the pass 35 minutes in reads the
	// retried row.
	in := verdictInputOf(store, setRow(cleanRows(store), "segment", waitingRow))
	in.GrantRecordedAt = opened.Add(10 * time.Minute)
	in.Previous = previous
	now := computeVerdict(in)
	plan := planWatch(previous, now, defaultStoreAlertWait, verdictNow)
	if plan.Alert {
		t.Errorf("an owner who approved inside the wait was mailed: state %q, reasons %v", now.State, reasonCodes(now))
	}
	if plan.Recover != "" {
		t.Errorf("a recovery was planned for an episode nobody was mailed about: %+v", plan)
	}
	// The recovery rule is as it was: a mailed episode is recovered once the
	// store reads clear.
	mailed := recordedAs(troubled, mailedFor(opened))
	if p := planWatch(mailed, now, defaultStoreAlertWait, verdictNow); p.Recover != verdictStamp(opened) {
		t.Errorf("a mailed episode was not recovered: %+v", p)
	}
}

func TestAConnectStampsWhenItRecordedTheGrant(t *testing.T) {
	stamps := func(h *writeHarness) []string {
		var out []string
		for _, c := range h.engine.callsNamed("recordStoreHealth") {
			if strings.Contains(c.query, `"`+grantRecordedKey+`"`) {
				if !c.internal {
					t.Errorf("the stamp ran as %+v, want operatorContext", c)
				}
				out = append(out, c.query)
			}
		}
		return out
	}
	h := reconnectHarness(t, managedRow(map[string]any{"scopesGranted": olderManagedScopes()}))
	h.managedConnect(t, managedConnectScopes())
	got := stamps(h)
	want := `"` + grantRecordedKey + `": "` + h.integ.connector.now().UTC().Format(time.RFC3339) + `"`
	if len(got) != 1 || !strings.Contains(got[0], want) {
		t.Fatalf("grant stamps = %v, want one carrying %s", got, want)
	}
	if h.statementIndex("recordStoreHealth", grantRecordedKey) < h.statementIndex("updateStore") ||
		h.statementIndex("recordStoreHealth", grantRecordedKey) > h.statementIndex("datasyncRetryRefused") {
		t.Error("the grant was stamped before the row was written or after the retries")
	}
	// A connect that fails before the row is written stamps nothing.
	h = newWriteHarness(t)
	h.engine.fail["createStore"] = errString("the database went away")
	h.integ.connector.background = func(func()) {}
	grant := h.grant()
	grant.Scopes, grant.OfflineGrant = managedConnectScopes(), `{"accessToken":"`+callbackAdminToken+`"}`
	h.integ.connector.writeConnect(context.Background(), callbackState(managedCredentialSource), grant)
	if got := stamps(h); len(got) != 0 {
		t.Errorf("a connect whose row was never written stamped %v", got)
	}
}

func TestAConnectThatFailsAfterRecordingTheGrantStillRetries(t *testing.T) {
	h := newWriteHarness(t)
	h.admin.httpStatus("ShopifyStorefrontTokenCreate", http.StatusForbidden)
	if got, kept := h.writeKept(credentialSourcePending); got != "storefront_token_failed" || kept != connectResultConnected {
		t.Fatalf("result %q, kept %q; want the mint to fail after the grant was kept", got, kept)
	}
	if n := len(h.retried(t)); n == 0 {
		t.Error("a connect that failed after recording the grant asked no retry")
	}
}

func TestASweepRefusesOnlyFromAGrantItHasJustRead(t *testing.T) {
	h := newHarness(t)
	// The cached store list still holds the old grant, without read_content;
	// the stored row already holds the new one.
	withScopes(t, h, "read_products", "read_orders")
	h.engine.setRows("storeById", []map[string]any{{
		"id": testStoreID, "domain": "acme-widgets.myshopify.com", "adminTokenRef": "ACME_ADMIN",
		"webhookSecretRef": "ACME_WEBHOOK", "status": StatusLive, "ownerUserId": "user-1",
		"scopesGranted": toAny([]string{"read_products", "read_orders", "read_content"}),
	}})
	h.admin.reply("ShopifyListBlog", listPageReply("blogs", false, ""))
	_, err := h.conn.Reconcile(context.Background(), "v1:shopify:blog", h.now)
	if memqlsync.IsNotGranted(err) {
		t.Fatalf("the sweep refused from the cached grant: %v", err)
	}
	if n := h.admin.countOp("ShopifyListBlog"); n != 1 {
		t.Errorf("the origin was asked %d times; the fresh grant holds the domain", n)
	}
	// With the stored row on the old grant too, the refusal stands and the
	// origin is not asked.
	h.engine.setRows("storeById", []map[string]any{{
		"id": testStoreID, "domain": "acme-widgets.myshopify.com", "adminTokenRef": "ACME_ADMIN", "status": StatusLive,
		"scopesGranted": toAny([]string{"read_products", "read_orders"}),
	}})
	before := h.admin.countOp("ShopifyListBlog")
	if _, err := h.conn.Reconcile(context.Background(), "v1:shopify:blog", h.now); !memqlsync.IsNotGranted(err) {
		t.Errorf("the fresh grant lacks the domain and the sweep answered %v", err)
	}
	if h.admin.countOp("ShopifyListBlog") != before {
		t.Error("the origin was asked about a domain the fresh grant cannot reach")
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.freshReads["storeById"]) == 0 {
		t.Fatal("no store read recorded")
	}
	for _, fresh := range h.engine.freshReads["storeById"] {
		if !fresh {
			t.Error("the grant was read without bypassing the result cache")
		}
	}

}

func TestABackfillRefusesOnlyFromAGrantItHasJustRead(t *testing.T) {
	h := newHarness(t)
	withScopes(t, h, "read_products")
	h.engine.setRows("storeById", []map[string]any{{
		"id": testStoreID, "domain": "acme-widgets.myshopify.com", "adminTokenRef": "ACME_ADMIN", "status": StatusLive,
		"scopesGranted": toAny([]string{"read_products", "read_orders"}),
	}})
	_, err := h.conn.BackfillPartition(context.Background(), testStoreID, "v1:shopify:order", "")
	if memqlsync.IsNotGranted(err) {
		t.Errorf("the backfill refused from the cached grant: %v", err)
	}
}
