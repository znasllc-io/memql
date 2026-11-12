package reviewspack_test

// live_e2e_test.go -- THE SHOPPER PATH AGAINST A REAL DATABASE (epic
// memql#5532).
//
// reviews' own tests prove the pack LOADS and that the public read's gate,
// exclusion and bound are right as decisions over rows. Three things they
// cannot reach, and all three are assumptions the rest of the epic rests on:
//
//	PROOF 1 -- THE SHOPPER NAMES NO ID AND THE ROW IS WRITTEN AT THE ONE THE
//	  LINK DERIVES. `id` is among the names the shopper surface refuses as a
//	  form field, so a shopper cannot choose where their row lands. The form's
//	  construct is the pack's own Go (reviewsSubmitFromLink): it finds the
//	  link by its token and writes the one review that link's id derives, so
//	  an edit is the next version of the same row.
//
//	PROOF 2 -- A ROW WRITTEN UNDER ONE storeId IS INVISIBLE UNDER ANOTHER,
//	  through the real query and the real row-authz gate rather than through
//	  a fixture. This is design D9, and it is the property that keeps a
//	  review submitted while previewing a storefront off the live one.
//
//	PROOF 3 -- ownerUserId IS THE ACTOR, so the borrowed-authority path
//	  really does make the MERCHANT the owner. If this stamped the shopper
//	  or an empty string the rows would be owned by nobody and every
//	  merchant read would answer zero.
//
// Postgres-gated like every *_db_test.go: skips when no database is
// reachable, and MEMQL_REQUIRE_DB=1 turns that skip into a failure.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	"github.com/znasllc-io/memql/packs/reviewspack"
)

// EVERY IDENTIFIER IS UNIQUE PER RUN, and that is not fastidiousness: the
// db-gated tests do not clean up, so a throwaway database accumulates rows
// across runs and a test asserting an EXACT count against a fixed storeId
// fails on its second run against code that is perfectly correct. Isolating
// by run is what keeps the count assertion meaning what it says.
func runScope() (merchantA, merchantB, storeLive, storeDev, handle string) {
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	return "user-reviews-a-" + n, "user-reviews-b-" + n,
		"store-reviews-live-" + n, "store-reviews-dev-" + n, "boot-" + n
}

// liveDB is the database the newest live engine runs on, handed to the pack's
// plugin context the way the app hands it (app/plugins.go).
var liveDB *bun.DB

func liveEngine(t *testing.T) *memql.MemQLEngine {
	return liveEngineWithDisabled(t)
}

func liveEngineWithDisabled(t *testing.T, disabled ...string) *memql.MemQLEngine {
	t.Helper()
	dsn := dbtest.DSN()
	probe := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := probe.PingContext(context.Background()); err != nil {
		_ = probe.Close()
		dbtest.Unreachable(t, "reviews live e2e", dsn, err)
	}
	_ = probe.Close()

	_ = os.Setenv("MEMQL_DATABASE_DSN", dsn)
	mnd, err := memoryNodes.NewMemoryNodesDatabase()
	if err != nil {
		t.Fatalf("NewMemoryNodesDatabase: %v", err)
	}
	mnd.Start(context.Background())
	select {
	case <-mnd.Ready():
	case <-time.After(60 * time.Second):
		t.Fatal("database did not become ready within 60s")
	}
	bunDB := mnd.BunDB()
	if bunDB == nil {
		t.Fatal("nil bun.DB after migrate")
	}
	liveDB = bunDB

	const domain = reviewspack.Domain
	memqldsl.RegisterTree(domain, reviewspack.Tree())
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	// The boot-time projection app phase 3 hands the DSL layer, here as a
	// test seam: this is the ONE input that makes a pack mounted-inert.
	memqldsl.SetDisabledPackDomains(disabled)
	t.Cleanup(func() { memqldsl.SetDisabledPackDomains(nil) })

	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(bunDB)
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	return eng
}

// asMerchant is the borrowed authority the bff applies: the SITE OWNER'S
// actor, exactly as auth.ContextWithUserActor builds it on the real path.
func asMerchant(userID string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userID)
}

