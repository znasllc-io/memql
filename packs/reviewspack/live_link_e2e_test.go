package reviewspack_test

// live_link_e2e_test.go -- A BUYER'S LINK AGAINST A REAL DATABASE.
//
// The unit tests prove the link's rules over fixture rows. This proves what
// they cannot: the request is found by its token's hash through the real
// query and row gate under the site owner's borrowed authority; the review
// lands as a VERIFIED row and an edit is the next revision of the SAME row;
// a publish closes the link; and the link's named refusals survive the
// engine (errors.As on what Execute returns), which is what lets the bff tell
// a shopper "this link has expired" instead of "something went wrong".
//
// Postgres-gated: skips when no database is reachable; MEMQL_REQUIRE_DB=1
// turns that skip into a failure.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/packs/reviewspack"
)

// putRequest writes a request row the way the pack does: its @serverOnly
// mutation, internal origin for the one call, AS THE SITE OWNER.
func putRequest(t *testing.T, eng *memql.MemQLEngine, merchant, store, requestID, handle, token string, expires time.Time) {
	t.Helper()
	call := `putReviewRequest(requestId: ` + quote(requestID) + `, storeId: ` + quote(store) +
		`, orderGid: "gid://shopify/Order/1", productHandle: ` + quote(handle) +
		`, email: "buyer@example.com", tokenHash: ` + quote(reviewspack.TokenHashForTest(token)) +
		`, issuedAt: ` + quote(time.Now().UTC().Format(time.RFC3339Nano)) +
		`, expiresAt: ` + quote(expires.UTC().Format(time.RFC3339Nano)) + `)`
	if _, err := eng.Execute(auth.ContextWithInternalOrigin(asMerchant(merchant)), call); err != nil {
		t.Fatalf("putReviewRequest: %v", err)
	}
}

func submitThrough(eng *memql.MemQLEngine, merchant, store, handle, token, body string) error {
	_, err := eng.Execute(asMerchant(merchant), `builtin reviewsSubmitFromLink(requestToken: `+quote(token)+
		`, productHandle: `+quote(handle)+`, body: `+quote(body)+`, rating: 4, storeId: `+quote(store)+`)`)
	return err
}

func refusedAs(t *testing.T, err error, code string) {
	t.Helper()
	var refusal *memql.ShopperRefusal
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("err = %v, want the named refusal %q to survive the engine", err, code)
	}
}

