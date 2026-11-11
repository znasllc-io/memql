package wholesalepack_test

// access_db_test.go -- THE STORE'S OWN PEOPLE DECIDE ITS APPLICATIONS, THROUGH
// AN ORGANIZATION PART (U6), against a real database and a real engine.
//
// A person granted `execute app:deployables/wholesale` in a store's
// organization decides that store's applications; the same grant in another
// organization reaches nothing here; the cluster's operators read every store's
// applications and decide none. The organizations, groups and grants are seeded
// the way a store's people are granted them, through seed_helpers_db_test.go.
//
// Postgres-gated like every *_db_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// accessStore is one store of one organization, in the production shape: the
// organization's account group opens Deployables and holds the wholesale part,
// and its person is in that group. The site's owner -- the merchant -- is one
// of the organization's people too (a site of an organization is read only by
// its people), in a group of its own that holds no part.
type accessStore struct {
	merchant, owner, store, site, account, staff, appID string
}

// newAccessStore seeds the organization, its person, its site and one
// application posted through the shopper path, so the application carries the
// site's organization.
func newAccessStore(t *testing.T, eng *memql.MemQLEngine, label string) accessStore {
	t.Helper()
	merchant, store, _ := wholesaleScope()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	s := accessStore{merchant: merchant + "-" + label, store: store + "-" + label,
		account: "acct-" + label + "-" + n, site: "site-" + label + "-" + n, staff: "user-staff-" + label + "-" + n}
	group := seedOrganization(t, eng, s.account)
	grantGroup(t, eng, group, "read", "app:deployables")
	grantGroup(t, eng, group, "execute", "app:deployables/wholesale")
	seedPersonIn(t, eng, s.staff, s.staff, "writer", true, group, s.account, "added")
	own := "group-own-" + label + "-" + n
	seedGroup(t, eng, own, "custom", s.account)
	seedPersonIn(t, eng, s.merchant, s.merchant, "writer", true, own, s.account, "added")
	seedSiteIn(t, eng, s.site, s.merchant, s.store, s.account)
	s.owner = siteOwnerOf(t, eng, s.site)
	openApplications(t, eng, asSiteOwner(s.owner), s.store, wholesalepack.AdapterCustomerTag)
	rec := postApplication(t, eng, s.site, s.store, s.owner, url.Values{
		"companyName": {"Trading " + label}, "applicantName": {"Sam Rivers"}, "applicantEmail": {"sam@" + label + ".example.com"},
	})
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/wholesale/thank-you") {
		t.Fatalf("the post answered %d %q, want 303 to the thank-you page", rec.Code, loc)
	}
	apps := wsRows(t, eng, signedIn(s.merchant), `query applicationsForStore(storeId: `+quote(s.store)+`)`)
	if len(apps) != 1 || bareID(asString(apps[0]["accountId"])) != s.account {
		t.Fatalf("the merchant reads %+v, want one application of %s", apps, s.account)
	}
	s.appID = bareID(asString(apps[0]["id"]))
	return s
}

func (s accessStore) decisions(t *testing.T, eng *memql.MemQLEngine, ctx context.Context) []map[string]any {
	t.Helper()
	// Decisions name their application in canonical form (ruling 27).
	return wsRows(t, eng, ctx, `query decisionsForApplication(applicationId: `+quote("v1:wholesale:application:"+s.appID)+`)`)
}

// approve is a decision with neither principalKind nor decidedBy: both are
// optional now, and ignored.
func (s accessStore) approve(eng *memql.MemQLEngine, ctx context.Context) error {
	_, err := eng.Execute(ctx, `builtin wholesaleRecordDecision(applicationId: `+quote(s.appID)+
		`, transition: "approve", note: "known to us")`)
	return err
}

// A PERSON HOLDING THE PART IN THE STORE'S ORGANIZATION APPROVES ITS
// APPLICATION, and the decision names them and the organization.
func TestAPersonHoldingThePartApprovesTheirOrganizationsApplication(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "holder")
	if err := s.approve(eng, signedIn(s.staff)); err != nil {
		t.Fatalf("the store's person holding the part could not approve: %v", err)
	}
	rows := s.decisions(t, eng, signedIn(s.staff))
	if len(rows) != 1 || rows[0]["transition"] != "approve" || rows[0]["decidedBy"] != userRef(s.staff) ||
		bareID(asString(rows[0]["accountId"])) != s.account {
		t.Fatalf("the decision log reads %+v, want one approve decided by %s for %s", rows, userRef(s.staff), s.account)
	}
}

