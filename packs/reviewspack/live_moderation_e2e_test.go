package reviewspack_test

// live_moderation_e2e_test.go -- THE PENDING STATE AGAINST A REAL DATABASE.
//
// The unit tests prove the lifecycle as a function over rows. This proves the
// three things they cannot:
//
//	PROOF 1 -- A STORED REVIEW IS NOT PUBLIC, even with the store's display on,
//	  through the real builtin, the real queries and the real row gate.
//	PROOF 2 -- A DECISION WRITTEN BY THE STORE'S PERSON IS READ BY THE PUBLIC
//	  READ, which runs under the site owner's borrowed authority. The legacy
//	  moderationAction was owned by its author, so a decision by anyone but the
//	  site owner hid nothing; a reviewDecision is owned by the review's merchant.
//	PROOF 3 -- THE CLUSTER'S OPERATORS CANNOT DECIDE, through the real engine.
//
// Postgres-gated: skips when no database is reachable; MEMQL_REQUIRE_DB=1
// turns that skip into a failure.

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/packs/reviewspack"
)

// livePackEngine is liveEngine with the pack's Go half materialized, which
// the app does at boot (app/plugins.go, materializePlugins) for every pack
// app/anchor_storefront_packs.go links, and a test binary must do for itself.
func livePackEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	eng := liveEngine(t)
	// GRANTS ARE READ ONLY THROUGH AN INSTALLED SOURCE, which an engine that
	// never Starts must install itself (the engine's own db tests do the same).
	eng.InstallGrantResolution()
	t.Cleanup(func() { auth.SetGrantSource(nil); auth.SetMembershipSource(nil) })
	prov, err := reviewspack.NewProvider(memql.PluginContext{Engine: eng, Logger: eng.Logger,
		BunDB: func() *bun.DB { return liveDB }})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if err := eng.RegisterIntegration(prov); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	return eng
}

func asPerson(userID string, role auth.Role) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userID, Role: role})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userID})
}

