package wholesalepack_test

// storefront_gate_db_test.go -- THE STOREFRONT FORM PASSES THE STORE'S GATE TOO
// (ruling 56), and A SET-UP THAT TIES AN EXISTING ROW CHANGES NOTHING ELSE,
// against a real database, through the real shopper handler.
//
// A store takes applications only through a site of the organization its
// program is tied to. A post through any other site -- one of another
// organization, or one of none -- or on a store whose settings row is tied to
// no organization, is refused exactly as a closed store refuses it, and writes
// nothing.
//
// Postgres-gated like every *_db_test.go.

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// storefrontScene is one person who owns sites in two organizations.
type storefrontScene struct {
	merchant, owner, accountA, accountB string
}

func newStorefrontScene(t *testing.T, eng *memql.MemQLEngine, label string) storefrontScene {
	t.Helper()
	merchant, _, _ := wholesaleScope()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	s := storefrontScene{merchant: merchant + "-" + label, accountA: "acct-" + label + "-a-" + n, accountB: "acct-" + label + "-b-" + n}
	// THE PERSON IS ONE OF EACH ORGANIZATION'S PEOPLE: a site of an
	// organization is read only by its people, the shopper path's owner too.
	for _, account := range []string{s.accountA, s.accountB} {
		seedOrganization(t, eng, account)
		own := "group-own-" + account
		seedGroup(t, eng, own, "custom", account)
		seedPersonIn(t, eng, s.merchant, s.merchant, "writer", true, own, account, "added")
	}
	s.owner = userRef(s.merchant)
	return s
}

// site seeds one of the person's sites bound to store, of account ("" for none).
func (s storefrontScene) site(t *testing.T, eng *memql.MemQLEngine, store, account string) string {
	t.Helper()
	id := "site-sf-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedSiteIn(t, eng, id, s.merchant, store, account)
	if owner := siteOwnerOf(t, eng, id); owner != s.owner {
		t.Fatalf("the site is owned by %q, want %q", owner, s.owner)
	}
	return id
}

// setUp sets store up as an operator naming site, open or closed.
func setUp(t *testing.T, eng *memql.MemQLEngine, store, site string, open bool) {
	t.Helper()
	wsExec(t, eng, asOperator("operator-"+store), `builtin wholesaleSetSettings(applicationsOpen: `+strconv.FormatBool(open)+
		`, entitlementAdapter: "customerTag", storeId: `+quote(store)+`, siteId: `+quote(site)+`)`)
}

func applicationForm(email string) url.Values {
	return url.Values{"companyName": {"Kestrel Cycles"}, "applicantName": {"Ada Example"}, "applicantEmail": {email}}
}

// closedRedirect is where a shopper is sent by a store that is set up for its
// organization and closed: the refusal every other refusal here must equal.
func closedRedirect(t *testing.T, eng *memql.MemQLEngine, s storefrontScene) string {
	t.Helper()
	_, store, _ := wholesaleScope()
	store += "-closed"
	site := s.site(t, eng, store, s.accountA)
	setUp(t, eng, store, site, false)
	rec := postApplication(t, eng, site, store, s.owner, applicationForm("ada@kestrel.example.com"))
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/wholesale/problem") {
		t.Fatalf("a closed store answered %d %q", rec.Code, loc)
	}
	return loc
}

// ONE OWNER, TWO ORGANIZATIONS, ONE STORE set up for A: a post through A's site
// is written, tied to A; a post through B's site is refused as a closed store
// refuses it, and writes nothing.
func TestAStoreTakesApplicationsOnlyThroughASiteOfItsOrganization(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newStorefrontScene(t, eng, "sf-two")
	closed := closedRedirect(t, eng, s)
	_, store, _ := wholesaleScope()
	siteA, siteB := s.site(t, eng, store, s.accountA), s.site(t, eng, store, s.accountB)
	setUp(t, eng, store, siteA, true)

	if loc := postApplication(t, eng, siteA, store, s.owner, applicationForm("a@kestrel.example.com")).Header().Get("Location"); !strings.HasPrefix(loc, "/wholesale/thank-you") {
		t.Fatalf("a post through the store's own organization's site was refused: %q", loc)
	}
	apps := wsRows(t, eng, seedCtx(), `query applicationsForStore(storeId: `+quote(store)+`)`)
	if len(apps) != 1 || bareID(asString(apps[0]["accountId"])) != s.accountA {
		t.Fatalf("the store holds %+v, want one application tied to %s", apps, s.accountA)
	}
	if loc := postApplication(t, eng, siteB, store, s.owner, applicationForm("b@kestrel.example.com")).Header().Get("Location"); loc != closed {
		t.Fatalf("a post through another organization's site redirected to %q, want the closed store's %q", loc, closed)
	}
	if n := unsetApplications(t, eng, store); n != 1 {
		t.Fatalf("the refused post wrote: the store holds %d applications, want 1", n)
	}
}

