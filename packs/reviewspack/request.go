package reviewspack

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// request.go -- THE BUYER'S LINK.
//
// One v1:reviews:reviewRequest per product line of a paid order, at an id
// derived from (store, order, product) so a line is one link however often it
// is considered. The link is /reviews/write?product=<handle>&t=<token> on the
// store's own site. The token is 32 random bytes; only its SHA-256 is stored,
// so a database read never yields a working link.
//
// Through the link the buyer writes a review -- VERIFIED, because the link
// was minted from a paid order line -- reopens it to read what they wrote,
// and edits it while it waits: each edit is the next revision of the SAME
// review row, which returns it to the moderation queue. The link closes when
// its review is published, removed or erased (read off the review's state,
// so publishing writes nothing here) and when it expires.
//
// A refusal is NAMED (memql.ShopperRefusal), so the shopper's page can say
// what happened: link_invalid (no such link, another store, another
// product), link_expired, link_used (the review can no longer change).

// The link refusals, declared on the form and the draft read (shopper.go).
const (
	RefusalLinkInvalid = "link_invalid"
	RefusalLinkExpired = "link_expired"
	RefusalLinkUsed    = "link_used"
)

// NotThisSitesLink is the one answer the form's construct gives a caller who
// is not the owner of the site it names, or who names a site the pack does not
// admit for the store: link_invalid, before the token is judged.
const NotThisSitesLink = "the link cannot be used through this site"

// LinkRefusals is every code the link path may refuse with.
var LinkRefusals = []string{RefusalLinkExpired, RefusalLinkUsed, RefusalLinkInvalid}

const requestIDPrefix = "request-"

// mintToken makes one link token and the hash that is stored for it.
func mintToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("reviews: minting a link token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, tokenHash(token), nil
}

// tokenHash is the stored form of a token.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// requestIDFor derives one order line's request id: stable, so a line is one
// link, and a bare slug, because the engine refuses a colon in a row id.
func requestIDFor(storeID, orderGid, handle string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(storeID) + "\n" + strings.TrimSpace(orderGid) + "\n" + strings.TrimSpace(handle)))
	return requestIDPrefix + hex.EncodeToString(sum[:])[:24]
}

// reviewIDForRequest is the one review row a request's link writes, every
// revision of it.
func reviewIDForRequest(requestID string) string {
	return "review-" + strings.TrimPrefix(bareID(requestID, "v1:reviews:reviewRequest:"), requestIDPrefix)
}

// bareID strips a canonical concept prefix off a row id.
func bareID(id, prefix string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), prefix)
}

// ---------------------------------------------------------------- eligibility

// EligibleLine is one product of an order that earns a review link.
type EligibleLine struct {
	ProductHandle string
	ProductTitle  string
	ProductGid    string
}

// paidStatuses are the financial statuses of an order whose payment was
// captured. A refunded order is still a purchase: leaving unhappy buyers out
// of the ask is review gating.
var paidStatuses = map[string]bool{"PAID": true, "PARTIALLY_PAID": true, "PARTIALLY_REFUNDED": true, "REFUNDED": true}

// EligibleLines is the products of an order that earn a link: a paid order
// with a total above zero (an order at 100% off is not a purchase), not a
// test, not cancelled, with an address to write to; and, within it, each
// product whose own line is above zero after every discount (a free sample in
// a paid order earns none), once however many lines carry it.
func EligibleLines(order map[string]any, lines []map[string]any, handleOf func(productGid string) string) []EligibleLine {
	if order == nil || !paidStatuses[strings.ToUpper(asString(order["displayFinancialStatus"]))] {
		return nil
	}
	if b, _ := order["test"].(bool); b {
		return nil
	}
	if b, _ := order["deleted"].(bool); b {
		return nil
	}
	if strings.TrimSpace(asString(order["cancelledAt"])) != "" || strings.TrimSpace(asString(order["email"])) == "" {
		return nil
	}
	if amountOf(order["totalPriceSet"]) <= 0 {
		return nil
	}
	out := []EligibleLine{}
	seen := map[string]bool{}
	for _, l := range lines {
		if b, _ := l["deleted"].(bool); b {
			continue
		}
		if b, _ := l["isGiftCard"].(bool); b {
			continue
		}
		price := l["priceAfterAllDiscountsBeforeTaxesSet"]
		if price == nil {
			price = l["discountedTotalSet"]
		}
		if amountOf(price) <= 0 {
			continue
		}
		gid := strings.TrimSpace(asString(l["productGid"]))
		if gid == "" {
			continue
		}
		handle := strings.TrimSpace(handleOf(gid))
		if handle == "" || seen[handle] {
			continue
		}
		seen[handle] = true
		out = append(out, EligibleLine{ProductHandle: handle, ProductTitle: asString(l["title"]), ProductGid: gid})
	}
	return out
}

