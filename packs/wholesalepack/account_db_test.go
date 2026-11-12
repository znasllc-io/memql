package wholesalepack_test

// account_db_test.go -- EVERY ROW CARRIES ITS ORGANIZATION, AND OLD ROWS ARE
// BACKFILLED (U5), against a real database and a real engine.
//
// A shopper's post is stamped with the organization of the site it arrived on,
// and the pack copies it onto everything it writes about that application. Rows
// written before this version have none; the operator's backfill ties them,
// and until it runs they read exactly as they did.
//
// Postgres-gated like every *_db_test.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	memqldsl "github.com/znasllc-io/memql/dsl"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// engineExecutor is the bff's ShopperEngineAdapter: the real engine behind the
// real shopper handler.
type engineExecutor struct{ eng *memql.MemQLEngine }

func (e engineExecutor) Execute(ctx context.Context, q string) (any, error) {
	return e.eng.Execute(ctx, q)
}

// engineSites resolves a site row under the owner's own actor, as the bff's
// shopperSiteReader does. The seeded sites carry no shopperForms switch, and
// the switch is not what these tests are about, so every readable site has it on.
type engineSites struct{ eng *memql.MemQLEngine }

func (s engineSites) ShopperSite(ctx context.Context, siteID, owner string) (*server.ShopperSite, error) {
	result, err := s.eng.Execute(asSiteOwner(owner), `query siteById(siteId: `+quote(siteID)+`)`)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		return nil, nil
	}
	store := func(v any) string {
		m, _ := v.(map[string]any)
		id, _ := m["storeId"].(string)
		return id
	}
	account, _ := rows[0]["accountId"].(string)
	return &server.ShopperSite{ID: siteID, ShopperForms: true, StoreID: store(rows[0]["binding"]),
		PreviewStoreID: store(rows[0]["previewBinding"]), AccountID: account}, nil
}

// siteOwnerOf is the site's ownerUserId as the row stores it, which is what the
// edge stamps as the owner whose authority a shopper's post borrows.
func siteOwnerOf(t *testing.T, eng *memql.MemQLEngine, siteID string) string {
	t.Helper()
	rows := wsRows(t, eng, asOperator("operator-"+siteID), `query siteById(siteId: `+quote(siteID)+`)`)
	if len(rows) != 1 {
		t.Fatalf("an operator reads %d sites at %s", len(rows), siteID)
	}
	return asString(rows[0]["ownerUserId"])
}