// A STORE WHOSE SETTINGS ROW IS TIED TO NO ORGANIZATION takes no application,
// open or not: refused as a closed store, nothing written.
func TestAStoreWhoseSettingsAreUntiedTakesNoApplication(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newStorefrontScene(t, eng, "sf-untied")
	closed := closedRedirect(t, eng, s)
	_, store, _ := wholesaleScope()
	site := s.site(t, eng, store, s.accountA)
	wsExec(t, eng, seedCtx(), `setWholesaleSettingsRow(settingsId: `+quote(wholesalepack.SettingsRowID(store))+
		`, ownerUserId: `+quote(s.owner)+`, storeId: `+quote(store)+`, applicationsOpen: true, entitlementAdapter: "customerTag"`+
		`, introText: "", accountId: "", siteId: "", staffEmail: "", termsRequired: false, linkLifetimeDays: 0, acceptanceText: "")`)
	if loc := postApplication(t, eng, site, store, s.owner, applicationForm("a@kestrel.example.com")).Header().Get("Location"); loc != closed {
		t.Fatalf("a post on an untied store redirected to %q, want the closed store's %q", loc, closed)
	}
	if n := unsetApplications(t, eng, store); n != 0 {
		t.Fatalf("the refused post wrote %d applications", n)
	}
}

// A SECOND BOUND SITE WITH NO ORGANIZATION on a store set up for A takes no
// application: refused as a closed store, nothing written.
func TestASiteOfNoOrganizationOnAStoreSetUpForAnotherTakesNoApplication(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newStorefrontScene(t, eng, "sf-none")
	closed := closedRedirect(t, eng, s)
	_, store, _ := wholesaleScope()
	siteA, siteNone := s.site(t, eng, store, s.accountA), s.site(t, eng, store, "")
	setUp(t, eng, store, siteA, true)
	if loc := postApplication(t, eng, siteNone, store, s.owner, applicationForm("a@kestrel.example.com")).Header().Get("Location"); loc != closed {
		t.Fatalf("a post through a site of no organization redirected to %q, want the closed store's %q", loc, closed)
	}
	if n := unsetApplications(t, eng, store); n != 0 {
		t.Fatalf("the refused post wrote %d applications", n)
	}
}

// TYING AN EXISTING ROW CHANGES NOTHING ELSE: an untied row that is open, on its
// route, with its intro text, is tied by the OS's set-up -- which names closed,
// no route and no text -- and stays open, on its route, with its text; the
// receipt says so.
func TestTyingAnUntiedRowKeepsEverySettingItHolds(t *testing.T) {
	eng := liveWholesaleEngine(t)
	g := newGateScene(t, eng, "tie-keeps", false)
	wsExec(t, eng, seedCtx(), `setWholesaleSettingsRow(settingsId: `+quote(wholesalepack.SettingsRowID(g.a.store))+
		`, ownerUserId: `+quote(g.a.owner)+`, storeId: `+quote(g.a.store)+`, applicationsOpen: true, entitlementAdapter: "customerTag"`+
		`, introText: "Trade accounts for bicycle shops.", accountId: "", siteId: "", staffEmail: "desk@example.com", termsRequired: false`+
		`, linkLifetimeDays: 14, acceptanceText: "")`)
	got := answerOf(t, eng, asOperator("operator-"+g.a.staff), `builtin wholesaleSetSettings(applicationsOpen: false, entitlementAdapter: ""`+
		`, introText: "", storeId: `+quote(g.a.store)+`, siteId: `+quote(g.a.site)+`)`)
	if got["applicationsOpen"] != true || got["entitlementAdapter"] != "customerTag" {
		t.Fatalf("the receipt says %+v, want the stored open and route", got)
	}
	row := latestPayload(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(g.a.store))
	if bareID(asString(row["accountId"])) != g.a.account || row["applicationsOpen"] != true || row["entitlementAdapter"] != "customerTag" ||
		row["introText"] != "Trade accounts for bicycle shops." || row["staffEmail"] != "desk@example.com" || row["ownerUserId"] != g.a.owner {
		t.Fatalf("the tied row is %+v, want it tied to %s and otherwise as it was", row, g.a.account)
	}
}

// A SET-UP NAMING A SITE OF ANOTHER OWNER THAN THE EXISTING ROW'S is refused,
// exactly, and writes nothing: a row keeps its owner, and tying it to another
// owner's organization would split the two.
func TestASetUpNamingAnotherOwnersSiteIsRefused(t *testing.T) {
	eng := liveWholesaleEngine(t)
	g := newGateScene(t, eng, "tie-owner", true)
	before := versionCount(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(g.a.store))
	_, err := eng.Execute(asOperator("operator-"+g.a.staff), `builtin wholesaleSetSettings(applicationsOpen: true, storeId: `+
		quote(g.a.store)+`, siteId: `+quote(g.bound)+`)`)
	if err == nil || err.Error() != wholesalepack.AnotherOwnerSentence {
		t.Fatalf("a set-up naming another owner's site: %v, want %q", err, wholesalepack.AnotherOwnerSentence)
	}
	if got := versionCount(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(g.a.store)); got != before {
		t.Fatalf("the refused set-up wrote %d versions", got-before)
	}
}

