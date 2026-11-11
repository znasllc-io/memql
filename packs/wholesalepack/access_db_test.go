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
	return wsRows(t, eng, ctx, `query decisionsForApplication(applicationId: `+quote(s.appID)+`)`)
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
