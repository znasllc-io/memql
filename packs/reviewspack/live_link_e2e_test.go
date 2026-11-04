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
	"errors"
	"strconv"
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
	seedSite(t, eng, site, merchant, store)
	grantReviewsPart(t, eng, merchant)
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
	if _, err := eng.Execute(asPerson(merchant, auth.Role("user")),
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