// THE PART IS HELD PER ORGANIZATION: a person holding it in A is refused on B's
// application, deciding it and retrying its access alike, and nothing is written.
func TestAPersonInOneOrganizationIsRefusedOnAnothersApplication(t *testing.T) {
	eng := liveWholesaleEngine(t)
	a, b := newAccessStore(t, eng, "orga"), newAccessStore(t, eng, "orgb")
	for name, call := range map[string]string{
		"wholesaleRecordDecision": `builtin wholesaleRecordDecision(applicationId: ` + quote(b.appID) + `, transition: "approve")`,
		"wholesaleRetryAccess":    `builtin wholesaleRetryAccess(applicationId: ` + quote(b.appID) + `)`,
	} {
		_, err := eng.Execute(signedIn(a.staff), call)
		if err == nil || !strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) {
			t.Fatalf("%s on another organization's application: %v, want %s", name, err, memql.CodeCapabilityNotHeld)
		}
	}
	if rows := b.decisions(t, eng, signedIn(b.merchant)); len(rows) != 0 {
		t.Fatalf("B's application carries %d decisions after A's person was refused", len(rows))
	}
}

// A CLUSTER OPERATOR READS EVERY STORE'S APPLICATIONS AND DECIDES NONE (X4).
func TestAClusterOperatorCannotDecide(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "operator")
	operator := asOperator("operator-" + s.staff)
	if rows := wsRows(t, eng, operator, `query applicationById(applicationId: `+quote(s.appID)+`)`); len(rows) != 1 {
		t.Fatalf("the operator reads %d applications, want the store's one", len(rows))
	}
	err := s.approve(eng, operator)
	if err == nil || !strings.Contains(err.Error(), "the cluster's operators host the store's applications and do not decide them") {
		t.Fatalf("a cluster operator's decision: %v", err)
	}
	if rows := s.decisions(t, eng, operator); len(rows) != 0 {
		t.Fatalf("the operator's refused decision left %d rows", len(rows))
	}
}

// AN OPERATOR SETS A STORE UP FOR ITS SITE, CLOSED AND WITH NO ROUTE: the row is
// the site's, carrying its organization, so its people read it.
func TestAnOperatorSetsUpAStoreClosedWithNoRouteForItsSite(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, site, account := tiedStore(t, eng, "setup-site")
	wsExec(t, eng, asOperator("operator-"+merchant), `builtin wholesaleSetSettings(siteId: `+quote(site)+
		`, storeId: `+quote(store)+`, applicationsOpen: false, entitlementAdapter: "")`)
	row := settingsOf(t, eng, asOperator("operator-"+merchant), store)
	if bareID(asString(row["accountId"])) != account || bareID(asString(row["siteId"])) != site ||
		row["applicationsOpen"] != false || asString(row["entitlementAdapter"]) != "" || row["ownerUserId"] != userRef(merchant) {
		t.Fatalf("the set-up wrote %+v, want closed, no route, owned by %s, tied to %s through %s", row, merchant, account, site)
	}
}

// THE SITE'S OWNER READS THE DECISION A PERSON OF THE STORE MADE (N2): the
// decision is owned by the application's merchant, whoever decided it.
func TestTheSiteOwnerReadsTheStaffMembersDecision(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "n2")
	if err := s.approve(eng, signedIn(s.staff)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	rows := s.decisions(t, eng, signedIn(s.merchant))
	if len(rows) != 1 || rows[0]["decidedBy"] != userRef(s.staff) || rows[0]["ownerUserId"] != userRef(s.merchant) {
		t.Fatalf("the merchant reads %+v, want the staff member's decision, owned by the merchant", rows)
	}
}

// A DECISION WRITTEN BEFORE THIS VERSION -- no ownerUserId, no accountId -- is
// read by nobody on the store's side under the new tier until the backfill
// names its merchant, and by the merchant after it.
func TestADecisionWrittenBeforeThisVersionIsReadableByTheMerchantAfterTheBackfill(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, site, _ := tiedStore(t, eng, "old-decision")
	r := seedOldRows(t, eng, merchant, store, site)
	if rows := wsRows(t, eng, signedIn(merchant), `query decisionsForApplication(applicationId: `+quote(r.appID)+`)`); len(rows) != 0 {
		t.Fatalf("before the backfill the merchant reads %d decisions of a row that names no merchant", len(rows))
	}
	backfill(t, eng, r)
	rows := wsRows(t, eng, signedIn(merchant), `query decisionsForApplication(applicationId: `+quote(r.appID)+`)`)
	if len(rows) != 1 || rows[0]["note"] != "known to us" {
		t.Fatalf("after the backfill the merchant reads %+v, want the old decision", rows)
	}
}