func TestLiveE2E_TheShopperPathWritesAndScopesByStore(t *testing.T) {
	eng := livePackEngine(t)
	declareShopperSurface(t)
	merchantA, merchantB, storeLive, storeDev, handle := runScope()

	// ---- PROOF 1: a buyer's post through the real form route --------------
	//
	// One merchant, two storefronts: the live store's and the development
	// store's a candidate is previewed against. Each post goes through the
	// handler exactly as the edge forwards it -- the site, the store it is
	// bound to and the site's owner -- and names no id.
	seedOwnerUser(t, eng, merchantA)
	siteLive, siteDev := "site-"+storeLive, "site-"+storeDev
	seedSite(t, eng, siteLive, merchantA, storeLive)
	seedSite(t, eng, siteDev, merchantA, storeDev)
	owner := siteOwnerOf(t, siteLive)
	liveReview := reviewThroughLink(t, eng, siteLive, storeLive, owner, "request-live-"+handle, handle, "Live store review")
	reviewThroughLink(t, eng, siteDev, storeDev, owner, "request-dev-"+handle, handle, "Written while previewing")

	// ---- PROOF 2: storeId scopes the read ---------------------------------
	live := rowsOf(t, eng, asMerchant(owner),
		`query reviewsForProduct(productHandle: `+quote(handle)+`, storeId: `+quote(storeLive)+`)`)
	if len(live) != 1 {
		t.Fatalf("the live store returned %d reviews, want exactly the one written against it "+
			"-- a review written while previewing must not reach shoppers", len(live))
	}
	if got, _ := live[0]["body"].(string); got != "Live store review" {
		t.Fatalf("the live store returned %q", got)
	}

	dev := rowsOf(t, eng, asMerchant(owner),
		`query reviewsForProduct(productHandle: `+quote(handle)+`, storeId: `+quote(storeDev)+`)`)
	if len(dev) != 1 {
		t.Fatalf("the development store returned %d reviews, want 1 -- the row must still be "+
			"readable where it was written", len(dev))
	}

	// ---- PROOF 1 (continued): the row IS the link's -----------------------
	if id, _ := live[0]["id"].(string); id != liveReview {
		t.Fatalf("the review landed at id %q, want the one its link derives, %q", id, liveReview)
	}

	// ---- PROOF 4: THE PUBLIC PROJECTION OMITS THE SHOPPER'S EMAIL --------
	//
	// Through the real query and the real shape, not through a Go struct's
	// omission: publishedReviewsForProduct projects reviewPublic, so the
	// address never reaches the read whose output is served to an
	// unauthenticated browser.
	pub := rowsOf(t, eng, asMerchant(owner),
		`query publishedReviewsForProduct(productHandle: `+quote(handle)+`, storeId: `+quote(storeLive)+`)`)
	if len(pub) != 1 {
		t.Fatalf("the public projection returned %d rows, want 1", len(pub))
	}
	if _, present := pub[0]["authorEmail"]; present {
		t.Fatalf("the public projection carries authorEmail: %v", pub[0])
	}
	if got, _ := pub[0]["authorName"].(string); got != "Dana" {
		t.Fatalf("the public projection lost the author's display name: %v", pub[0])
	}
	// ...while the MERCHANT's projection keeps it, which is what makes the
	// omission above a choice rather than a field nothing writes.
	if _, present := live[0]["authorEmail"]; !present {
		t.Fatal("the merchant's projection must carry authorEmail so they can reply -- " +
			"without this the test above passes for the wrong reason")
	}

	// ---- PROOF 3: the MERCHANT owns it, not the shopper -------------------
	if got, _ := live[0]["ownerUserId"].(string); got != owner {
		t.Fatalf("ownerUserId = %q, want the site owner %q. A shopper has no identity, so a "+
			"row owned by anyone else -- or by nobody -- is unreadable by the merchant whose "+
			"storefront it was submitted through", got, owner)
	}
	if name, _ := live[0]["authorName"].(string); name != "Dana" {
		t.Fatalf("authorName = %q; the shopper is DATA on the row", name)
	}

	// ---- AND THE OWNER TIER ACTUALLY BITES --------------------------------
	//
	// A second merchant reads zero, which is what makes the borrowed-owner
	// stamp on the wire self-checking: naming somebody else's site buys
	// nothing, because the row read under that actor comes back empty.
	other := rowsOf(t, eng, asMerchant(merchantB),
		`query reviewsForProduct(productHandle: `+quote(handle)+`, storeId: `+quote(storeLive)+`)`)
	if len(other) != 0 {
		t.Fatalf("a different merchant read %d of merchant A's reviews, want 0", len(other))
	}
}

