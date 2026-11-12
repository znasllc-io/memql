package reviewspack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// request_test.go -- A BUYER'S LINK.
//
// One link per product line of a paid order. The token is 32 random bytes
// and only its SHA-256 is stored. Through it the buyer writes a review,
// verified because the link is; reopens it to read what they wrote; and edits
// it while it waits -- each edit the next revision of the same row. The link
// closes when the review is published (or removed or erased) and when it
// expires, and a refusal names which.

func TestTheTokenIsRandomAndOnlyItsHashIsKept(t *testing.T) {
	a, ha, err := mintToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := mintToken()
	if a == b || len(a) < 43 || strings.ContainsAny(a, "+/=") {
		t.Fatalf("tokens %q and %q: want two different url-safe 32-byte tokens", a, b)
	}
	sum := sha256.Sum256([]byte(a))
	if ha != hex.EncodeToString(sum[:]) || tokenHash(a) != ha {
		t.Fatalf("hash = %q, want the SHA-256 of the token", ha)
	}
}

func TestRequestAndReviewIDsAreStableBareSlugs(t *testing.T) {
	id := requestIDFor("v1:shopify:store:s1", "gid://shopify/Order/9", "fylo-start-floss-picks")
	if id != requestIDFor("v1:shopify:store:s1", "gid://shopify/Order/9", "fylo-start-floss-picks") {
		t.Fatal("the request id is not stable: one order line must be one link")
	}
	if strings.Contains(id, ":") || !strings.HasPrefix(id, "request-") {
		t.Fatalf("requestIDFor = %q, want a bare request- slug", id)
	}
	if rv := reviewIDForRequest(id); strings.Contains(rv, ":") || rv == id || !strings.HasPrefix(rv, "review-") {
		t.Fatalf("reviewIDForRequest = %q, want a bare review- slug of its own", rv)
	}
}

// ---------------------------------------------------------------- eligibility

func money(amount string) map[string]any {
	return map[string]any{"shopMoney": map[string]any{"amount": amount, "currencyCode": "USD"}}
}

func paidOrder() map[string]any {
	return map[string]any{"gid": "gid://shopify/Order/9", "name": "#1009", "email": "buyer@example.com",
		"displayFinancialStatus": "PAID", "totalPriceSet": money("24.00")}
}

func line(product, amount string) map[string]any {
	return map[string]any{"productGid": product, "priceAfterAllDiscountsBeforeTaxesSet": money(amount), "title": "t"}
}

func handles(gid string) string {
	return map[string]string{"gid://shopify/Product/1": "floss-picks", "gid://shopify/Product/2": "toothpaste"}[gid]
}

func eligibleHandles(t *testing.T, order map[string]any, lines ...map[string]any) []string {
	t.Helper()
	got := []string{}
	for _, l := range EligibleLines(order, lines, handles) {
		got = append(got, l.ProductHandle)
	}
	return got
}

func TestAPaidOrderEarnsOneLinkPerPaidProduct(t *testing.T) {
	got := eligibleHandles(t, paidOrder(),
		line("gid://shopify/Product/1", "12.00"),
		line("gid://shopify/Product/1", "12.00"), // the same product twice is one link
		line("gid://shopify/Product/2", "0.00"),  // a free sample in a paid order earns none
	)
	if len(got) != 1 || got[0] != "floss-picks" {
		t.Fatalf("eligible = %v, want [floss-picks]", got)
	}
}

func TestOrdersThatEarnNoLink(t *testing.T) {
	cases := map[string]func(map[string]any){
		"a $0 order (a 100% discount)": func(o map[string]any) { o["totalPriceSet"] = money("0.00") },
		"payment not captured":         func(o map[string]any) { o["displayFinancialStatus"] = "AUTHORIZED" },
		"a voided order":               func(o map[string]any) { o["displayFinancialStatus"] = "VOIDED" },
		"a test order":                 func(o map[string]any) { o["test"] = true },
		"a cancelled order":            func(o map[string]any) { o["cancelledAt"] = "2026-11-01T10:00:00Z" },
		"no email to send to":          func(o map[string]any) { o["email"] = "" },
	}
	for name, change := range cases {
		o := paidOrder()
		change(o)
		if got := eligibleHandles(t, o, line("gid://shopify/Product/1", "12.00")); len(got) != 0 {
			t.Errorf("%s earned links %v", name, got)
		}
	}
}