func TestLiveE2E_ABuyersLinkWritesAVerifiedReviewAndClosesOnPublish(t *testing.T) {
	eng := livePackEngine(t)
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	merchant := "user-reviews-link-" + n
	store := "store-reviews-link-" + n
	handle := "floss-picks-" + n
	token := "tok-" + n + "-0123456789abcdefghijklmnopqrstuvwxyz"
	site := "site-reviews-link-" + n
	account := "acct-reviews-link-" + n
	seedSiteIn(t, eng, site, merchant, store, account)
	grantReviewsPart(t, eng, merchant)
	// The removal approver is one of the store's organization's people.
	seedStorePerson(t, eng, account, merchant)
	putRequest(t, eng, merchant, store, "request-"+n, handle, token, time.Now().Add(14*24*time.Hour))

	if err := submitThrough(eng, merchant, store, handle, token, "first words"); err != nil {
		t.Fatalf("a valid link was refused: %v", err)
	}
	draft := payloadOf(t, eng, asMerchant(merchant),
		`builtin reviewsDraftForLink(requestToken: `+quote(token)+`, storeId: `+quote(store)+`)`)
	if draft["body"] != "first words" || draft["state"] != "waiting" {
		t.Fatalf("draft = %v, want the buyer's words, waiting", draft)
	}
	if err := submitThrough(eng, merchant, store, handle, token, "second thoughts"); err != nil {
		t.Fatalf("an edit through an open link was refused: %v", err)
	}
	rows := rowsOf(t, eng, asMerchant(merchant), `query reviewsInStore(storeId: `+quote(store)+`)`)
	if len(rows) != 1 {
		t.Fatalf("the store holds %d reviews, want one row that the edit revised", len(rows))
	}
	if rows[0]["revision"] != float64(2) || rows[0]["verifiedBuyer"] != true || rows[0]["body"] != "second thoughts" {
		t.Fatalf("review = %v, want revision 2, verified, with the edited words", rows[0])
	}

	// Published, the link closes: the next submit is link_used.
	if _, err := eng.Execute(asPerson(merchant, auth.RoleOwner),
		`builtin reviewsUpdateSettings(storeId: `+quote(store)+`, siteId: `+quote(site)+`, publicDisplay: true)`); err != nil {
		t.Fatalf("settings: %v", err)
	}
	reviewID := rows[0]["id"].(string)
	if _, err := eng.Execute(asPerson(merchant, auth.RoleWriter),
		`builtin reviewsDecide(reviewId: `+quote(reviewID)+`, action: "publish", revision: 2)`); err != nil {
		t.Fatalf("publish: %v", err)
	}
	shown := payloadOf(t, eng, asMerchant(merchant),
		`builtin reviewsPublishedForProduct(storeId: `+quote(store)+`, productHandle: `+quote(handle)+`)`)
	list, _ := shown["reviews"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["verifiedBuyer"] != true {
		t.Fatalf("published = %v, want the one review, marked verified", shown)
	}
	refusedAs(t, submitThrough(eng, merchant, store, handle, token, "too late"), "link_used")

	// ---- the store's record leaves with it ---------------------------------
	export := payloadOf(t, eng, asPerson(merchant, auth.RoleWriter),
		`builtin reviewsExportStore(storeId: `+quote(store)+`, siteId: `+quote(site)+`)`)
	var doc map[string]any
	if err := json.Unmarshal([]byte(export["json"].(string)), &doc); err != nil {
		t.Fatalf("the export is not JSON: %v", err)
	}
	if len(doc["decisions"].([]any)) != 1 || doc["reviews"].([]any)[0].(map[string]any)["state"] != "published" {
		t.Fatalf("export = %v, want the review published and its decision", doc)
	}

	// ---- erasure on request ---------------------------------------------------
	if _, err := eng.Execute(asPerson(merchant, auth.RoleOwner),
		`builtin reviewsUpdateSettings(storeId: `+quote(store)+`, siteId: `+quote(site)+`, removalApproverUserId: `+quote(merchant)+`)`); err != nil {
		t.Fatalf("naming the removal approver: %v", err)
	}
	if _, err := eng.Execute(asPerson(merchant, auth.RoleWriter),
		`builtin reviewsEraseReviewer(reviewId: `+quote(reviewID)+`, note: "asked by email")`); err != nil {
		t.Fatalf("the named owner could not erase: %v", err)
	}
	after := payloadOf(t, eng, asMerchant(merchant),
		`builtin reviewsPublishedForProduct(storeId: `+quote(store)+`, productHandle: `+quote(handle)+`)`)
	if after["count"] != float64(0) {
		t.Fatalf("an erased review is still public (count %v)", after["count"])
	}
	// EVERY VERSION, read off the table itself: the history is where a
	// point-in-time read would still find the words.
	versionRows, err := liveDB.DB.QueryContext(context.Background(),
		`SELECT coalesce(payload->>'body',''), coalesce(payload->>'authorName',''), coalesce(payload->>'erasedAt','') FROM "MemoryNodes" WHERE id = $1`,
		"v1:reviews:review:"+strings.TrimPrefix(reviewID, "v1:reviews:review:"))
	if err != nil {
		t.Fatal(err)
	}
	versions := 0
	for versionRows.Next() {
		var body, name, erased string
		if err := versionRows.Scan(&body, &name, &erased); err != nil {
			t.Fatal(err)
		}
		versions++
		if body != "" || name != "" || erased == "" {
			t.Fatalf("version %d still holds the writer's words (body %q, name %q, erasedAt %q)", versions, body, name, erased)
		}
	}
	_ = versionRows.Close()
	if versions < 2 {
		t.Fatalf("found %d versions of the review, want both revisions rewritten", versions)
	}
	var email string
	if err := liveDB.DB.QueryRowContext(context.Background(),
		`SELECT coalesce(payload->>'email','') FROM "MemoryNodes" WHERE id = $1 ORDER BY "createdAt" DESC LIMIT 1`,
		"v1:reviews:reviewRequest:request-"+n).Scan(&email); err != nil || email != "" {
		t.Fatalf("the link's email = %q (%v), want it erased", email, err)
	}

	// An expired link and a forged token refuse by name too.
	expiredToken := token + "-expired"
	putRequest(t, eng, merchant, store, "request-exp-"+n, handle+"-x", expiredToken, time.Now().Add(-time.Hour))
	refusedAs(t, submitThrough(eng, merchant, store, handle+"-x", expiredToken, "late"), "link_expired")
	refusedAs(t, submitThrough(eng, merchant, store, handle, "forged-"+n, "nope"), "link_invalid")
}

// THE ORDER-LINE QUERY RESOLVES ACROSS DOMAINS: the reviews pack reads the
// mirrored order's lines to decide which products earn a link.
func TestLiveE2E_TheOrderLineQueryLoads(t *testing.T) {
	eng := livePackEngine(t)
	ctx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: "user-reviews-op", Role: auth.RoleOwner}), &auth.TokenInfo{Subject: "user-reviews-op"})
	if _, err := eng.Execute(ctx, `query reviewOrderLines(storeId: "store-none", orderGid: "gid://shopify/Order/0")`); err != nil {
		t.Fatalf("reviewOrderLines did not resolve: %v", err)
	}
}