// amountOf reads a Shopify MoneyBag's shop amount. Unreadable is zero, which
// earns nothing: fail toward not mailing.
func amountOf(v any) float64 {
	m, _ := v.(map[string]any)
	shop, _ := m["shopMoney"].(map[string]any)
	f, err := strconv.ParseFloat(strings.TrimSpace(asString(shop["amount"])), 64)
	if err != nil {
		return 0
	}
	return f
}

// ---------------------------------------------------------------- the link

type link struct {
	request   map[string]any
	requestID string
	reviewID  string
	review    map[string]any // nil before the first submission
	state     State
}

func refuse(code, why string) error {
	return memql.NewShopperRefusal(code, why)
}

func (p *Provider) clock() time.Time {
	if p.now != nil {
		return p.now().UTC()
	}
	return time.Now().UTC()
}

// openLink resolves a token to its request and the review written through it,
// or refuses by name. Reads run under the caller's actor, which on the shopper
// path is the site owner's borrowed authority -- the request's owner.
func (p *Provider) openLink(ctx context.Context, storeID, token, productHandle string) (*link, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	token = strings.TrimSpace(token)
	if token == "" || strings.TrimSpace(storeID) == "" {
		return nil, refuse(RefusalLinkInvalid, "no link token")
	}
	rows, err := p.rowsFor(ctx, "reviewRequestByTokenHash", map[string]string{"storeId": storeID, "tokenHash": tokenHash(token)})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, refuse(RefusalLinkInvalid, "no request carries this token for this store")
	}
	req := rows[0]
	if asString(req["erasedAt"]) != "" {
		return nil, refuse(RefusalLinkUsed, "the reviewer data was erased")
	}
	if productHandle = strings.TrimSpace(productHandle); productHandle != "" && productHandle != strings.TrimSpace(asString(req["productHandle"])) {
		return nil, refuse(RefusalLinkInvalid, "the link is for another product")
	}
	lk := &link{request: req, requestID: bareID(asString(req["id"]), "v1:reviews:reviewRequest:")}
	lk.reviewID = reviewIDForRequest(lk.requestID)
	reviews, err := p.rowsFor(ctx, "reviewById", map[string]string{"reviewId": lk.reviewID})
	if err != nil {
		return nil, err
	}
	if len(reviews) > 0 {
		lk.review = reviews[0]
		decisions, err := p.decisionsForReview(ctx, canonicalReviewID(lk.reviewID))
		if err != nil {
			return nil, err
		}
		lk.state = StateOf(VersionOf(lk.review), decisions)
		switch lk.state {
		case StatePublished, StateRemoved, StateErased:
			return nil, refuse(RefusalLinkUsed, "the review is "+string(lk.state)+" and can no longer change")
		}
	}
	expires := asTime(req["expiresAt"])
	if expires.IsZero() || !p.clock().Before(expires) {
		return nil, refuse(RefusalLinkExpired, "the link expired at "+timeText(req["expiresAt"]))
	}
	return lk, nil
}