// A REFUNDED ORDER IS STILL A PURCHASE. Leaving unhappy buyers out of the ask
// is the review gating the store's criteria forbid.
func TestARefundedOrderStillEarnsALink(t *testing.T) {
	for _, status := range []string{"REFUNDED", "PARTIALLY_REFUNDED", "PARTIALLY_PAID"} {
		o := paidOrder()
		o["displayFinancialStatus"] = status
		if got := eligibleHandles(t, o, line("gid://shopify/Product/1", "12.00")); len(got) != 1 {
			t.Errorf("%s earned %v, want one link", status, got)
		}
	}
}

// ---------------------------------------------------------------- the link

// linkReader answers the reads the link path makes.
type linkReader struct {
	request   map[string]any            // by token hash
	reviews   map[string]map[string]any // by id
	decisions []map[string]any
	site      map[string]any
}

func (l *linkReader) Rows(_ context.Context, query string, args map[string]string) ([]map[string]any, error) {
	switch query {
	case "reviewRequestByTokenHash":
		if l.request != nil && l.request["tokenHash"] == args["tokenHash"] && l.request["storeId"] == args["storeId"] {
			return []map[string]any{l.request}, nil
		}
	case "reviewById":
		if r, ok := l.reviews[args["reviewId"]]; ok {
			return []map[string]any{r}, nil
		}
	case "reviewDecisionsForReview":
		return l.decisions, nil
	case "siteById":
		// THE STORE'S SITE, owned by the request's merchant: the site a form
		// post arrives through. A test's own fields are laid over it.
		site := map[string]any{"id": args["siteId"], "ownerUserId": "merchant-1",
			"binding": map[string]any{"storeId": "store-live"}}
		for k, v := range l.site {
			site[k] = v
		}
		return []map[string]any{site}, nil
	}
	return nil, nil
}

// linkCaller is the caller the shopper handler runs the form's construct as:
// the owner of the site the post arrived through.
func linkCaller() context.Context {
	return auth.ContextWithUserActor(context.Background(), "merchant-1")
}

// THE CONSTRUCT IS FOR THE SITE IT NAMES: a caller who is not the site's
// owner, a site of another store and no site at all are one refusal, before
// the token is judged, and nothing is written.
func TestTheFormsConstructIsForTheOwnerOfTheSiteItNames(t *testing.T) {
	cases := []struct {
		name   string
		ctx    context.Context
		site   map[string]any
		change func(map[string]any)
	}{
		{"another person", auth.ContextWithUserActor(context.Background(), "someone-else"), nil, nil},
		{"no session", context.Background(), nil, nil},
		{"a site of another store", linkCaller(), map[string]any{"binding": map[string]any{"storeId": "store-other"}}, nil},
		{"no site named", linkCaller(), nil, func(a map[string]any) { delete(a, "siteId") }},
	}
	for _, c := range cases {
		p, w := linkProvider(&linkReader{request: openRequest(), site: c.site})
		args := linkArgs()
		if c.change != nil {
			c.change(args)
		}
		_, err := p.submitFromLink(c.ctx, args, 0)
		var refusal *memql.ShopperRefusal
		if !errors.As(err, &refusal) || refusal.Code != RefusalLinkInvalid || !strings.Contains(err.Error(), NotThisSitesLink) {
			t.Errorf("%s: %v, want link_invalid: %s", c.name, err, NotThisSitesLink)
		}
		w.none(t)
	}
}

func (l *linkReader) RowsWithList(context.Context, string, map[string]string, string, []string) ([]map[string]any, error) {
	return nil, nil
}

const theToken = "tok-0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

var linkNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func openRequest() map[string]any {
	return map[string]any{"id": "v1:reviews:reviewRequest:request-abc", "storeId": "store-live",
		"productHandle": "floss-picks", "tokenHash": tokenHash(theToken), "ownerUserId": "merchant-1",
		"expiresAt": linkNow.Add(48 * time.Hour).Format(time.RFC3339Nano)}
}

func linkArgs() map[string]any {
	return map[string]any{"requestToken": theToken, "productHandle": "floss-picks", "body": "Easy to use",
		"rating": 5, "authorName": "Dana", "storeId": "store-live", "siteId": "site-1", "submissionId": "sub-1"}
}

func linkProvider(r *linkReader) (*Provider, *fakeWriter) {
	w := &fakeWriter{}
	return &Provider{reader: r, writer: w, now: func() time.Time { return linkNow }}, w
}

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	var refusal *memql.ShopperRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a named refusal", err)
	}
	return refusal.Code
}

func TestAValidLinkWritesAVerifiedReviewAtRevisionOne(t *testing.T) {
	p, w := linkProvider(&linkReader{request: openRequest(), site: map[string]any{"accountId": "acct-shop"}})
	if _, err := p.submitFromLink(linkCaller(), linkArgs(), 0); err != nil {
		t.Fatal(err)
	}
	row := w.only(t, "putReview")
	for k, want := range map[string]any{"reviewId": "review-abc", "requestId": "request-abc", "verifiedBuyer": true,
		"revision": 1, "storeId": "store-live", "accountId": "acct-shop", "body": "Easy to use", "productHandle": "floss-picks"} {
		if row[k] != want {
			t.Fatalf("%s = %v, want %v (row %v)", k, row[k], want, row)
		}
	}
}

func TestAnEditThroughTheLinkIsTheNextRevision(t *testing.T) {
	r := &linkReader{request: openRequest(), reviews: map[string]map[string]any{
		"review-abc": {"id": "review-abc", "storeId": "store-live", "revision": 1, "body": "first"},
	}}
	p, w := linkProvider(r)
	if _, err := p.submitFromLink(linkCaller(), linkArgs(), 0); err != nil {
		t.Fatal(err)
	}
	if got := w.only(t, "putReview")["revision"]; got != 2 {
		t.Fatalf("revision = %v, want 2: an edit is the next revision of the SAME row", got)
	}
}

// THE LETTER OF THE PROCESS: the link closes on publish or expiry, so a
// declined review can still be changed while its link is open.
func TestADeclinedReviewCanStillBeEdited(t *testing.T) {
	r := &linkReader{request: openRequest(),
		reviews:   map[string]map[string]any{"review-abc": {"id": "review-abc", "storeId": "store-live", "revision": 1}},
		decisions: []map[string]any{decisionRow("review-abc", ActionDecline, 1, 0)},
	}
	p, w := linkProvider(r)
	if _, err := p.submitFromLink(linkCaller(), linkArgs(), 0); err != nil {
		t.Fatalf("a declined review's open link refused an edit: %v", err)
	}
	w.only(t, "putReview")
}

func TestTheLinkRefusesByName(t *testing.T) {
	expired := openRequest()
	expired["expiresAt"] = linkNow.Add(-time.Minute).Format(time.RFC3339Nano)
	published := &linkReader{request: openRequest(),
		reviews:   map[string]map[string]any{"review-abc": {"id": "review-abc", "storeId": "store-live", "revision": 1}},
		decisions: []map[string]any{decisionRow("review-abc", ActionPublish, 1, 0)},
	}
	cases := []struct {
		name   string
		reader *linkReader
		change func(map[string]any)
		code   string
	}{
		{"no token", &linkReader{request: openRequest()}, func(a map[string]any) { a["requestToken"] = "" }, "link_invalid"},
		{"a forged token", &linkReader{request: openRequest()}, func(a map[string]any) { a["requestToken"] = "forged" }, "link_invalid"},
		{"another store's token", &linkReader{request: openRequest()}, func(a map[string]any) { a["storeId"] = "store-other" }, "link_invalid"},
		{"another product", &linkReader{request: openRequest()}, func(a map[string]any) { a["productHandle"] = "toothpaste" }, "link_invalid"},
		{"expired", &linkReader{request: expired}, nil, "link_expired"},
		{"published", published, nil, "link_used"},
	}
	for _, c := range cases {
		p, w := linkProvider(c.reader)
		args := linkArgs()
		if c.change != nil {
			c.change(args)
		}
		_, err := p.submitFromLink(linkCaller(), args, 0)
		if got := refusalCode(t, err); got != c.code {
			t.Errorf("%s: refused %q, want %q", c.name, got, c.code)
		}
		w.none(t)
		if _, err := p.draftForLink(context.Background(), args, 0); refusalCode(t, err) != c.code {
			t.Errorf("%s: the draft read refused differently from the form", c.name)
		}
	}
}