func rowsOf(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, query string) []map[string]any {
	t.Helper()
	result, err := eng.Execute(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return memql.MaterializeRows(result)
}

// quote renders a MemQL string literal. The engine's own escaping, never Go's.
func quote(s string) string {
	return `"` + s + `"`
}

// A DISABLED PACK IS MOUNTED-INERT, and this is the property that makes
// "storefront packs ship disabled" SAFE rather than merely quiet (epic
// memql#5532, issue memql#5549). The record names it as acceptance for this
// epic, and the generic predicate test in dsl/ covers the path rule rather
// than the outcome -- so this asserts the outcome, for the pack that ships.
//
// CONCEPTS STAY, because schemas are declarative and inert: cross-domain
// imports and @relationship targets keep resolving, and rows written before
// the flip stay browsable. BEHAVIOUR GOES, so the shopper surface a disabled
// pack declared reaches a construct that is not in the registry at all --
// absent rather than present and refusing.
func TestLiveE2E_ADisabledPackIsInert(t *testing.T) {
	eng := liveEngineWithDisabled(t, reviewspack.Domain)
	declareShopperSurface(t)
	merchant, _, storeLive, _, handle := runScope()
	site := "site-" + storeLive
	seedOwnerUser(t, eng, merchant)
	seedSite(t, eng, site, merchant, storeLive)
	owner := siteOwnerOf(t, site)
	// A live link, written as the pack would write one: the concept stays,
	// so a seed at internal origin can still write it.
	token := "tok-inert-" + handle + "-0123456789abcdefghijklmnopqrstuvwxyz"
	now := time.Now().UTC()
	if _, err := eng.Execute(seedCtx(), rawReviewsInsert("v1:reviews:reviewRequest", "request-inert-"+handle, map[string]any{
		"ownerUserId": owner, "storeId": storeLive, "orderGid": "gid://shopify/Order/1", "productHandle": handle,
		"email": "buyer@example.com", "tokenHash": reviewspack.TokenHashForTest(token),
		"issuedAt": now.Format(time.RFC3339Nano), "expiresAt": now.Add(time.Hour).Format(time.RFC3339Nano)})); err != nil {
		t.Fatalf("seed the link: %v", err)
	}

	// The BEHAVIOURAL half is gone: the form's construct is not loaded, so
	// the write cannot happen at all. A COMPLETE call, so the refusal is "the
	// construct is not loaded" and not "an argument is missing" -- an inert
	// pack has to be what stops this, and a call the loader would reject
	// anyway proves nothing.
	if _, err := eng.Execute(asMerchant(owner), `builtin reviewsSubmitFromLink(requestToken: `+quote(token)+
		`, productHandle: `+quote(handle)+`, body: "x", storeId: `+quote(storeLive)+`)`); err == nil {
		t.Fatal("reviewsSubmitFromLink ran on a DISABLED pack: a mounted-inert pack must load no " +
			"behavioural construct, so the shopper write path has nothing to reach")
	}
	// ...and the form route, posted through the real handler, writes nothing.
	rec := postReviewForm(t, eng, site, storeLive, owner, reviewForm(token, handle, "x"))
	if loc := rec.Header().Get("Location"); strings.HasPrefix(loc, "/reviews/thank-you") {
		t.Fatalf("a DISABLED pack's form answered %q", loc)
	}
	if n := versionsOf(t, "v1:reviews:review", reviewspack.ReviewIDForRequestForTest("request-inert-"+handle)); n != 0 {
		t.Fatalf("a DISABLED pack's form wrote %d review versions", n)
	}

	// ...and so is the read behind the declared shopper read.
	if _, err := eng.Execute(asMerchant(owner),
		`query reviewsForStore(storeId: `+quote(storeLive)+`)`); err == nil {
		t.Fatal("a query on a DISABLED pack still resolved")
	}

	// The CONCEPT half stays, which is what keeps rows written before the
	// flip browsable and cross-domain imports resolving.
	if _, err := memoryNodes.DefaultRegistry().Get("v1:reviews:review"); err != nil {
		t.Fatalf("a disabled pack's concepts must still load -- mounted-inert, not "+
			"unmounted: %v", err)
	}
}
