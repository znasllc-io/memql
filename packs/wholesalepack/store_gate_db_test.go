package wholesalepack_test

// store_gate_db_test.go -- A STORE ADMITS THE WHOLESALE SECTION'S ACTS ONLY ONCE
// IT IS SET UP, AND ONLY FOR ITS ORGANIZATION (ruling 55), against a real
// database and a real engine.
//
// Two stores that are not set up -- one with no settings row, one whose row
// names no organization -- are called by their own organization's holder
// through their own site, and by another organization's holder through a site
// of theirs bound to the store. Every site-gated builtin refuses with the
// sentence and writes nothing; the set-up ties the store; after it the store's
// own organization is admitted and the other is refused.
//
// Postgres-gated like every *_db_test.go.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// gateScene is organization A's store, not set up, with A's site, and
// organization B's holder with a site of B's bound to A's store.
type gateScene struct {
	a, b  accessStore
	bound string // B's site bound to A's store
}

func newGateScene(t *testing.T, eng *memql.MemQLEngine, label string, untied bool) gateScene {
	t.Helper()
	g := gateScene{a: unsetStore(t, eng, label+"-a"), b: unsetStore(t, eng, label+"-b")}
	g.bound = "site-" + label + "-b-on-a-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedSiteIn(t, eng, g.bound, g.b.merchant, g.a.store, g.b.account)
	if untied {
		// A ROW TIED TO NO ORGANIZATION, as a set-up through one owner's two
		// sites, or by trusted code with no bound site, wrote one before ruling 55.
		wsExec(t, eng, seedCtx(), `setWholesaleSettingsRow(settingsId: `+quote(wholesalepack.SettingsRowID(g.a.store))+
			`, ownerUserId: `+quote(g.a.owner)+`, storeId: `+quote(g.a.store)+`, applicationsOpen: true, entitlementAdapter: "customerTag"`+
			`, introText: "", accountId: "", siteId: "", staffEmail: "", termsRequired: false, linkLifetimeDays: 0, acceptanceText: "")`)
	}
	return g
}

// siteGatedCalls is every site-gated builtin through site on store.
func siteGatedCalls(site, store string) map[string]string {
	on := `siteId: ` + quote(site) + `, storeId: ` + quote(store)
	return map[string]string{
		"wholesaleList":            `builtin wholesaleList(` + on + `)`,
		"wholesaleDecisionLog":     `builtin wholesaleDecisionLog(` + on + `)`,
		"wholesaleBuyers":          `builtin wholesaleBuyers(` + on + `)`,
		"wholesaleStoreSettings":   `builtin wholesaleStoreSettings(` + on + `)`,
		"wholesaleUpdateSettings":  `builtin wholesaleUpdateSettings(` + on + `, applicationsOpen: true, staffEmail: "desk@example.com")`,
		"wholesaleDocuments":       `builtin wholesaleDocuments(` + on + `)`,
		"wholesalePublishDocument": `builtin wholesalePublishDocument(` + on + `, kind: "terms", label: "Planted terms", body: "Net 30.")`,
		"wholesaleAddBuyer": `builtin wholesaleAddBuyer(` + on + `, companyName: "Kestrel Cycles", applicantName: "Ada Example", ` +
			`applicantEmail: "ada@kestrel.example.com")`,
		"wholesaleExportStore": `builtin wholesaleExportStore(` + on + `)`,
	}
}

// storeRowCount is every row of the store a site-gated act could write: its
// applications, its document versions of each kind and its settings versions,
// under both spellings of its id, read as the deployment.
func storeRowCount(t *testing.T, eng *memql.MemQLEngine, store string) int {
	t.Helper()
	n := unsetApplications(t, eng, store)
	for _, form := range []string{store, "v1:shopify:store:" + store} {
		for _, kind := range []string{"terms", "map", "priceSheet"} {
			n += len(wsRows(t, eng, seedCtx(), `query documentVersionsForStore(storeId: `+quote(form)+`, kind: `+quote(kind)+`)`))
		}
		n += versionCount(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(form))
	}
	return n
}