// payloadOf reads the single node a builtin answers with.
func payloadOf(t *testing.T, eng *memql.MemQLEngine, ctx context.Context, call string) map[string]any {
	t.Helper()
	result, err := eng.Execute(ctx, call)
	if err != nil {
		t.Fatalf("%s: %v", call, err)
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 {
		raw, _ := json.Marshal(rows)
		t.Fatalf("%s answered %d rows, want 1: %s", call, len(rows), raw)
	}
	return rows[0]
}

func TestLiveE2E_AReviewIsShownOnlyOnceAStorePersonPublishesIt(t *testing.T) {
	eng := livePackEngine(t)
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	merchant := "user-reviews-shop-" + n
	staff := "user-reviews-staff-" + n
	stranger := "user-reviews-nopart-" + n
	store := "store-reviews-pub-" + n
	site := "site-reviews-pub-" + n
	handle := "boot-" + n
	declareShopperSurface(t)
	seedOwnerUser(t, eng, merchant)
	seedSite(t, eng, site, merchant, store)
	grantReviewsPart(t, eng, merchant)

	// From here the merchant is who a deployed cluster says they are: the
	// canonical id their token carries, which is also the site's stored owner.
	merchant = siteOwnerOf(t, site)

	// A buyer's review, through their link and the real form route.
	reviewThroughLink(t, eng, site, store, merchant, "request-pub-"+n, handle, "Great boots")

	// The store's settings, set up by an operator who is also the site owner
	// (the production shape: the deployable's owner is a cluster operator).
	if _, err := eng.Execute(asPerson(merchant, auth.RoleOwner),
		`builtin reviewsUpdateSettings(storeId: `+quote(store)+`, siteId: `+quote(site)+`, publicDisplay: true)`); err != nil {
		t.Fatalf("reviewsUpdateSettings: %v", err)
	}

	read := func() map[string]any {
		return payloadOf(t, eng, asMerchant(merchant),
			`builtin reviewsPublishedForProduct(storeId: `+quote(store)+`, productHandle: `+quote(handle)+`)`)
	}

	// ---- PROOF 1 ------------------------------------------------------------
	if got := read()["count"]; got != float64(0) {
		t.Fatalf("a review nobody published is public (count %v) with the store's display on", got)
	}
	queue := payloadOf(t, eng, asPerson(merchant, auth.Role("user")),
		`builtin reviewsList(storeId: `+quote(store)+`, siteId: `+quote(site)+`)`)
	if queue["total"] != float64(1) {
		t.Fatalf("the queue holds %v reviews, want the one waiting", queue["total"])
	}
	queued := queue["reviews"].([]any)[0].(map[string]any)
	reviewID := queued["id"].(string)
	contentHash, _ := queued["contentHash"].(string)
	if contentHash == "" {
		t.Fatal("the queue carries no fingerprint of the words it lists")
	}
	if _, err := time.Parse(time.RFC3339Nano, asText(queued["createdAt"])); err != nil {
		t.Fatalf("the queue's createdAt %v is not ISO text: %v", queued["createdAt"], err)
	}

	// ---- PROOF 3 ------------------------------------------------------------
	if _, err := eng.Execute(asPerson(staff, auth.RoleOwner),
		`builtin reviewsDecide(reviewId: `+quote(reviewID)+`, action: "publish", revision: 1)`); err == nil {
		t.Fatal("a cluster operator recorded a decision on a store's review")
	}
	// And a person without the reviews part cannot decide either.
	if _, err := eng.Execute(asPerson(stranger, auth.Role("user")),
		`builtin reviewsDecide(reviewId: `+quote(reviewID)+`, action: "publish", revision: 1)`); err == nil {
		t.Fatal("a person without the reviews part recorded a decision")
	}

	// ---- PROOF 2 ------------------------------------------------------------
	if _, err := eng.Execute(asPerson(merchant, auth.Role("user")),
		`builtin reviewsDecide(reviewId: `+quote(reviewID)+`, action: "publish", revision: 1, contentHash: "0000")`); err == nil {
		t.Fatal("a publish naming words the review does not hold was recorded")
	}
	if _, err := eng.Execute(asPerson(merchant, auth.Role("user")),
		`builtin reviewsDecide(reviewId: `+quote(reviewID)+`, action: "publish", revision: 1, contentHash: `+quote(contentHash)+`)`); err != nil {
		t.Fatalf("the store's person could not publish: %v", err)
	}
	shown := read()
	if shown["count"] != float64(1) {
		t.Fatalf("after a publish the public read shows %v reviews, want 1", shown["count"])
	}
	first := shown["reviews"].([]any)[0].(map[string]any)
	if _, err := time.Parse(time.RFC3339Nano, asText(first["createdAt"])); err != nil {
		t.Fatalf("the public read's createdAt %v is not ISO text a browser parses: %v", first["createdAt"], err)
	}
	summary, _ := shown["summary"].(map[string]any)
	if summary == nil || summary["average"] != float64(4) || summary["reviewCount"] != float64(1) {
		t.Fatalf("summary = %v, want average 4 over 1 review", shown["summary"])
	}

	log := rowsOf(t, eng, asPerson(merchant, auth.Role("user")),
		`query reviewDecisionsForReview(reviewId: `+quote(reviewID)+`)`)
	if len(log) != 1 || log[0]["action"] != "publish" || log[0]["decidedBy"] != merchant ||
		log[0]["ownerUserId"] != merchant {
		t.Fatalf("decision log = %v, want one publish owned by the merchant", log)
	}
}

// THE WRITE MUTATIONS CANNOT BE CALLED AROUND THE BUILTINS THAT JUDGE THEM.
// A signed-in client -- here the store's own owner, the person with the most
// right to the rows -- calls the settings and the decision writer directly. None
// may write: settings change only through reviewsUpdateSettings and
// reviewsSetPublicDisplay (which merge the row and judge who may change
// what), and a decision is recorded only through reviewsDecide.
func TestLiveE2E_NoClientWritesSettingsOrDecisionsDirectly(t *testing.T) {
	eng := livePackEngine(t)
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	merchant := "user-reviews-direct-" + n
	store := "store-reviews-direct-" + n
	client := asPerson(merchant, auth.Role("user"))

	_, _ = eng.Execute(client, `putReviewSettings(settingsId: `+quote("settings-direct2-"+n)+
		`, ownerUserId: `+quote(merchant)+`, storeId: `+quote(store)+`, publicDisplay: true)`)
	if rows := rowsOf(t, eng, client, `query reviewSettingsForStore(storeId: `+quote(store)+`)`); len(rows) != 0 {
		t.Fatalf("a client wrote %d settings rows directly; settings change only through the merging builtins: %v", len(rows), rows)
	}

	_, _ = eng.Execute(client, `appendReviewDecision(decisionId: `+quote("decision-direct-"+n)+
		`, ownerUserId: `+quote(merchant)+`, reviewId: "r-direct", storeId: `+quote(store)+
		`, action: "publish", revision: 1, decidedBy: `+quote(merchant)+`)`)
	if rows := rowsOf(t, eng, client, `query reviewDecisionsForReview(reviewId: "r-direct")`); len(rows) != 0 {
		t.Fatalf("a client recorded a decision directly, around reviewsDecide: %v", rows)
	}
}

func asText(v any) string {
	s, _ := v.(string)
	return s
}