// postApplication is a shopper's form post through the real handler.
func postApplication(t *testing.T, eng *memql.MemQLEngine, siteID, storeID, owner string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	h := server.NewShopperHandler(engineExecutor{eng}, engineSites{eng}, nil)
	req := httptest.NewRequest(http.MethodPost, memql.ShopperFormPathPrefix+"wholesale/application",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(memql.ShopperSiteHeader, siteID)
	req.Header.Set(memql.ShopperStoreHeader, storeID)
	req.Header.Set(memql.ShopperOwnerHeader, owner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// latestPayload reads the newest stored version of a row straight from the
// table, for a concept the fixture ships no query for.
func latestPayload(t *testing.T, concept, rowID string) map[string]any {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	if !strings.HasPrefix(rowID, concept+":") {
		rowID = concept + ":" + rowID
	}
	var raw []byte
	if err := db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE concept = ? AND id = ? ORDER BY "createdAt" DESC LIMIT 1`,
		concept, rowID).Scan(context.Background(), &raw); err != nil {
		t.Fatalf("read %s: %v", rowID, err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", rowID, err)
	}
	return out
}

// seedUser writes the user row a site owner has: a site is read under its
// owner's own actor, and the tier admits an identified person only.
func seedUser(t *testing.T, eng *memql.MemQLEngine, userID string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"displayName": userID, "primaryEmail": userID + "@example.test",
		"role": "writer", "active": true})
	if _, err := eng.Execute(seedCtx(), `insert(`+langparser.QuoteString("v1:identity:user")+`, id=`+
		langparser.QuoteString(userID)+`, payload=`+string(payload)+`)`); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func bareID(id string) string {
	if i := strings.LastIndex(id, ":"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// answerOf is the one node a builtin answers with.
func answerOf(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, call string) map[string]any {
	t.Helper()
	rows := wsRows(t, eng, ctx, call)
	if len(rows) != 1 {
		raw, _ := json.Marshal(rows)
		t.Fatalf("%s answered %d rows, want 1: %s", call, len(rows), raw)
	}
	return rows[0]
}

// A SUBMISSION ON A SITE OF ORGANIZATION A writes the application with A and
// source storefront, and hands A to the client's extension, whose row carries
// it too. The people of A read the application without owning it.
func TestASubmissionOnASiteOfOrganizationAWritesTheApplicationAndTheExtensionRowWithA(t *testing.T) {
	memqldsl.RegisterTree("northwind", os.DirFS(filepath.Join("testdata", "clients", "northwind")))
	t.Cleanup(func() { memqldsl.UnregisterTree("northwind") })
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	account, site, staff := "acct-a-"+n, "site-a-"+n, "user-staff-a-"+n
	// The merchant and a person of A who owns nothing here, both in A.
	group := seedOrganization(t, eng, account)
	seedPersonIn(t, eng, merchant, merchant, "writer", true, group, account, "added")
	seedStorePerson(t, eng, account, staff)
	seedSiteIn(t, eng, site, merchant, store, account)
	owner := siteOwnerOf(t, eng, site)
	openApplications(t, eng, asSiteOwner(owner), store, wholesalepack.AdapterCustomerTag)

	rec := postApplication(t, eng, site, store, owner, url.Values{
		"companyName": {"Organization A Trading"}, "applicantName": {"Sam Rivers"},
		"applicantEmail": {"sam@a.example.com"}, "ein": {"12-3456789"},
		"accountId": {"acct-somebody-elses"},
	})
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/wholesale/thank-you") {
		t.Fatalf("the post answered %d %q, want 303 to the thank-you page", rec.Code, loc)
	}

	apps := wsRows(t, eng, asSiteOwner(owner), `query applicationsForStore(storeId: `+quote(store)+`)`)
	if len(apps) != 1 {
		t.Fatalf("the merchant reads %d applications, want 1", len(apps))
	}
	if bareID(asString(apps[0]["accountId"])) != account || apps[0]["source"] != "storefront" || apps[0]["siteId"] != site {
		t.Fatalf("the application was written with accountId %v, source %v, siteId %v; want %s, storefront, %s",
			apps[0]["accountId"], apps[0]["source"], apps[0]["siteId"], account, site)
	}
	appID := bareID(asString(apps[0]["id"]))

	detail := latestPayload(t, "v1:northwind:northwindTaxDetail", appID)
	if bareID(asString(detail["accountId"])) != account || detail["ein"] != "12-3456789" {
		t.Fatalf("the extension's row = %+v, want the EIN and accountId %s", detail, account)
	}

	// THE TIER'S THIRD ARM: a person of A who does not own the site reads it.
	if rows := wsRows(t, eng, asSiteOwner(staff), `query applicationById(applicationId: `+quote(appID)+`)`); len(rows) != 1 {
		t.Fatalf("a person of the application's organization reads %d rows, want 1", len(rows))
	}
}

// A STORE WHOSE ONLY SITE HAS NO ORGANIZATION CANNOT BE SET UP (ruling 55): its
// program would belong to nobody on the store's side. So it takes no
// application either: the shopper's post is refused as by any store with no
// settings. Before ruling 55 the set-up was accepted and the application was
// written with no organization.
func TestAStoreWhoseSiteHasNoOrganizationIsNotSetUpAndTakesNoApplication(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	site := "site-none-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedUser(t, eng, merchant)
	seedSiteIn(t, eng, site, merchant, store, "")
	owner := siteOwnerOf(t, eng, site)
	for _, named := range []string{"", `, siteId: ` + quote(site)} {
		_, err := eng.Execute(auth.ContextWithInternalOrigin(asSiteOwner(owner)), `builtin wholesaleSetSettings(applicationsOpen: true, `+
			`entitlementAdapter: "customerTag", storeId: `+quote(store)+named+`)`)
		if err == nil || !strings.Contains(err.Error(), "belongs to no organization") {
			t.Fatalf("a set-up through a site of no organization (%q): %v", named, err)
		}
	}
	rec := postApplication(t, eng, site, store, owner, url.Values{
		"companyName": {"Untied Trading"}, "applicantName": {"Sam Rivers"}, "applicantEmail": {"sam@none.example.com"},
	})
	if loc := rec.Header().Get("Location"); strings.HasPrefix(loc, "/wholesale/thank-you") {
		t.Fatalf("a store that is not set up took an application: %d %q", rec.Code, loc)
	}
	if apps := wsRows(t, eng, asSiteOwner(owner), `query applicationsForStore(storeId: `+quote(store)+`)`); len(apps) != 0 {
		t.Fatalf("the merchant reads %+v, want none", apps)
	}
}

// oldRows writes an application, a decision, an entitlement and the store's
// settings exactly as this pack wrote them before this version: no accountId
// anywhere, and the decision without its merchant.
type oldRows struct {
	merchant, owner, store, site, appID, decisionID string
}

// seedOldRows writes them owned by the merchant in canonical form, as a real
// actor wrote them.
func seedOldRows(t *testing.T, eng *memql.MemQLEngine, merchant, store, site string) oldRows {
	return seedOldRowsOwnedAs(t, eng, merchant, userRef(merchant), store, site)
}

// seedOldRowsOwnedAs writes them owned by owner, in whatever form it is given.
func seedOldRowsOwnedAs(t *testing.T, eng *memql.MemQLEngine, merchant, owner, store, site string) oldRows {
	t.Helper()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	r := oldRows{merchant: merchant, owner: owner, store: store, site: site, appID: "old-app-" + n, decisionID: "old-dec-" + n}
	insert := func(concept, rowID string, payload map[string]any) {
		raw, _ := json.Marshal(payload)
		if _, err := eng.Execute(seedCtx(), `insert(`+langparser.QuoteString(concept)+`, id=`+
			langparser.QuoteString(rowID)+`, payload=`+string(raw)+`)`); err != nil {
			t.Fatalf("seed old %s: %v", concept, err)
		}
	}
	app := map[string]any{"ownerUserId": r.owner, "storeId": store, "companyName": "Old Trading",
		"applicantName": "Pat Old", "applicantEmail": "pat@old.example.com"}
	if site != "" {
		app["siteId"] = site
	}
	insert("v1:wholesale:application", r.appID, app)
	insert("v1:wholesale:applicationDecision", r.decisionID, map[string]any{"applicationId": r.appID,
		"storeId": store, "transition": "approve", "decidedBy": "user-old-decider-" + n,
		"principalKind": "client", "note": "known to us"})
	insert("v1:wholesale:entitlement", wholesalepack.EntitlementRowID(r.appID), map[string]any{
		"ownerUserId": r.owner, "applicationId": r.appID, "storeId": store, "buyerEmail": "pat@old.example.com",
		"adapter": wholesalepack.AdapterCustomerTag, "state": "granted", "reference": "wholesale"})
	insert("v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(store), map[string]any{
		"ownerUserId": r.owner, "storeId": store, "applicationsOpen": true,
		"entitlementAdapter": wholesalepack.AdapterCustomerTag, "introText": "Apply here."})
	return r
}

// readOld reads the four rows as ctx; the decision is read as an operator,
// because its tier is still its decider's.
func readOld(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, r oldRows) (app, ent, settings map[string]any) {
	t.Helper()
	one := func(q string) map[string]any {
		rows := wsRows(t, eng, ctx, q)
		if len(rows) != 1 {
			t.Fatalf("%s answered %d rows, want 1", q, len(rows))
		}
		return rows[0]
	}
	return one(`query applicationById(applicationId: ` + quote(r.appID) + `)`),
		one(`query entitlementForApplication(applicationId: ` + quote(r.appID) + `)`),
		one(`query wholesaleSettingsForStore(storeId: ` + quote(r.store) + `)`)
}

func decisionOf(t *testing.T, eng *memql.MemQLEngine, r oldRows) map[string]any {
	t.Helper()
	rows := wsRows(t, eng, asOperator("operator-"+r.merchant), `query decisionsForApplication(applicationId: `+quote(r.appID)+`)`)
	if len(rows) != 1 {
		t.Fatalf("an operator reads %d decisions, want 1", len(rows))
	}
	return rows[0]
}

func backfill(t *testing.T, eng *memql.MemQLEngine, r oldRows) map[string]any {
	t.Helper()
	return answerOf(t, eng, asOperator("operator-"+r.merchant),
		`builtin wholesaleBackfillAccounts(storeId: `+quote(r.store)+`)`)
}

func countOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return -1
}

// THE BACKFILL TIES AN OLD APPLICATION, ITS DECISIONS AND ITS ENTITLEMENT, and
// the store's settings, to the organization of the application's site -- and
// changes no field any of them held. Before it runs, the old rows read exactly
// as they did under the tiers that gained an account arm.
func TestTheBackfillTiesAnOldApplicationItsDecisionsAndItsEntitlement(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	account, site := "acct-old-"+n, "site-old-"+n
	seedOrganization(t, eng, account)
	seedSiteIn(t, eng, site, merchant, store, account)
	r := seedOldRows(t, eng, merchant, store, site)

	// PRE-CHANGE ROWS STAY READABLE by their owner and by a cluster owner.
	beforeApp, beforeEnt, beforeSettings := readOld(t, eng, asSiteOwner(merchant), r)
	readOld(t, eng, asOperator("operator-"+merchant), r)
	beforeDecision := decisionOf(t, eng, r)

	report := backfill(t, eng, r)
	if countOf(report["tied"]) != 4 || countOf(report["alreadyTied"]) != 0 {
		t.Fatalf("report = %+v, want tied 4 (application, decision, entitlement, settings) and alreadyTied 0", report)
	}

	app, ent, settings := readOld(t, eng, asSiteOwner(merchant), r)
	decision := decisionOf(t, eng, r)
	for name, row := range map[string]map[string]any{"application": app, "entitlement": ent, "settings": settings, "decision": decision} {
		if bareID(asString(row["accountId"])) != account {
			t.Fatalf("the %s was tied to %v, want %s", name, row["accountId"], account)
		}
	}
	if settings["siteId"] != site {
		t.Fatalf("the settings were tied without their site: %v", settings["siteId"])
	}
	// X5: THE BACKFILL CHANGES NO DECISION FIELD, and gives it its merchant.
	for _, f := range []string{"applicationId", "storeId", "transition", "decidedBy", "principalKind", "note"} {
		if decision[f] != beforeDecision[f] {
			t.Fatalf("the decision's %s changed from %v to %v", f, beforeDecision[f], decision[f])
		}
	}
	if decision["ownerUserId"] != userRef(merchant) {
		t.Fatalf("the decision's ownerUserId = %v, want the application's merchant", decision["ownerUserId"])
	}
	for f, pair := range map[string][2]map[string]any{
		"ownerUserId": {beforeApp, app}, "storeId": {beforeApp, app}, "siteId": {beforeApp, app},
		"companyName": {beforeApp, app}, "applicantName": {beforeApp, app}, "applicantEmail": {beforeApp, app},
	} {
		if pair[0][f] != pair[1][f] {
			t.Fatalf("the application's %s changed from %v to %v", f, pair[0][f], pair[1][f])
		}
	}
	for _, f := range []string{"ownerUserId", "applicationId", "storeId", "buyerEmail", "adapter", "state", "reference"} {
		if beforeEnt[f] != ent[f] {
			t.Fatalf("the entitlement's %s changed from %v to %v", f, beforeEnt[f], ent[f])
		}
	}
	for _, f := range []string{"ownerUserId", "storeId", "applicationsOpen", "entitlementAdapter", "introText"} {
		if beforeSettings[f] != settings[f] {
			t.Fatalf("the settings' %s changed from %v to %v", f, beforeSettings[f], settings[f])
		}
	}
	// AND THE OPERATOR STILL READS THEM.
	readOld(t, eng, asOperator("operator-"+merchant), r)
}

// AN APPLICATION WHOSE SITE CANNOT BE FOUND is left untied and named, with why.
func TestTheBackfillReportsARowWithNoResolvableSite(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	r := seedOldRows(t, eng, merchant, store, "")

	report := backfill(t, eng, r)
	untied, _ := report["untied"].([]any)
	if len(untied) != 1 {
		t.Fatalf("report = %+v, want one untied application", report)
	}
	first, _ := untied[0].(map[string]any)
	if bareID(asString(first["id"])) != r.appID || first["reason"] != "no site serves this store" {
		t.Fatalf("untied[0] = %+v, want %s with reason %q", first, r.appID, "no site serves this store")
	}
	if countOf(report["tied"]) != 0 {
		t.Fatalf("an application with no site tied %v rows", report["tied"])
	}
	if app, _, _ := readOld(t, eng, asSiteOwner(merchant), r); asString(app["accountId"]) != "" {
		t.Fatalf("the untied application was tied to %v", app["accountId"])
	}
}

// RUN TWICE, THE SECOND RUN TIES NOTHING MORE: every row it finds is counted as
// already tied, and none is rewritten.
func TestTheBackfillRunTwiceTiesNothingMore(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	account, site := "acct-twice-"+n, "site-twice-"+n
	seedOrganization(t, eng, account)
	seedSiteIn(t, eng, site, merchant, store, account)
	r := seedOldRows(t, eng, merchant, store, site)

	first := backfill(t, eng, r)
	versions := versionCount(t, "v1:wholesale:application", r.appID)
	second := backfill(t, eng, r)
	if countOf(first["tied"]) < 1 || countOf(second["tied"]) != 0 || countOf(second["alreadyTied"]) != countOf(first["tied"]) {
		t.Fatalf("first run %+v, second run %+v: want the second to tie 0 and find the first run's %v already tied",
			first, second, first["tied"])
	}
	if got := versionCount(t, "v1:wholesale:application", r.appID); got != versions {
		t.Fatalf("the second run rewrote a tied application: %d versions, then %d", versions, got)
	}
}

// ONLY THE CLUSTER'S OPERATORS (or trusted server code) RUN THE BACKFILL. Its
// writes run at internal origin, which the row gate does not owner-check, so it
// refuses everybody else before it reads anything -- the store's own merchant
// included.
func TestTheBackfillRefusesACallerWhoIsNotAClusterOperator(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	account, site := "acct-refuse-"+n, "site-refuse-"+n
	seedOrganization(t, eng, account)
	seedSiteIn(t, eng, site, merchant, store, account)
	r := seedOldRows(t, eng, merchant, store, site)
	versions := versionCount(t, "v1:wholesale:application", r.appID)

	_, err := eng.Execute(asSiteOwner(merchant), `builtin wholesaleBackfillAccounts(storeId: `+quote(store)+`)`)
	if err == nil || !strings.Contains(err.Error(), "cluster's operators") {
		t.Fatalf("the store's merchant ran the backfill, or was refused for another reason: %v", err)
	}
	if got := versionCount(t, "v1:wholesale:application", r.appID); got != versions {
		t.Fatalf("a refused backfill wrote %d versions", got-versions)
	}
	if app, _, _ := readOld(t, eng, asSiteOwner(merchant), r); asString(app["accountId"]) != "" {
		t.Fatalf("a refused backfill tied the application to %v", app["accountId"])
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// signedIn is a person signed in as themselves: the actor the identity
// resolver builds, whose userId is the canonical v1:identity:user:<id>.
func signedIn(userID string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userRef(userID), Role: auth.RoleWriter})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userRef(userID)})
}

// tiedStore seeds an organization and a site of it bound to a fresh store.
func tiedStore(t *testing.T, eng *memql.MemQLEngine, label string) (merchant, store, site, account string) {
	t.Helper()
	merchant, store, _ = wholesaleScope()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	account, site = "acct-"+label+"-"+n, "site-"+label+"-"+n
	seedOrganization(t, eng, account)
	seedSiteIn(t, eng, site, merchant, store, account)
	return merchant, store, site, account
}

// contosoReviewVersions counts the stored versions of the review contoso's
// automation opens for an application.
func contosoReviewVersions(t *testing.T, appID string) int {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	defer db.Close()
	var n int
	if err := db.NewRaw(`SELECT count(*) FROM "MemoryNodes" WHERE concept = 'v1:contoso:contosoReview' AND id LIKE ?`,
		"%"+appID).Scan(context.Background(), &n); err != nil {
		t.Fatalf("count contoso reviews: %v", err)
	}
	return n
}

// A BACKFILL SETS OFF NOTHING. Its writes are new versions of rows a client
// triggers on, so a client automation that is not limited to a row's first
// version would re-provision every approved application and re-open every
// review. The fixture clients carry the first-version filter a client copies,
// and with it the run calls the adapter no more and touches no review.
func TestABackfillSetsOffNoClientAutomation(t *testing.T) {
	eng := automatedWholesaleEngine(t)
	merchant, store, _, _ := tiedStore(t, eng, "quiet")
	ctx := asSiteOwner(merchant)
	openApplications(t, eng, ctx, store, automationFakeAdapter)
	appID := submit(t, eng, ctx, store, "Quiet Trading", "buyer@quiet.example.com")
	decide(t, eng, asDecider(t, eng, merchant), appID, wholesalepack.TransitionApprove, merchant, "approved")

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (len(fakeAdapter.callsFor(appID)) < 1 || contosoReviewVersions(t, appID) < 1) {
		time.Sleep(100 * time.Millisecond)
	}
	if calls := len(fakeAdapter.callsFor(appID)); calls != 1 || contosoReviewVersions(t, appID) != 1 {
		t.Fatalf("before the backfill: %d adapter calls and %d review versions, want 1 and 1",
			calls, contosoReviewVersions(t, appID))
	}

	report := answerOf(t, eng, asOperator("operator-"+merchant), `builtin wholesaleBackfillAccounts(storeId: `+quote(store)+`)`)
	if countOf(report["tied"]) < 3 {
		t.Fatalf("the backfill tied %v rows, want the application, its decision and its entitlement", report["tied"])
	}
	// The automations run on the bus's goroutines; give them every chance.
	time.Sleep(3 * time.Second)
	if calls := fakeAdapter.callsFor(appID); len(calls) != 1 {
		t.Fatalf("the backfill re-provisioned the application: %d adapter calls, want 1", len(calls))
	}
	if n := contosoReviewVersions(t, appID); n != 1 {
		t.Fatalf("the backfill re-opened the client's review: %d versions, want 1", n)
	}
}

// A BACKFILL WRITE IS FENCED ON THE VERSION IT READ. A provision or a settings
// change that lands between the backfill's read and its write survives, and
// the row still ends tied.
func TestABackfillWriteThatLosesItsFenceKeepsTheConcurrentWrite(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, site, account := tiedStore(t, eng, "fence")
	r := seedOldRows(t, eng, merchant, store, site)
	writer := auth.ContextWithInternalOrigin(asSiteOwner(merchant))
	fired := map[string]bool{}
	before := func(_ context.Context, mutation string, _ map[string]any) {
		if fired[mutation] {
			return
		}
		fired[mutation] = true
		switch mutation {
		case "backfillEntitlementRow":
			wsExec(t, eng, writer, `writeEntitlementRow(entitlementId: `+quote(wholesalepack.EntitlementRowID(r.appID))+
				`, ownerUserId: `+quote(userRef(merchant))+`, applicationId: `+quote(r.appID)+`, storeId: `+quote(store)+
				`, buyerEmail: "pat@old.example.com", adapter: "customerTag", state: "granted", reference: "concurrent-ref"`+
				`, error: "", errorCode: "", shopifyCustomerId: "", customerCreated: false, accountId: "")`)
		case "backfillSettingsRow":
			wsExec(t, eng, writer, `setWholesaleSettingsRow(settingsId: `+quote(wholesalepack.SettingsRowID(store))+
				`, ownerUserId: `+quote(userRef(merchant))+`, storeId: `+quote(store)+`, applicationsOpen: true`+
				`, entitlementAdapter: "customerTag", introText: "Concurrent text.", accountId: "", siteId: "")`)
		}
	}
	if _, err := wholesalepack.BackfillForTest(asOperator("operator-"+merchant), eng,
		map[string]any{"storeId": store}, 0, before); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if !fired["backfillEntitlementRow"] || !fired["backfillSettingsRow"] {
		t.Fatalf("the concurrent writes did not run: %v", fired)
	}
	_, ent, settings := readOld(t, eng, asSiteOwner(merchant), r)
	if ent["reference"] != "concurrent-ref" || bareID(asString(ent["accountId"])) != account {
		t.Fatalf("the entitlement = %+v, want the concurrent reference kept and the row tied to %s", ent, account)
	}
	if settings["introText"] != "Concurrent text." || bareID(asString(settings["accountId"])) != account {
		t.Fatalf("the settings = %+v, want the concurrent text kept and the row tied to %s", settings, account)
	}
}

// THE WALK READS PAST ITS FIRST WINDOW: with a window of two, five old
// applications are all tied.
func TestTheBackfillWalksPastItsFirstWindow(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, site, account := tiedStore(t, eng, "walk")
	var apps []string
	for i := 0; i < 5; i++ {
		id := "walk-app-" + strconv.Itoa(i) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		raw, _ := json.Marshal(map[string]any{"ownerUserId": userRef(merchant), "storeId": store, "siteId": site,
			"companyName": "Walk " + strconv.Itoa(i), "applicantName": "Pat", "applicantEmail": "pat@walk.example.com"})
		wsExec(t, eng, seedCtx(), `insert("v1:wholesale:application", id=`+quote(id)+`, payload=`+string(raw)+`)`)
		apps = append(apps, id)
	}
	report, err := wholesalepack.BackfillForTest(asOperator("operator-"+merchant), eng, map[string]any{"storeId": store}, 2, nil)
	if err != nil {
		t.Fatalf("a walk over more than one window failed: %v", err)
	}
	if countOf(report["tied"]) != 5 {
		t.Fatalf("report = %+v, want all five applications tied", report)
	}
	for _, id := range apps {
		rows := wsRows(t, eng, asSiteOwner(merchant), `query applicationById(applicationId: `+quote(id)+`)`)
		if len(rows) != 1 || bareID(asString(rows[0]["accountId"])) != account {
			t.Fatalf("application %s was not tied: %+v", id, rows)
		}
	}
}

// A STORE-SCOPED RUN NAMED BY THE STORE'S CANONICAL ID finds its rows, as a run
// named by the bare id does.
func TestAStoreScopedRunNamedByItsCanonicalIdTiesItsRows(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, site, _ := tiedStore(t, eng, "canon")
	seedOldRows(t, eng, merchant, store, site)
	report := answerOf(t, eng, asOperator("operator-"+merchant),
		`builtin wholesaleBackfillAccounts(storeId: `+quote("v1:shopify:store:"+store)+`)`)
	if countOf(report["tied"]) != 4 {
		t.Fatalf("report = %+v, want the store's four rows tied", report)
	}
}

// AN OPERATOR SETS A STORE UP, AND ITS MERCHANT, SIGNED IN AS THEMSELVES, READS
// THE SETTINGS: the owner taken from the site is stored in the form the read
// predicate compares with a real actor's userId.
func TestAMerchantSignedInReadsTheSettingsAnOperatorSetUp(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _, _ := tiedStore(t, eng, "setup")
	wsExec(t, eng, asOperator("operator-"+merchant), `builtin wholesaleSetSettings(applicationsOpen: true, `+
		`entitlementAdapter: "customerTag", storeId: `+quote(store)+`)`)
	rows := wsRows(t, eng, signedIn(merchant), `query wholesaleSettingsForStore(storeId: `+quote(store)+`)`)
	if len(rows) != 1 || rows[0]["ownerUserId"] != userRef(merchant) {
		t.Fatalf("the merchant, signed in, reads %+v; want their store's settings, owned in canonical form", rows)
	}
}

// AN OLD ROW WHOSE OWNER WAS STORED BARE is stored, once tied, in the form its
// merchant's signed-in actor carries, so the merchant reads it.
func TestABackfillStoresABareOwnerInTheFormItsMerchantReads(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, site, _ := tiedStore(t, eng, "bare")
	r := seedOldRowsOwnedAs(t, eng, merchant, merchant, store, site)
	backfill(t, eng, r)
	app, ent, settings := readOld(t, eng, signedIn(merchant), r)
	for name, row := range map[string]map[string]any{"application": app, "entitlement": ent, "settings": settings} {
		if row["ownerUserId"] != userRef(merchant) {
			t.Fatalf("the %s's owner = %v, want %s", name, row["ownerUserId"], userRef(merchant))
		}
	}
}
