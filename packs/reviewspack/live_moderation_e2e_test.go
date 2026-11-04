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
	prov, err := reviewspack.NewProvider(memql.PluginContext{Engine: eng, Logger: eng.Logger})
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
	store := "store-reviews-pub-" + n
	handle := "boot-" + n

	// A shopper's review, exactly as the bff writes one.
	call := `submitReview(body: "Great boots", productHandle: ` + quote(handle) +
		`, rating: 5, siteId: "site-reviews-e2e", storeId: ` + quote(store) +
		`, submissionId: ` + quote("sub-pub-"+n) + `)`
	if _, err := eng.Execute(asMerchant(merchant), call); err != nil {
		t.Fatalf("submitReview: %v", err)
	}

	// The store's settings, set up by an operator who is also the site owner
	// (the production shape: the deployable's owner is a cluster operator).
	if _, err := eng.Execute(asPerson(merchant, auth.RoleOwner),
		`builtin reviewsUpdateSettings(storeId: `+quote(store)+`, publicDisplay: true)`); err != nil {
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
		`builtin reviewsList(storeId: `+quote(store)+`)`)
	if queue["total"] != float64(1) {
		t.Fatalf("the queue holds %v reviews, want the one waiting", queue["total"])
	}
	reviewID := queue["reviews"].([]any)[0].(map[string]any)["id"].(string)

	// ---- PROOF 3 ------------------------------------------------------------
	if _, err := eng.Execute(asPerson(staff, auth.RoleOwner),
		`builtin reviewsDecide(reviewId: `+quote(reviewID)+`, action: "publish", revision: 1)`); err == nil {
		t.Fatal("a cluster operator recorded a decision on a store's review")
	}

	// ---- PROOF 2 ------------------------------------------------------------
	if _, err := eng.Execute(asPerson(merchant, auth.Role("user")),
		`builtin reviewsDecide(reviewId: `+quote(reviewID)+`, action: "publish", revision: 1)`); err != nil {
		t.Fatalf("the store's person could not publish: %v", err)
	}
	shown := read()
	if shown["count"] != float64(1) {
		t.Fatalf("after a publish the public read shows %v reviews, want 1", shown["count"])
	}
	summary, _ := shown["summary"].(map[string]any)
	if summary == nil || summary["average"] != float64(5) || summary["reviewCount"] != float64(1) {
		t.Fatalf("summary = %v, want average 5 over 1 review", shown["summary"])
	}

	log := rowsOf(t, eng, asPerson(merchant, auth.Role("user")),
		`query reviewDecisionsForReview(reviewId: `+quote(reviewID)+`)`)
	if len(log) != 1 || log[0]["action"] != "publish" || log[0]["decidedBy"] != merchant ||
		log[0]["ownerUserId"] != merchant {
		t.Fatalf("decision log = %v, want one publish owned by the merchant", log)
	}
}
