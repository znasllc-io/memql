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

// A SITE WITH NO ORGANIZATION IS A LEGITIMATE STATE: the application is written
// with none and stays its owner's, as before.
func TestASubmissionOnASiteWithNoOrganizationIsWrittenWithNone(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, store, _ := wholesaleScope()
	site := "site-none-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seedUser(t, eng, merchant)
	seedSiteIn(t, eng, site, merchant, store, "")
	owner := siteOwnerOf(t, eng, site)
	openApplications(t, eng, asSiteOwner(owner), store, wholesalepack.AdapterCustomerTag)

	rec := postApplication(t, eng, site, store, owner, url.Values{
		"companyName": {"Untied Trading"}, "applicantName": {"Sam Rivers"}, "applicantEmail": {"sam@none.example.com"},
	})
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/wholesale/thank-you") {
		t.Fatalf("the post was refused: %d %q", rec.Code, loc)
	}
	apps := wsRows(t, eng, asSiteOwner(owner), `query applicationsForStore(storeId: `+quote(store)+`)`)
	if len(apps) != 1 || asString(apps[0]["accountId"]) != "" {
		t.Fatalf("the merchant reads %+v, want one application with no accountId", apps)
	}
}

// oldRows writes an application, a decision, an entitlement and the store's
// settings exactly as this pack wrote them before this version: no accountId
// anywhere, and the decision without its merchant.
type oldRows struct {
	merchant, store, site, appID, decisionID string
}

func seedOldRows(t *testing.T, eng *memql.MemQLEngine, merchant, store, site string) oldRows {
	t.Helper()
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	r := oldRows{merchant: merchant, store: store, site: site, appID: "old-app-" + n, decisionID: "old-dec-" + n}
	insert := func(concept, rowID string, payload map[string]any) {
		raw, _ := json.Marshal(payload)
		if _, err := eng.Execute(seedCtx(), `insert(`+langparser.QuoteString(concept)+`, id=`+
			langparser.QuoteString(rowID)+`, payload=`+string(raw)+`)`); err != nil {
			t.Fatalf("seed old %s: %v", concept, err)
		}
	}
	app := map[string]any{"ownerUserId": merchant, "storeId": store, "companyName": "Old Trading",
		"applicantName": "Pat Old", "applicantEmail": "pat@old.example.com"}
	if site != "" {
		app["siteId"] = site
	}
	insert("v1:wholesale:application", r.appID, app)
	insert("v1:wholesale:applicationDecision", r.decisionID, map[string]any{"applicationId": r.appID,
		"storeId": store, "transition": "approve", "decidedBy": "user-old-decider-" + n,
		"principalKind": "client", "note": "known to us"})
	insert("v1:wholesale:entitlement", wholesalepack.EntitlementRowID(r.appID), map[string]any{
		"ownerUserId": merchant, "applicationId": r.appID, "storeId": store, "buyerEmail": "pat@old.example.com",
		"adapter": wholesalepack.AdapterCustomerTag, "state": "granted", "reference": "wholesale"})
	insert("v1:wholesale:wholesaleSettings", wholesalepack.SettingsRowID(store), map[string]any{
		"ownerUserId": merchant, "storeId": store, "applicationsOpen": true,
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
	if decision["ownerUserId"] != merchant {
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