func TestTheDraftAnswersWhatTheBuyerWrote(t *testing.T) {
	r := &linkReader{request: openRequest(), reviews: map[string]map[string]any{
		"review-abc": {"id": "review-abc", "storeId": "store-live", "revision": 1, "body": "first", "rating": 4, "authorName": "Dana"},
	}}
	p, _ := linkProvider(r)
	nodes, err := p.draftForLink(context.Background(), map[string]any{"requestToken": theToken, "storeId": "store-live"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(nodes[0].Payload)
	for _, want := range []string{`"body":"first"`, `"rating":4`, `"state":"waiting"`, `"productHandle":"floss-picks"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("draft = %s, want it to carry %s", payload, want)
		}
	}
	if strings.Contains(payload, "tokenHash") || strings.Contains(payload, "email") {
		t.Fatalf("draft = %s: it must not carry the hash or any address", payload)
	}
}

func TestIssuingALinkIsForOperatorsAndWritesAsTheSiteOwner(t *testing.T) {
	r := &linkReader{site: map[string]any{"id": "site-1", "ownerUserId": "merchant-1", "accountId": "acct-shop",
		"hostname": "shop.example.com", "binding": map[string]any{"storeId": "store-live"}}}
	order := paidOrder()
	p, w := linkProvider(r)
	p.orders = func(context.Context, string, string) (map[string]any, []map[string]any, func(string) string, error) {
		return order, []map[string]any{line("gid://shopify/Product/1", "12.00")}, handles, nil
	}
	args := map[string]any{"storeId": "store-live", "orderGid": "gid://shopify/Order/9", "productHandle": "floss-picks", "siteId": "site-1"}
	if _, err := p.issueRequest(moderatorCtx("mod-1"), args, 0); err == nil {
		t.Fatal("a non-operator issued a review link")
	}
	w.none(t)
	op := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "staff-1", Role: auth.RoleOwner})
	nodes, err := p.issueRequest(op, args, 0)
	if err != nil {
		t.Fatal(err)
	}
	row := w.only(t, "putReviewRequest")
	if w.calls[0].actor != "merchant-1" {
		t.Fatalf("the request was written as %q, want the site owner: the shopper path reads it under their authority", w.calls[0].actor)
	}
	if row["email"] != "buyer@example.com" || row["tokenHash"] == "" || row["expiresAt"] == "" {
		t.Fatalf("row = %v", row)
	}
	if strings.Contains(string(nodes[0].Payload), asString(row["tokenHash"])) {
		t.Fatal("the answer carries the stored hash; it must carry the link only")
	}
	var answer map[string]string
	if err := json.Unmarshal(nodes[0].Payload, &answer); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(answer["url"], "https://shop.example.com/reviews/write?product=floss-picks&t=") {
		t.Fatalf("url = %q, want the link on the site's own address", answer["url"])
	}
	if token := strings.TrimPrefix(answer["url"], "https://shop.example.com/reviews/write?product=floss-picks&t="); tokenHash(token) != row["tokenHash"] {
		t.Fatal("the link's token does not hash to the stored hash: the buyer's link would be refused")
	}
}