// A DIRECT CALL OF THE SUBMISSION is written only as the shopper path writes it:
// through the store's admitted site, carrying that site's organization, by that
// site's owner. A signed-in stranger, and a member of the store's organization
// who is not the site's owner, naming the right site -- with its organization
// or with none -- are refused as a closed store, and nothing is written. The
// shopper path through the handler still writes.
func TestADirectSubmissionIsWrittenOnlyAsTheShopperPathWritesIt(t *testing.T) {
	eng := liveWholesaleEngine(t)
	s := newStorefrontScene(t, eng, "sf-direct")
	_, store, _ := wholesaleScope()
	site := s.site(t, eng, store, s.accountA)
	setUp(t, eng, store, site, true)
	member := "user-member-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedPersonIn(t, eng, member, member, "writer", true, "group-own-"+s.accountA, s.accountA, "added")
	stranger := "user-stranger-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedUser(t, eng, stranger)
	const closed = "wholesale: this store is not accepting applications"
	for _, caller := range []string{stranger, member} {
		for _, account := range []string{"", `, accountId: ` + quote(s.accountA)} {
			_, err := eng.Execute(signedIn(caller), `builtin wholesaleSubmitApplication(applicantEmail: "direct@kestrel.example.com"`+
				`, applicantName: "Ada Example", companyName: "Kestrel Cycles", siteId: `+quote(site)+`, storeId: `+quote(store)+
				account+`, submissionId: `+quote("sub-"+strconv.FormatInt(time.Now().UnixNano(), 36))+`)`)
			if err == nil || err.Error() != closed {
				t.Errorf("%s naming the store's site (account %q): %v, want %q", caller, account, err, closed)
			}
		}
	}
	if n := unsetApplications(t, eng, store); n != 0 {
		t.Fatalf("refused direct submissions wrote %d applications", n)
	}
	if loc := postApplication(t, eng, site, store, s.owner, applicationForm("a@kestrel.example.com")).Header().Get("Location"); !strings.HasPrefix(loc, "/wholesale/thank-you") {
		t.Fatalf("the shopper path through the handler was refused: %q", loc)
	}
	apps := wsRows(t, eng, seedCtx(), `query applicationsForStore(storeId: `+quote(store)+`)`)
	if len(apps) != 1 || bareID(asString(apps[0]["accountId"])) != s.accountA || apps[0]["ownerUserId"] != s.owner {
		t.Fatalf("the shopper path wrote %+v, want one application of %s owned by %s", apps, s.accountA, s.owner)
	}
}

// AN UNTIED ROW THAT IS OPEN AND REQUIRES ITS TERMS, with all three documents
// published, is tied by the set-up and stays open: the terms rule is met.
func TestTyingAnOpenUntiedRowThatRequiresItsTermsKeepsItOpen(t *testing.T) {
	eng := liveWholesaleEngine(t)
	g := newGateScene(t, eng, "tie-terms", false)
	for _, kind := range []string{"terms", "map", "priceSheet"} {
		wsExec(t, eng, seedCtx(), `appendWholesaleDocumentVersion(ownerUserId: `+quote(g.a.owner)+`, storeId: `+quote(g.a.store)+
			`, accountId: `+quote(g.a.account)+`, kind: `+quote(kind)+`, label: `+quote(kind+" 2026")+`, body: "Net 30.", sha256: "x"`+
			`, publishedBy: `+quote(userRef(g.a.staff))+`, publishedAt: `+quote(time.Now().UTC().Format(time.RFC3339Nano))+`)`)
	}
	wsExec(t, eng, seedCtx(), `setWholesaleSettingsRow(settingsId: `+quote(wholesalepack.SettingsRowID(g.a.store))+
		`, ownerUserId: `+quote(g.a.owner)+`, storeId: `+quote(g.a.store)+`, applicationsOpen: true, entitlementAdapter: "customerTag"`+
		`, introText: "", accountId: "", siteId: "", staffEmail: "", termsRequired: true, linkLifetimeDays: 0, acceptanceText: "")`)
	wsExec(t, eng, asOperator("operator-"+g.a.staff), `builtin wholesaleSetSettings(applicationsOpen: false, entitlementAdapter: ""`+
		`, storeId: `+quote(g.a.store)+`, siteId: `+quote(g.a.site)+`)`)
	row := latestPayload(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(g.a.store))
	if bareID(asString(row["accountId"])) != g.a.account || row["applicationsOpen"] != true || row["termsRequired"] != true {
		t.Fatalf("the tied row is %+v, want it tied, open and requiring its terms", row)
	}
}