// submitFromLink is the shopper form's construct: a review written, or
// edited, through the buyer's link.
func (p *Provider) submitFromLink(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	siteID := strings.TrimSpace(asString(args["siteId"]))
	// THE SITE FIRST, and before the token: the construct writes as the owner
	// of the site the post arrived through, so it is for that site's owner and
	// for a site the pack admits for the store. The shopper handler runs it so,
	// as the site's owner; a caller naming it directly gets the same one answer
	// for every other site and every other caller, which says nothing about the
	// token.
	site, err := p.sitePostedThrough(ctx, siteID, storeID)
	if err != nil {
		return nil, err
	}
	lk, err := p.openLink(ctx, storeID, asString(args["requestToken"]), asString(args["productHandle"]))
	if err != nil {
		return nil, err
	}
	revision := 1
	if lk.review != nil {
		revision = revisionOf(asInt(lk.review["revision"])) + 1
	}
	write := map[string]any{
		"reviewId":      lk.reviewID,
		"storeId":       storeID,
		"productHandle": strings.TrimSpace(asString(lk.request["productHandle"])),
		"body":          asString(args["body"]),
		"requestId":     lk.requestID,
		"verifiedBuyer": true,
		"revision":      revision,
	}
	setIfPresent(write, "siteId", siteID)
	setIfPresent(write, "title", strings.TrimSpace(asString(args["title"])))
	setIfPresent(write, "authorName", strings.TrimSpace(asString(args["authorName"])))
	setIfPresent(write, "authorEmail", strings.TrimSpace(asString(args["authorEmail"])))
	if rating := asInt(args["rating"]); rating >= 1 && rating <= 5 {
		write["rating"] = rating
	}
	// THE ORGANIZATION IS THE SITE'S, copied onto the row so the store's
	// people read and moderate it (the account grant). A site of no
	// organization leaves the row untied: its owner's and the operators'.
	setIfPresent(write, "accountId", strings.TrimSpace(asString(site["accountId"])))
	if err := p.writer.Write(ctx, "putReview", write); err != nil {
		return nil, err
	}
	return receipt("v1:reviews:submitted", lk.reviewID, map[string]any{"reviewId": lk.reviewID, "revision": revision})
}

// sitePostedThrough is the site a form post names, once the caller is its
// owner and the pack admits it for the store (siteForStore): a site that serves
// the store and belongs to the store's organization. Every other case -- no
// site, no such site, a site of another store or organization, a caller who is
// not its owner, no session -- is one refusal, NotThisSitesLink. A read that
// fails is returned as the fault it is.
func (p *Provider) sitePostedThrough(ctx context.Context, siteID, storeID string) (map[string]any, error) {
	notThis := refuse(RefusalLinkInvalid, NotThisSitesLink)
	access, ok := auth.AccessFromContext(ctx)
	if !ok || access == nil || access.IsAnonymousActor() || strings.TrimSpace(access.UserId) == "" {
		return nil, notThis
	}
	site, err := p.siteForStore(ctx, siteID, storeID)
	if errors.Is(err, errSiteNotAdmitted) {
		return nil, notThis
	}
	// A FAULT IS NOT A REFUSAL: it goes back as it is, and the shopper handler
	// tells the shopper its general failure. It depends on nothing about which
	// site was named or whose it is, so it reveals neither.
	if err != nil {
		return nil, err
	}
	if !sameUser(asString(site["ownerUserId"]), access.UserId) {
		return nil, notThis
	}
	return site, nil
}

// draftForLink answers what the buyer wrote through their link, so reopening
// it pre-fills the form, and refuses a closed link before anyone types.
func (p *Provider) draftForLink(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	lk, err := p.openLink(ctx, storeID, asString(args["requestToken"]), asString(args["productHandle"]))
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"state":         "new",
		"productHandle": strings.TrimSpace(asString(lk.request["productHandle"])),
		"expiresAt":     timeText(lk.request["expiresAt"]),
	}
	if lk.review != nil {
		body["state"] = string(lk.state)
		body["revision"] = revisionOf(asInt(lk.review["revision"]))
		for _, k := range []string{"title", "body", "authorName"} {
			if v := strings.TrimSpace(asString(lk.review[k])); v != "" {
				body[k] = v
			}
		}
		if r := asRating(lk.review["rating"]); r >= 1 && r <= 5 {
			body["rating"] = r
		}
	}
	return receipt("v1:reviews:draft", lk.reviewID, body)
}

