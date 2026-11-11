package wholesalepack_test

// write_authz_db_test.go -- EVERY WRITING BUILTIN AUTHORIZES ITS CALLER ITSELF.
//
// The pack's writer stamps internal origin, and a write at internal origin is
// not owner-checked by the engine's row gate (component/memql/
// rowauthz_write_guard.go). So the row's tier protects nothing from a pack
// builtin: each builtin that writes decides for itself who may, before it
// writes. These run the refusals through the real engine, against the rows
// they protect.
//
// Postgres-gated like every *_db_test.go.

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// asOperator is a cluster operator's own signed-in authority.
func asOperator(userID string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: auth.RoleOwner})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userID})
}

// versionCount is how many versions of one row are stored: a write that must
// not happen is measured by the version it did not add.
func versionCount(t *testing.T, concept, rowID string) int {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	if !strings.HasPrefix(rowID, concept+":") {
		rowID = concept + ":" + rowID
	}
	var n int
	if err := db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE concept = ? AND id = ?`, concept, rowID).
		Scan(context.Background(), &n); err != nil {
		t.Fatalf("count versions of %s: %v", rowID, err)
	}
	return n
}

func settingsOf(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, storeID string) map[string]any {
	t.Helper()
	rows := wsRows(t, eng, ctx, `query wholesaleSettingsForStore(storeId: `+wsQuote(storeID)+`)`)
	if len(rows) == 0 {
		t.Fatalf("the merchant reads no settings for %s", storeID)
	}
	return rows[0]
}

// SETTINGS ARE THE CLUSTER'S OPERATORS' TO SET. A merchant -- even the store's
// own -- is refused, so nobody can open, close or re-route another store's
// program or take its row; an operator's change keeps the row's owner.
func TestLiveE2E_OnlyAnOperatorOrInternalOriginSetsSettings(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, storeLive, _ := wholesaleScope()
	other, _, _ := wholesaleScope()
	other += "-other"
	openApplications(t, eng, asSiteOwner(merchant), storeLive, wholesalepack.AdapterCustomerTag)
	before := versionCount(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(storeLive))

	for name, ctx := range map[string]context.Context{
		"the store's own merchant": asSiteOwner(merchant),
		"another merchant":         asSiteOwner(other),
	} {
		_, err := eng.Execute(ctx, `builtin wholesaleSetSettings(applicationsOpen: false, `+
			`entitlementAdapter: "shopifyB2B", storeId: `+wsQuote(storeLive)+`)`)
		if err == nil {
			t.Fatalf("%s rewrote the store's wholesale settings", name)
		}
		if !strings.Contains(err.Error(), "cluster's operators") {
			t.Fatalf("%s was refused for the wrong reason: %v", name, err)
		}
	}
	if got := versionCount(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(storeLive)); got != before {
		t.Fatalf("refused calls wrote %d settings versions", got-before)
	}
	row := settingsOf(t, eng, asSiteOwner(merchant), storeLive)
	if row["applicationsOpen"] != true || row["entitlementAdapter"] != wholesalepack.AdapterCustomerTag ||
		row["ownerUserId"] != merchant {
		t.Fatalf("the settings changed under refused calls: %+v", row)
	}

	// AN OPERATOR CHANGES THEM, AND THE ROW STAYS THE MERCHANT'S.
	wsExec(t, eng, asOperator("operator-"+merchant), `builtin wholesaleSetSettings(applicationsOpen: false, `+
		`entitlementAdapter: "customerTag", storeId: `+wsQuote(storeLive)+`)`)
	row = settingsOf(t, eng, asSiteOwner(merchant), storeLive)
	if row["applicationsOpen"] != false {
		t.Fatalf("the operator's change did not land: %+v", row)
	}
	if row["ownerUserId"] != merchant {
		t.Fatalf("an operator's write moved the settings to %v; the store's merchant %q must keep them",
			row["ownerUserId"], merchant)
	}
}

// AN APPLICATION IS NEVER WRITTEN OVER. Any signed-in caller who reaches the
// builtin can name submissionId, so a second submission at a taken id is
// refused without a word about the row, and a repeat by its own submitter
// answers it and writes nothing.
func TestLiveE2E_AnApplicationIsNeverWrittenOver(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, storeLive, _ := wholesaleScope()
	other, otherStore, _ := wholesaleScope()
	other += "-other"
	otherStore += "-other"
	openApplications(t, eng, asSiteOwner(merchant), storeLive, wholesalepack.AdapterCustomerTag)
	openApplications(t, eng, asSiteOwner(other), otherStore, wholesalepack.AdapterCustomerTag)

	appID := submit(t, eng, asSiteOwner(merchant), storeLive, "Original Trading", "first@original.example")
	const concept = "v1:wholesale:application"
	original := wsRows(t, eng, asSiteOwner(merchant), `query applicationById(applicationId: `+wsQuote(appID)+`)`)
	if len(original) != 1 || versionCount(t, concept, appID) != 1 {
		t.Fatalf("the first submission did not land as one version: %+v", original)
	}

	// ANOTHER CALLER, THE SAME ID.
	_, err := eng.Execute(asSiteOwner(other), `builtin wholesaleSubmitApplication(applicantEmail: "x@takeover.example"`+
		`, applicantName: "Taker", companyName: "Takeover Ltd", storeId: `+wsQuote(otherStore)+
		`, submissionId: `+wsQuote(appID)+`)`)
	if err == nil {
		t.Fatal("a second caller's submission at a taken id was accepted")
	}
	for _, leak := range []string{"Original Trading", "first@original.example", storeLive, merchant} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("the refusal reveals %q about a row the caller cannot read: %v", leak, err)
		}
	}
	if got := versionCount(t, concept, appID); got != 1 {
		t.Fatalf("the refused submission wrote a version: %d versions", got)
	}
	after := wsRows(t, eng, asSiteOwner(merchant), `query applicationById(applicationId: `+wsQuote(appID)+`)`)
	for _, f := range []string{"ownerUserId", "storeId", "companyName", "applicantName", "applicantEmail", "createdAt"} {
		if after[0][f] != original[0][f] {
			t.Fatalf("%s changed from %v to %v", f, original[0][f], after[0][f])
		}
	}

	// THE SAME SUBMISSION AGAIN answers and writes nothing.
	wsExec(t, eng, asSiteOwner(merchant), `builtin wholesaleSubmitApplication(applicantEmail: "first@original.example"`+
		`, applicantName: "Sam Rivers", companyName: "Original Trading", siteId: "site-wholesale-e2e", storeId: `+
		wsQuote(storeLive)+`, submissionId: `+wsQuote(appID)+`)`)
	if got := versionCount(t, concept, appID); got != 1 {
		t.Fatalf("a repeated submission wrote a second version: %d versions", got)
	}
}

// TWO SUBMISSIONS AT ONE ID AT ONCE leave one row: the row-absent fence
// decides between them inside the write.
func TestLiveE2E_TwoSubmissionsAtOneIdAtOnceLeaveOneRow(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, storeLive, _ := wholesaleScope()
	openApplications(t, eng, asSiteOwner(merchant), storeLive, wholesalepack.AdapterCustomerTag)
	appID := "sub-" + id.NewShortId()

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, company := range []string{"First Racer", "Second Racer"} {
		wg.Add(1)
		go func(company string) {
			defer wg.Done()
			<-start
			_, _ = eng.Execute(asSiteOwner(merchant), `builtin wholesaleSubmitApplication(applicantEmail: "race@race.example"`+
				`, applicantName: "Racer", companyName: `+wsQuote(company)+`, storeId: `+wsQuote(storeLive)+
				`, submissionId: `+wsQuote(appID)+`)`)
		}(company)
	}
	close(start)
	wg.Wait()
	if got := versionCount(t, "v1:wholesale:application", appID); got != 1 {
		t.Fatalf("two submissions at one id at once left %d versions, want 1", got)
	}
}

// A CALLER WHO CANNOT READ AN APPLICATION CANNOT DECIDE IT. The decision's
// write is no longer owner-checked, so the read under the caller is the check.
func TestLiveE2E_ACallerWhoCannotReadAnApplicationCannotDecideIt(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, storeLive, _ := wholesaleScope()
	openApplications(t, eng, asSiteOwner(merchant), storeLive, wholesalepack.AdapterCustomerTag)
	appID := submit(t, eng, asSiteOwner(merchant), storeLive, "Not Yours", "buyer@notyours.example")

	stranger := asSiteOwner(merchant + "-stranger")
	_, err := eng.Execute(stranger, `builtin wholesaleRecordDecision(applicationId: `+wsQuote(appID)+
		`, decidedBy: "stranger", principalKind: "client", transition: "approve")`)
	if err == nil || !strings.Contains(err.Error(), "not readable by this caller") {
		t.Fatalf("a caller who cannot read the application decided it, or was refused for another reason: %v", err)
	}
	if rows := wsRows(t, eng, asSiteOwner(merchant),
		`query decisionsForApplication(applicationId: `+wsQuote(appID)+`)`); len(rows) != 0 {
		t.Fatalf("the refused decision left %d rows", len(rows))
	}
}