// EVERY SITE-GATED ACT ON A STORE THAT IS NOT SET UP is refused with the
// sentence -- by its own organization's holder through its own site, and by
// another organization's holder through a site of theirs bound to it -- and
// writes nothing; wholesaleStoreSettings answers not set up, with no document.
func TestEverySiteGatedActRefusesAStoreThatIsNotSetUpInTheDatabase(t *testing.T) {
	eng := liveWholesaleEngine(t)
	for _, untied := range []bool{false, true} {
		state := "no settings row"
		if untied {
			state = "a row of no organization"
		}
		g := newGateScene(t, eng, "gate-"+strconv.FormatBool(untied), untied)
		// A DOCUMENT ALREADY UNDER THE STORE'S ID, which no reply may show.
		wsExec(t, eng, seedCtx(), `appendWholesaleDocumentVersion(ownerUserId: `+quote(g.b.owner)+`, storeId: `+quote(g.a.store)+
			`, accountId: `+quote(g.b.account)+`, kind: "terms", label: "Already planted", body: "planted", sha256: "x", publishedBy: `+
			quote(userRef(g.b.staff))+`, publishedAt: `+quote(time.Now().UTC().Format(time.RFC3339Nano))+`)`)
		before := storeRowCount(t, eng, g.a.store)
		for _, caller := range []struct {
			name, staff, site string
		}{{"A's holder through A's site", g.a.staff, g.a.site}, {"B's holder through B's site bound to A's store", g.b.staff, g.bound}} {
			for _, store := range []string{g.a.store, "v1:shopify:store:" + g.a.store} {
				for name, call := range siteGatedCalls(caller.site, store) {
					result, err := eng.Execute(signedIn(caller.staff), call)
					if name == "wholesaleStoreSettings" {
						if err != nil {
							t.Errorf("%s, %s, store %q: %s: %v", state, caller.name, store, name, err)
							continue
						}
						rows := memql.MaterializeRows(result)
						docs, _ := rows[0]["documents"].(map[string]any)
						if len(rows) != 1 || rows[0]["exists"] != false || docs["terms"] != nil || rows[0]["applicationsOpen"] != false {
							t.Errorf("%s, %s, store %q: the settings reply tells something of the store: %+v", state, caller.name, store, rows)
						}
						continue
					}
					if err == nil || !strings.Contains(err.Error(), wholesalepack.NotSetUpSentence) {
						t.Errorf("%s, %s, store %q: %s: %v, want %q", state, caller.name, store, name, err, wholesalepack.NotSetUpSentence)
					}
				}
			}
		}
		if after := storeRowCount(t, eng, g.a.store); after != before {
			t.Fatalf("%s: refused acts wrote %d rows of A's store", state, after-before)
		}
	}
}

// THE PLANTED DOCUMENT: organization B's holder, through a site of B's bound to
// A's store that is not set up, publishes A's terms. Refused, and no version is
// written -- so none can later satisfy A's terms rule or be shown to A's
// applicants.
func TestAnotherOrganizationCannotPlantADocumentInAStoreNotSetUp(t *testing.T) {
	eng := liveWholesaleEngine(t)
	for _, untied := range []bool{false, true} {
		g := newGateScene(t, eng, "plant-"+strconv.FormatBool(untied), untied)
		for _, kind := range []string{"terms", "map", "priceSheet"} {
			_, err := eng.Execute(signedIn(g.b.staff), `builtin wholesalePublishDocument(siteId: `+quote(g.bound)+`, storeId: `+
				quote(g.a.store)+`, kind: `+quote(kind)+`, label: "Planted", body: "Planted by another organization.")`)
			if err == nil || !strings.Contains(err.Error(), wholesalepack.NotSetUpSentence) {
				t.Errorf("untied %v: B publishing A's %s: %v", untied, kind, err)
			}
			for _, form := range []string{g.a.store, "v1:shopify:store:" + g.a.store} {
				if rows := wsRows(t, eng, seedCtx(), `query documentVersionsForStore(storeId: `+quote(form)+`, kind: `+quote(kind)+`)`); len(rows) != 0 {
					t.Fatalf("untied %v: B's refused publish left %+v under %q", untied, rows, form)
				}
			}
		}
	}
}