// seedOldLog writes an untied application of the merchant and, for each
// transition, one decision exactly as this pack wrote them before the account
// tie: no ownerUserId, no accountId. Under the decision's new tier the store's
// side reads none of them until the backfill.
func seedOldLog(t *testing.T, eng *memql.MemQLEngine, merchant, store string, transitions ...string) string {
	t.Helper()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	appID := "old-log-" + n
	insert := func(concept, rowID string, payload map[string]any) {
		raw, _ := json.Marshal(payload)
		wsExec(t, eng, seedCtx(), `insert(`+quote(concept)+`, id=`+quote(rowID)+`, payload=`+string(raw)+`)`)
	}
	insert("v1:wholesale:application", appID, map[string]any{"ownerUserId": userRef(merchant), "storeId": store,
		"companyName": "Old Log Trading", "applicantName": "Pat Old", "applicantEmail": "pat@oldlog.example.com"})
	for i, transition := range transitions {
		insert("v1:wholesale:applicationDecision", appID+"-d"+strconv.Itoa(i), map[string]any{"applicationId": appID,
			"storeId": store, "transition": transition, "decidedBy": "user-old-decider-" + n, "principalKind": "client"})
		time.Sleep(5 * time.Millisecond) // a log's order is its rows' creation order
	}
	return appID
}

// A DECISION'S LEGALITY IS JUDGED AGAINST THE WHOLE LOG (X27): an old rejection
// or revocation the merchant cannot read still makes their approve illegal,
// refused exactly as an illegal transition always is.
func TestAnApproveOverAnOldUnreadableFinalDecisionIsRefusedAgainstTheLog(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	decider := asDecider(t, eng, merchant)
	for state, log := range map[string][]string{
		wholesalepack.StateRejected: {"reject"},
		wholesalepack.StateRevoked:  {"approve", "revoke"},
	} {
		appID := seedOldLog(t, eng, merchant, store, log...)
		if rows := wsRows(t, eng, decider, `query decisionsForApplication(applicationId: `+quote(appID)+`)`); len(rows) != 0 {
			t.Fatalf("%s: the merchant reads %d old decisions; the case needs a log they cannot read", state, len(rows))
		}
		_, err := eng.Execute(decider, `builtin wholesaleRecordDecision(applicationId: `+quote(appID)+`, transition: "approve")`)
		want := wholesalepack.LegalTransition(state, wholesalepack.TransitionApprove)
		if err == nil || !strings.Contains(err.Error(), want.Error()) {
			t.Fatalf("%s: an approve over the old log answered %v, want %v", state, err, want)
		}
	}
}

// THE STATE OF AN APPLICATION WHOSE ONLY DECISION IS AN OLD UNTIED APPROVAL is
// approved, for the shopper path's borrowed authority and for the merchant
// signed in.
func TestTheStateOfAnOldApprovalIsApprovedForItsReaders(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	appID := seedOldLog(t, eng, merchant, store, "approve")
	for name, ctx := range map[string]context.Context{"the shopper path": asSiteOwner(merchant), "the merchant": signedIn(merchant)} {
		got := answerOf(t, eng, ctx, `builtin wholesaleApplicationState(applicationId: `+quote(appID)+`)`)
		if got["state"] != wholesalepack.StateApproved {
			t.Fatalf("%s is told %v, want approved", name, got["state"])
		}
	}
}

// TWO UPDATES FROM ONE READ: the save that loses the fence is refused with the
// sentence the OS shows, and the change that landed first stays.
func TestTheSecondOfTwoUpdatesFromOneReadIsRefused(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, site, _ := tiedStore(t, eng, "fence-update")
	operator := asOperator("operator-" + merchant)
	wsExec(t, eng, operator, `builtin wholesaleSetSettings(siteId: `+quote(site)+`, storeId: `+quote(store)+
		`, applicationsOpen: true, entitlementAdapter: "customerTag")`)
	fired := false
	_, err := wholesalepack.UpdateSettingsForTest(operator, eng,
		map[string]any{"siteId": site, "storeId": store, "introText": "the second save"},
		func(context.Context, string, map[string]any) {
			if fired {
				return
			}
			fired = true
			wsExec(t, eng, operator, `builtin wholesaleUpdateSettings(siteId: `+quote(site)+`, storeId: `+quote(store)+
				`, introText: "the first save")`)
		})
	if !fired {
		t.Fatal("the concurrent save did not run")
	}
	if err == nil || !strings.Contains(err.Error(), "These settings changed while you were editing. Reload and try again.") {
		t.Fatalf("the save that lost its fence answered %v", err)
	}
	if got := settingsOf(t, eng, operator, store)["introText"]; got != "the first save" {
		t.Fatalf("the settings read %v after the refused save, want the first save kept", got)
	}
}