// siteRow reads one site under the caller's actor; nil when it cannot, a
// failed read included. For a caller that refuses on nil (reviewsIssueRequest).
func (p *Provider) siteRow(ctx context.Context, siteID string) map[string]any {
	site, err := p.siteRowErr(ctx, siteID)
	if err != nil {
		return nil
	}
	return site
}

// siteRowErr reads one site under the caller's actor: nil and no error when no
// such site is readable, and the read's own error when the read failed.
func (p *Provider) siteRowErr(ctx context.Context, siteID string) (map[string]any, error) {
	if siteID == "" {
		return nil, nil
	}
	rows, err := p.rowsFor(ctx, "siteById", map[string]string{"siteId": siteID})
	if err != nil {
		return nil, fmt.Errorf("reviews: reading site %q: %w", siteID, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// ---------------------------------------------------------------- issuing

// orderReader reads one order, its lines and its products' handles.
type orderReader func(ctx context.Context, storeID, orderGid string) (order map[string]any, lines []map[string]any, handleOf func(string) string, err error)

// engineOrders is the production orderReader: the mirrored order, its lines
// and its products, read under ctx.
func (p *Provider) engineOrders(ctx context.Context, storeID, orderGid string) (map[string]any, []map[string]any, func(string) string, error) {
	orders, err := p.rowsFor(ctx, "shopifyOrderByGid", map[string]string{"storeId": storeID, "gid": orderGid})
	if err != nil {
		return nil, nil, nil, err
	}
	if len(orders) == 0 {
		return nil, nil, nil, fmt.Errorf("reviews: order %q is not readable on store %q", orderGid, storeID)
	}
	lines, err := p.rowsFor(ctx, "reviewOrderLines", map[string]string{"storeId": storeID, "orderGid": orderGid})
	if err != nil {
		return nil, nil, nil, err
	}
	handles := map[string]string{}
	handleOf := func(productGid string) string {
		if h, ok := handles[productGid]; ok {
			return h
		}
		rows, err := p.rowsFor(ctx, "shopifyProductByGid", map[string]string{"storeId": storeID, "gid": productGid})
		h := ""
		if err == nil && len(rows) > 0 {
			h = strings.TrimSpace(asString(rows[0]["handle"]))
		}
		handles[productGid] = h
		return h
	}
	return orders[0], lines, handleOf, nil
}

// issueRequest mints one link by hand, for the cluster's operators: testing a
// store before it goes live, and re-sending a buyer's link on the store's
// request. The request row is written as the SITE OWNER, whose borrowed
// authority the shopper path reads it under.
func (p *Provider) issueRequest(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || !auth.IsClusterOperator(ac.Role) {
		return nil, fmt.Errorf("reviews: only the cluster's operators issue a review link by hand")
	}
	storeID := strings.TrimSpace(asString(args["storeId"]))
	orderGid := strings.TrimSpace(asString(args["orderGid"]))
	handle := strings.TrimSpace(asString(args["productHandle"]))
	siteID := strings.TrimSpace(asString(args["siteId"]))
	if storeID == "" || orderGid == "" || handle == "" || siteID == "" {
		return nil, fmt.Errorf("reviews: storeId, orderGid, productHandle and siteId are required")
	}
	site := p.siteRow(ctx, siteID)
	if site == nil {
		return nil, fmt.Errorf("reviews: site %q is not readable", siteID)
	}
	if !siteServesStore(site, storeID) {
		return nil, fmt.Errorf("reviews: site %q is not bound to store %q", siteID, storeID)
	}
	owner := strings.TrimSpace(asString(site["ownerUserId"]))
	host := strings.TrimSpace(asString(site["hostname"]))
	if owner == "" || host == "" {
		return nil, fmt.Errorf("reviews: site %q has no owner or no address", siteID)
	}
	read := p.orders
	if read == nil {
		read = p.engineOrders
	}
	// THE CLUSTER'S OPERATORS ONLY (the builtin's description). Mirrored orders are cluster-owner tier, so an operator
	// below owner could not read the order its link is for; an operator reads it as the daily ask does. Anyone else who
	// holds the reviews part (a client's moderator) still reads it as themselves, and is refused.
	readCtx := ctx
	if ac, ok := auth.AccessFromContext(ctx); ok && auth.IsClusterOperator(ac.Role) {
		readCtx = auth.ContextWithSystemActor(ctx, dueActor)
	}
	order, lines, handleOf, err := read(readCtx, storeID, orderGid)
	if err != nil {
		return nil, err
	}
	var chosen *EligibleLine
	for _, l := range EligibleLines(order, lines, handleOf) {
		if l.ProductHandle == handle {
			l := l
			chosen = &l
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("reviews: %s on order %s does not earn a review link (a paid line above $0 of a paid, uncancelled order is needed)", handle, orderGid)
	}
	settings, err := p.settingsFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	requestID := requestIDFor(storeID, orderGid, handle)
	held, hold, release, err := p.lockRequest(ctx, requestID)
	if err != nil {
		return nil, err
	}
	defer release()
	existing, at, err := p.currentRequest(auth.ContextWithSystemActor(held, dueActor), requestID)
	if err != nil {
		return nil, err
	}
	if erased, err := p.requestWasErased(auth.ContextWithSystemActor(held, dueActor), requestID); err != nil {
		return nil, err
	} else if erased {
		return nil, fmt.Errorf("reviews: an erased request cannot be reissued")
	}
	if existing != nil && asString(existing["erasedAt"]) != "" {
		return nil, fmt.Errorf("reviews: an erased request cannot be reissued")
	}
	token, hash, err := mintToken()
	if err != nil {
		return nil, err
	}
	now := p.clock()
	expires := now.Add(time.Duration(EffectiveSettings(settings).LinkLifetimeDays) * 24 * time.Hour)
	write := map[string]any{
		"requestId":       requestID,
		"storeId":         storeID,
		"siteId":          siteID,
		"orderGid":        orderGid,
		"productHandle":   handle,
		"email":           strings.TrimSpace(asString(order["email"])),
		"tokenHash":       hash,
		"issuedAt":        now.Format(time.RFC3339Nano),
		"expiresAt":       expires.Format(time.RFC3339Nano),
		"sendState":       "skipped",
		"sendNote":        "issued by hand",
		"tokenCiphertext": "",
	}
	setIfPresent(write, "accountId", strings.TrimSpace(asString(site["accountId"])))
	setIfPresent(write, "orderName", strings.TrimSpace(asString(order["name"])))
	setIfPresent(write, "productTitle", strings.TrimSpace(chosen.ProductTitle))
	if err := p.writeRequest(auth.ContextWithUserActor(held, owner), hold, requestID, at, write); err != nil {
		return nil, err
	}
	return receipt("v1:reviews:issued", requestID, map[string]any{
		"requestId": requestID,
		"url":       linkURL(host, handle, token),
		"expiresAt": expires.Format(time.RFC3339Nano),
	})
}

// linkURL is a buyer's link on the store's own site.
func linkURL(host, handle, token string) string {
	return "https://" + host + "/reviews/write?product=" + url.QueryEscape(handle) + "&t=" + url.QueryEscape(token)
}

// siteServesStore reports whether a site is bound to a store, as its serving
// or its preview binding.
func siteServesStore(site map[string]any, storeID string) bool {
	for _, k := range []string{"binding", "previewBinding"} {
		b, _ := site[k].(map[string]any)
		if strings.TrimSpace(asString(b["storeId"])) == storeID {
			return true
		}
	}
	return false
}