// THE SET-UP TIES THE STORE: refused with the exact sentence when the site
// cannot be told (two sites bound, none named); for an operator naming A's
// site it ties the row to A, keeping an existing row's owner; after it A's
// holder is admitted and B's is refused as another organization.
func TestTheSetUpTiesTheStoreAndThenOnlyItsOrganizationIsAdmitted(t *testing.T) {
	eng := liveWholesaleEngine(t)
	for _, untied := range []bool{false, true} {
		g := newGateScene(t, eng, "setup-"+strconv.FormatBool(untied), untied)
		operator := asOperator("operator-" + g.a.staff)
		before := versionCount(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(g.a.store))
		_, err := eng.Execute(operator, `builtin wholesaleSetSettings(applicationsOpen: false, storeId: `+quote(g.a.store)+`)`)
		if err == nil || !strings.Contains(err.Error(), wholesalepack.NameTheSiteSentence) {
			t.Fatalf("untied %v: a set-up with two sites bound and none named: %v, want %q", untied, err, wholesalepack.NameTheSiteSentence)
		}
		if got := versionCount(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(g.a.store)); got != before {
			t.Fatalf("untied %v: the refused set-up wrote %d versions", untied, got-before)
		}
		wsExec(t, eng, operator, `builtin wholesaleSetSettings(applicationsOpen: false, storeId: `+quote(g.a.store)+
			`, siteId: `+quote(g.a.site)+`)`)
		row := latestPayload(t, "v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(g.a.store))
		if bareID(asString(row["accountId"])) != g.a.account || bareID(asString(row["siteId"])) != g.a.site || row["ownerUserId"] != g.a.owner {
			t.Fatalf("untied %v: the set-up wrote %+v, want A's row tied to %s", untied, row, g.a.account)
		}
		for name, call := range siteGatedCalls(g.a.site, g.a.store) {
			if name == "wholesaleAddBuyer" || name == "wholesalePublishDocument" || name == "wholesaleUpdateSettings" {
				continue // writes; the reads are enough to show admission
			}
			if _, err := eng.Execute(signedIn(g.a.staff), call); err != nil {
				t.Errorf("untied %v: A's holder after the set-up: %s: %v", untied, name, err)
			}
		}
		for name, call := range siteGatedCalls(g.bound, g.a.store) {
			if _, err := eng.Execute(signedIn(g.b.staff), call); err == nil || !strings.Contains(err.Error(), "another organization") {
				t.Errorf("untied %v: B's holder after the set-up: %s: %v, want another organization", untied, name, err)
			}
		}
	}
}

// THE ACTS ON ONE APPLICATION do not go through the store gate: the organization
// gate judges them against the APPLICATION's organization. So on a store that is
// not set up, an application tied to A is opened, decided, retried and removed
// by A's holder, while B's holder is refused every one. What the approval's
// grant does then depends on the store's row: with none, provisioning records
// no_settings and calls no route; with a row of no organization, it calls the
// route the row names (customerTag here, which reaches Shopify where Shopify is
// wired -- this harness has no Shopify, so the attempt stays pending with
// shopify_error).
func TestTheActsOnOneApplicationOfAStoreNotSetUpAreTheApplicationsOrganizations(t *testing.T) {
	eng := liveWholesaleEngine(t)
	for _, untied := range []bool{false, true} {
		g := newGateScene(t, eng, "appacts-"+strconv.FormatBool(untied), untied)
		appID := "seeded-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		wsExec(t, eng, seedCtx(), `createApplicationRow(applicationId: `+quote(appID)+`, ownerUserId: `+quote(g.a.owner)+
			`, storeId: `+quote(g.a.store)+`, siteId: `+quote(g.a.site)+`, accountId: `+quote(g.a.account)+
			`, companyName: "Plover Cycles", applicantName: "Ida Example", applicantEmail: "ida@plover.example.com", submittedAt: `+happenedNow()+`)`)
		calls := []struct{ name, call string }{
			{"wholesaleGet", `builtin wholesaleGet(applicationId: ` + quote(appID) + `)`},
			{"wholesaleRecordDecision", `builtin wholesaleRecordDecision(applicationId: ` + quote(appID) + `, transition: "approve")`},
			{"wholesaleRetryAccess", `builtin wholesaleRetryAccess(applicationId: ` + quote(appID) + `)`},
			{"wholesaleRemoveBuyer", `builtin wholesaleRemoveBuyer(applicationId: ` + quote(appID) + `, note: "")`},
		}
		for _, c := range calls {
			if _, err := eng.Execute(signedIn(g.b.staff), c.call); err == nil || !strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) {
				t.Errorf("untied %v: B's holder: %s: %v, want %s", untied, c.name, err, memql.CodeCapabilityNotHeld)
			}
		}
		for _, c := range calls {
			if _, err := eng.Execute(signedIn(g.a.staff), c.call); err != nil {
				t.Errorf("untied %v: A's holder: %s on an application of a store not set up: %v", untied, c.name, err)
			}
		}
		rows := decisionsOf(t, eng, signedIn(g.a.staff), appID)
		if len(rows) != 2 || rows[0]["transition"] != "revoke" || rows[1]["transition"] != "approve" {
			t.Fatalf("untied %v: the application's log is %+v, want approve then revoke by A", untied, rows)
		}
		ents := wsRows(t, eng, seedCtx(), `query entitlementForApplication(applicationId: `+quote("v1:wholesale:application:"+appID)+`)`)
		want := map[bool][2]string{false: {"failed", "no_settings"}, true: {"pending", "shopify_error"}}[untied]
		if len(ents) != 1 || ents[0]["state"] != want[0] || ents[0]["errorCode"] != want[1] {
			t.Fatalf("untied %v: the entitlement after the retry is %+v, want %s with %s", untied, ents, want[0], want[1])
		}
	}
}