// RULING 27, MEASURED: the engine does not join a bare and a canonical
// application id in a decision's field, so a log written under one form would
// be missed by a fold asked with the other. The pack writes the canonical form
// and folds both, so a decision made under either form is found under either.
func TestADecisionIsFoundUnderEitherFormOfItsApplicationsId(t *testing.T) {
	eng := liveWholesaleEngine(t)
	for _, canonical := range []bool{false, true} {
		merchant, store, _ := wholesaleScope()
		ctx := asSiteOwner(merchant)
		openApplications(t, eng, ctx, store, wholesalepack.AdapterCustomerTag)
		bare := submit(t, eng, ctx, store, "Either Form Trading", "buyer@either.example.com")
		canon := "v1:wholesale:application:" + bare
		named := bare
		if canonical {
			named = canon
		}
		decide(t, eng, asDecider(t, eng, merchant), named, wholesalepack.TransitionReject, merchant, "either")
		rows := wsRows(t, eng, ctx, `query decisionsForApplication(applicationId: `+quote(canon)+`)`)
		if len(rows) != 1 || rows[0]["applicationId"] != canon {
			t.Fatalf("decided under %q: the row is %+v, want one naming %s", named, rows, canon)
		}
		if raw := wsRows(t, eng, ctx, `query decisionsForApplication(applicationId: `+quote(bare)+`)`); len(raw) != 0 {
			t.Fatalf("the engine now joins the two forms (%d rows under the bare id); the fold's second read is no longer needed", len(raw))
		}
		for _, asked := range []string{bare, canon} {
			if got := answerOf(t, eng, ctx, `builtin wholesaleApplicationState(applicationId: `+quote(asked)+`)`); got["state"] != wholesalepack.StateRejected {
				t.Fatalf("decided under %q, asked under %q: %v, want rejected", named, asked, got["state"])
			}
		}
	}
}

// A PERSON OF THE SAME ORGANIZATION WHO DOES NOT HOLD THE PART decides nothing
// and reads no settings: the merchant here is one, in a group with no part.
func TestAPersonOfTheOrganizationWithoutThePartIsRefused(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "nopart")
	for name, call := range map[string]string{
		"wholesaleRecordDecision": `builtin wholesaleRecordDecision(applicationId: ` + quote(s.appID) + `, transition: "approve")`,
		"wholesaleStoreSettings":  `builtin wholesaleStoreSettings(siteId: ` + quote(s.site) + `, storeId: ` + quote(s.store) + `)`,
	} {
		if _, err := eng.Execute(signedIn(s.merchant), call); err == nil || !strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) {
			t.Fatalf("%s by a person without the part: %v, want %s", name, err, memql.CodeCapabilityNotHeld)
		}
	}
}

// THE RECEIPT NAMES THE DECIDER THE ROW HOLDS.
func TestTheDecisionReceiptNamesTheDeciderTheRowHolds(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newAccessStore(t, eng, "receipt")
	got := answerOf(t, eng, signedIn(s.staff), `builtin wholesaleRecordDecision(applicationId: `+quote(s.appID)+`, transition: "approve")`)
	rows := s.decisions(t, eng, signedIn(s.staff))
	if len(rows) != 1 || got["decidedBy"] != rows[0]["decidedBy"] {
		t.Fatalf("the receipt names %v, the row %+v", got["decidedBy"], rows)
	}
}

// A SET-UP THAT A STAFF SAVE OVERTAKES IS REFUSED, and the staff change stays.
func TestASetUpOvertakenByAStaffSaveIsRefused(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, site, account := tiedStore(t, eng, "fence-setup")
	group := "acct-" + account
	grantGroup(t, eng, group, "read", "app:deployables")
	grantGroup(t, eng, group, "execute", "app:deployables/wholesale")
	staff := "user-staff-fence-setup-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedPersonIn(t, eng, staff, staff, "writer", true, group, account, "added")
	operator := asOperator("operator-" + merchant)
	wsExec(t, eng, operator, `builtin wholesaleSetSettings(siteId: `+quote(site)+`, storeId: `+quote(store)+
		`, applicationsOpen: true, entitlementAdapter: "customerTag")`)
	fired := false
	_, err := wholesalepack.SetSettingsForTest(operator, eng,
		map[string]any{"siteId": site, "storeId": store, "applicationsOpen": false},
		func(context.Context, string, map[string]any) {
			if fired {
				return
			}
			fired = true
			wsExec(t, eng, signedIn(staff), `builtin wholesaleUpdateSettings(siteId: `+quote(site)+`, storeId: `+quote(store)+
				`, introText: "the staff change")`)
		})
	if !fired {
		t.Fatal("the staff save did not run")
	}
	if err == nil || !strings.Contains(err.Error(), wholesalepack.ChangedWhileEditingSentence) {
		t.Fatalf("the overtaken set-up answered %v", err)
	}
	row := settingsOf(t, eng, operator, store)
	if row["introText"] != "the staff change" || row["applicationsOpen"] != true {
		t.Fatalf("the settings read %+v, want the staff change kept and the set-up not applied", row)
	}
}