// THE MODERATOR IS TOLD, BY THE STORE'S AUTOMATION ONLY: one outbound email per
// review revision, staged through the transactional outbox.
func TestLiveE2E_AWaitingReviewStagesOneEmailForTheModerator(t *testing.T) {
	eng := livePackEngine(t)
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	merchant := "user-reviews-notify-" + n
	store := "store-reviews-notify-" + n
	site := "site-reviews-notify-" + n
	handle := "floss-picks-" + n
	token := "tok-notify-" + n + "-0123456789abcdefghijklmnopqrstuvwxyz"
	seedSite(t, eng, site, merchant, store)
	if _, err := eng.Execute(asPerson(merchant, auth.RoleOwner),
		`builtin reviewsUpdateSettings(storeId: `+quote(store)+`, siteId: `+quote(site)+`, notifyEmail: "moderator@example.com")`); err != nil {
		t.Fatalf("settings: %v", err)
	}
	putRequest(t, eng, merchant, store, "request-notify-"+n, handle, token, time.Now().Add(time.Hour))
	if err := submitThrough(eng, merchant, store, handle, token, "a waiting review"); err != nil {
		t.Fatal(err)
	}
	reviewID := "review-notify-" + n
	if _, err := eng.Execute(asMerchant(merchant), `builtin reviewsNotifyModerator(reviewId: `+quote(reviewID)+`)`); err == nil {
		t.Fatal("a client call staged a moderator email")
	}
	staged := payloadOf(t, eng, auth.ContextWithInternalOrigin(asMerchant(merchant)),
		`builtin reviewsNotifyModerator(reviewId: `+quote(reviewID)+`)`)
	if staged["status"] != "staged" {
		t.Fatalf("notice = %v, want an email staged", staged)
	}
	var target, subject string
	if err := liveDB.DB.QueryRowContext(context.Background(),
		`SELECT coalesce(payload->>'target',''), coalesce(payload->>'subject','') FROM "MemoryNodes" WHERE concept = 'v1:platform:outboundRequest' AND id LIKE $1 ORDER BY "createdAt" DESC LIMIT 1`,
		"%"+staged["requestId"].(string)+"%").Scan(&target, &subject); err != nil {
		t.Fatalf("no outbound request was staged: %v", err)
	}
	if target != "moderator@example.com" || !strings.Contains(subject, handle) {
		t.Fatalf("staged email to %q about %q", target, subject)
	}
}

// THE DAILY ASK FINDS A DUE ORDER AND ISSUES ITS LINK, through the real
// queries: the store's settings, the site that serves it, the orders the delay
// reached, their lines and products. The order mail route is not linked into
// this test engine, so the send fails -- and the request is recorded as failed,
// written as the merchant, ready for the next run to ask again.
func TestLiveE2E_TheDailyAskIssuesTheLinkADueOrderEarned(t *testing.T) {
	eng := livePackEngine(t)
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	merchant := "user-reviews-ask-" + n
	store := "store-reviews-ask-" + n
	site := "site-reviews-ask-" + n
	account := "acct-reviews-ask-" + n
	seedSiteIn(t, eng, site, merchant, store, account)
	if _, err := eng.Execute(asPerson(merchant, auth.RoleOwner),
		`builtin reviewsUpdateSettings(storeId: `+quote(store)+`, siteId: `+quote(site)+`, requestsEnabled: true, requestTemplateId: "tmpl-ask", requestSenderIdentityId: "si-house")`); err != nil {
		t.Fatalf("settings: %v", err)
	}
	orderGid := "gid://shopify/Order/" + n
	processed := time.Now().UTC().Add(-14*24*time.Hour - time.Hour).Format(time.RFC3339)
	money := map[string]any{"shopMoney": map[string]any{"amount": "24.00", "currencyCode": "USD"}}
	seedMirror(t, eng, "v1:shopify:order", "order-"+n, map[string]any{"storeId": store, "gid": orderGid,
		"email": "buyer@example.com", "name": "#1009", "displayFinancialStatus": "PAID", "totalPriceSet": money, "processedAt": processed})
	seedMirror(t, eng, "v1:shopify:orderLineItem", "line-"+n, map[string]any{"storeId": store, "gid": "gid://shopify/LineItem/" + n,
		"parentGid": orderGid, "productGid": "gid://shopify/Product/" + n, "priceAfterAllDiscountsBeforeTaxesSet": money, "title": "Floss Picks"})
	seedMirror(t, eng, "v1:shopify:product", "product-"+n, map[string]any{"storeId": store, "gid": "gid://shopify/Product/" + n,
		"handle": "floss-picks-" + n, "title": "Floss Picks"})

	summary := payloadOf(t, eng, auth.ContextWithInternalOrigin(asMerchant(merchant)),
		`builtin reviewsSendDueRequests(storeId: `+quote(store)+`)`)
	if summary["considered"] != float64(1) {
		t.Fatalf("summary = %v, want the one due line considered", summary)
	}
	var state, owner string
	if err := liveDB.DB.QueryRowContext(context.Background(),
		`SELECT coalesce(payload->>'sendState',''), coalesce(payload->>'ownerUserId','') FROM "MemoryNodes" WHERE concept = 'v1:reviews:reviewRequest' AND payload->>'orderGid' = $1 ORDER BY "createdAt" DESC LIMIT 1`,
		orderGid).Scan(&state, &owner); err != nil {
		t.Fatalf("no request was issued: %v", err)
	}
	if state != "failed" || strings.TrimPrefix(owner, "v1:identity:user:") != merchant {
		t.Fatalf("request: sendState %q owner %q, want failed (no mail route here) and written as the merchant", state, owner)
	}
}
