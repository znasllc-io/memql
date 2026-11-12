package reviewspack_test

// server_written_db_test.go -- THE PACK'S ROWS ARE WRITTEN ONLY BY THE PACK.
//
// The engine's raw insert() is public language and never consults a
// mutation's @serverOnly: that annotation gates a named mutation, not the
// concept. @serverWritten on a concept refuses every write without internal
// origin, and every concept this pack declares carries it. So a raw insert of
// any of the five -- a review already carrying a buyer's name, a decision that
// publishes, a link carrying a token the writer chose, settings naming another
// address -- is refused for the store's owner, for a person the reviews part
// admits to moderate, for a person of another organization and for a caller
// with no session; and no mutation of the pack is callable by name around the
// builtin that judges it.
//
// The pack's own paths still write: a buyer's first review and an edit through
// the real form route and the real handler, each named refusal of that form
// with its code and redirect, a decision and a settings save through the
// builtins (the daily ask's write and settlement are
// TestLiveE2E_TheDailyAskIssuesTheLinkADueOrderEarned).
//
// Postgres-gated like every *_db_test.go: skips when no database is reachable,
// and MEMQL_REQUIRE_DB=1 turns that skip into a failure.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/packs/reviewspack"
)

// serverWrittenRefusal is the engine's own refusal of a write to a
// @serverWritten concept without internal origin.
const serverWrittenRefusal = "@serverWritten requires internal origin"

// anonymousWriteRefusal is the engine's refusal of any write by a caller with
// no session, which comes before every concept's own rule.
const anonymousWriteRefusal = "this call carries no identity"

// reviewsExecutor is the bff's engine adapter: the real engine behind the real
// shopper handler.
type reviewsExecutor struct{ eng *memql.MemQLEngine }

func (e reviewsExecutor) Execute(ctx context.Context, q string) (any, error) { return e.eng.Execute(ctx, q) }

// reviewsSites resolves a site row under its owner's own actor, as the bff's
// site reader does. The seeded sites carry no shopperForms switch, and the
// switch is not what these tests are about, so every readable site has it on.
type reviewsSites struct{ eng *memql.MemQLEngine }

func (s reviewsSites) ShopperSite(_ context.Context, siteID, owner string) (*server.ShopperSite, error) {
	result, err := s.eng.Execute(asMerchant(owner), `query siteById(siteId: `+quote(siteID)+`)`)
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

// declareShopperSurface declares the pack's shopper routes for one test, as
// Register does at boot, and takes them away after it.
func declareShopperSurface(t *testing.T) {
	t.Helper()
	memql.ResetShopperSurfaceForTest()
	t.Cleanup(memql.ResetShopperSurfaceForTest)
	reviewspack.RegisterShopperSurfaceForTest()
}

// postReviewForm is a buyer's post of the reviews form through the real
// handler, stamped as the edge stamps it: the site, the store it is bound to
// and the site's owner, whose authority the post borrows.
func postReviewForm(t *testing.T, eng *memql.MemQLEngine, site, store, owner string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, memql.ShopperFormPathPrefix+"reviews/review", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(memql.ShopperSiteHeader, site)
	req.Header.Set(memql.ShopperStoreHeader, store)
	req.Header.Set(memql.ShopperOwnerHeader, owner)
	rec := httptest.NewRecorder()
	// The handler's own log of a refusal it answers by name is not this
	// suite's output.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	server.NewShopperHandler(reviewsExecutor{eng}, reviewsSites{eng}, quiet).ServeHTTP(rec, req)
	return rec
}

// reviewForm is the form a buyer posts through their link.
func reviewForm(token, handle, body string) url.Values {
	return url.Values{"requestToken": {token}, "productHandle": {handle}, "body": {body}, "title": {"Fits well"},
		"rating": {"4"}, "authorName": {"Dana"}, "authorEmail": {"dana@example.com"}}
}

// redirectedTo asserts a 303 to want.
func redirectedTo(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != want {
		t.Fatalf("the form answered %d to %q, want 303 to %q", rec.Code, loc, want)
	}
}

// seedOwnerUser writes the user row a site owner has, BEFORE their site is
// seeded: the shopper path reads the site under its owner's own actor, and the
// site's stored owner is that person's canonical id.
func seedOwnerUser(t *testing.T, eng *memql.MemQLEngine, userID string) {
	t.Helper()
	payload := map[string]any{"displayName": userID, "primaryEmail": userID + "@example.test",
		"role": string(auth.RoleWriter), "active": true}
	if _, err := eng.Execute(seedCtx(), rawReviewsInsert("v1:identity:user", userID, payload)); err != nil {
		t.Fatalf("seed the site owner: %v", err)
	}
}

// siteOwnerOf is the site's stored owner: what the edge stamps as the owner
// whose authority a buyer's post borrows.
func siteOwnerOf(t *testing.T, siteID string) string {
	t.Helper()
	return asText(latestOf(t, "v1:platform:site", siteID)["ownerUserId"])
}

// reviewThroughLink is a buyer's review as a buyer writes one: a link for the
// product (written as the pack writes one), then the form posted through it.
// It answers the review's canonical row id.
func reviewThroughLink(t *testing.T, eng *memql.MemQLEngine, site, store, owner, requestID, handle, body string) string {
	t.Helper()
	token := "tok-" + requestID + "-0123456789abcdefghijklmnopqrstuvwxyz"
	putRequest(t, eng, owner, store, requestID, handle, token, time.Now().Add(14*24*time.Hour))
	redirectedTo(t, postReviewForm(t, eng, site, store, owner, reviewForm(token, handle, body)), "/reviews/thank-you")
	return "v1:reviews:review:" + reviewspack.ReviewIDForRequestForTest(requestID)
}

// rawReviewsInsert is the public insert() any caller may write.
func rawReviewsInsert(concept, id string, payload map[string]any) string {
	raw, _ := json.Marshal(payload)
	return `insert(` + langparser.QuoteString(concept) + `, id=` + langparser.QuoteString(id) + `, payload=` + string(raw) + `)`
}

// versionsOf is how many versions of one row are stored: a write that must
// not happen is measured by the version it did not add.
func versionsOf(t *testing.T, concept, rowID string) int {
	t.Helper()
	if !strings.HasPrefix(rowID, concept+":") {
		rowID = concept + ":" + rowID
	}
	var n int
	if err := liveDB.DB.QueryRowContext(context.Background(),
		`SELECT count(*) FROM "MemoryNodes" WHERE concept = $1 AND id = $2`, concept, rowID).Scan(&n); err != nil {
		t.Fatalf("count versions of %s: %v", rowID, err)
	}
	return n
}

// latestOf is the newest stored version of a row, read off the table.
func latestOf(t *testing.T, concept, rowID string) map[string]any {
	t.Helper()
	if !strings.HasPrefix(rowID, concept+":") {
		rowID = concept + ":" + rowID
	}
	var raw []byte
	if err := liveDB.DB.QueryRowContext(context.Background(),
		`SELECT payload FROM "MemoryNodes" WHERE concept = $1 AND id = $2 ORDER BY "createdAt" DESC LIMIT 1`,
		concept, rowID).Scan(&raw); err != nil {
		t.Fatalf("read %s: %v", rowID, err)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// writtenWorld is one store of one organization in the production shape, with
// an open buyer's link and the review written through it by the real form.
type writtenWorld struct {
	eng                        *memql.MemQLEngine
	n                          string
	merchant, store, site      string
	owner                      string // the site's stored owner, which the edge stamps
	account                    string
	moderator, outsider        string // canonical ids, as a signed-in actor carries them
	handle, token              string
	requestID, reviewID        string // bare
	reviewRowID, decisionRowID string // canonical
}

func newWrittenWorld(t *testing.T, label string) *writtenWorld {
	t.Helper()
	eng := livePackEngine(t)
	declareShopperSurface(t)
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	w := &writtenWorld{eng: eng, n: n,
		merchant: "user-reviews-" + label + "-shop-" + n, store: "store-reviews-" + label + "-" + n,
		site: "site-reviews-" + label + "-" + n, account: "acct-reviews-" + label + "-" + n,
		handle: "floss-picks-" + n, token: "tok-" + label + "-" + n + "-0123456789abcdefghijklmnopqrstuvwxyz",
		requestID: "request-" + label + "-" + n}
	other := "acct-reviews-" + label + "-other-" + n
	group, otherGroup := seedOrganization(t, eng, w.account), seedOrganization(t, eng, other)
	for _, g := range []string{group, otherGroup} {
		grantGroup(t, eng, g, "read", "app:deployables")
		grantGroup(t, eng, g, "execute", "app:deployables/reviews")
	}
	// The merchant is one of the organization's people, in a group of its own
	// that holds no reviews part: the site they own is read under their actor.
	own := "group-reviews-" + label + "-own-" + n
	seedGroup(t, eng, own, "custom", w.account)
	seedPersonIn(t, eng, w.merchant, "Morgan Merchant", string(auth.RoleWriter), true, own, w.account, "added")
	seedSiteIn(t, eng, w.site, w.merchant, w.store, w.account)
	w.owner = siteOwnerOf(t, w.site)
	moderator, outsider := "user-reviews-"+label+"-moderator-"+n, "user-reviews-"+label+"-outsider-"+n
	seedPersonIn(t, eng, moderator, "Robin Moderator", string(auth.RoleWriter), true, group, w.account, "added")
	seedPersonIn(t, eng, outsider, "Oli Outsider", string(auth.RoleWriter), true, otherGroup, other, "added")
	w.moderator, w.outsider = "v1:identity:user:"+moderator, "v1:identity:user:"+outsider

	// The store's settings, set up by an operator who owns the site (the
	// production shape), with the display on.
	if _, err := eng.Execute(asPerson(w.owner, auth.RoleOwner),
		`builtin reviewsUpdateSettings(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+`, publicDisplay: true)`); err != nil {
		t.Fatalf("settings: %v", err)
	}
	// The buyer's link, written as the pack writes one (its own @serverOnly
	// mutation at internal origin, as the site owner): the fixture the
	// operator's reviewsIssueRequest and the daily ask stand for.
	putRequest(t, eng, w.owner, w.store, w.requestID, w.handle, w.token, time.Now().Add(14*24*time.Hour))
	w.reviewID = reviewspack.ReviewIDForRequestForTest(w.requestID)
	w.reviewRowID = "v1:reviews:review:" + w.reviewID

	// THE BUYER'S FIRST REVIEW, through the form route.
	redirectedTo(t, postReviewForm(t, eng, w.site, w.store, w.owner, reviewForm(w.token, w.handle, "first words")),
		"/reviews/thank-you")
	// A decision on it by the store's moderator, through the builtin: escalate
	// changes nothing the shopper sees, so the link stays open.
	if _, err := eng.Execute(w.as(w.moderator),
		`builtin reviewsDecide(reviewId: `+quote(w.reviewRowID)+`, action: "escalate", revision: 1, note: "checking")`); err != nil {
		t.Fatalf("the moderator could not decide: %v", err)
	}
	log := rowsOf(t, eng, w.as(w.moderator), `query reviewDecisionsForReview(reviewId: `+quote(w.reviewRowID)+`)`)
	if len(log) != 1 {
		t.Fatalf("the decision log holds %d decisions, want the one escalate", len(log))
	}
	w.decisionRowID = asText(log[0]["id"])
	return w
}

// as is a signed-in person, in the form a deployed cluster spells them.
func (w *writtenWorld) as(userID string) context.Context { return asPerson(userID, auth.RoleWriter) }

// writeCaller is one caller a write is tried by, and the person they are (""
// for a caller with no session).
type writeCaller struct {
	name, self string
	ctx        context.Context
}

// callers are the four a raw write is tried by: the store's owner signed in as
// themselves, a person the reviews part admits to moderate the store, a person
// holding the same part in another organization, and a caller with no session.
func (w *writtenWorld) callers() []writeCaller {
	return []writeCaller{
		{"owner", w.owner, w.as(w.owner)},
		{"moderator", w.moderator, w.as(w.moderator)},
		{"other-organization", w.outsider, w.as(w.outsider)},
		{"no-session", "", auth.ContextWithAnonymousActor(context.Background())},
	}
}

// refusalFor is the refusal the engine owes a caller's write.
func refusalFor(caller string) string {
	if caller == "no-session" {
		return anonymousWriteRefusal
	}
	return serverWrittenRefusal
}

// harmfulRows is, for each concept, a row whose payload would do harm, and
// the ids it is aimed at: a new one, and the pack's own row where one exists.
func (w *writtenWorld) harmfulRows() map[string]struct {
	payload map[string]any
	ids     []string
} {
	now := time.Now().UTC()
	type target = struct {
		payload map[string]any
		ids     []string
	}
	fresh := func(kind string) string { return "srvw-" + kind + "-" + w.n }
	return map[string]target{
		// Another person's name on a review already verified and already on
		// its fifth revision, and the buyer's own review rewritten.
		"v1:reviews:review": {map[string]any{"ownerUserId": w.owner, "storeId": w.store, "accountId": w.account,
			"productHandle": w.handle, "body": "Planted praise", "authorName": "Somebody Else", "verifiedBuyer": true,
			"revision": 5, "requestId": w.requestID}, []string{fresh("review"), w.reviewID}},
		// A decision that publishes the buyer's review.
		"v1:reviews:reviewDecision": {map[string]any{"ownerUserId": w.owner, "reviewId": w.reviewRowID, "storeId": w.store,
			"accountId": w.account, "action": "publish", "revision": 1, "decidedBy": w.owner},
			[]string{fresh("decision"), strings.TrimPrefix(w.decisionRowID, "v1:reviews:reviewDecision:")}},
		// A link carrying the hash of a token the writer chose, and the
		// buyer's own link re-aimed at it.
		"v1:reviews:reviewRequest": {map[string]any{"ownerUserId": w.owner, "storeId": w.store, "accountId": w.account,
			"orderGid": "gid://shopify/Order/9", "productHandle": w.handle, "email": "planted@example.com",
			"tokenHash": reviewspack.TokenHashForTest(w.chosenToken()), "issuedAt": now.Format(time.RFC3339Nano),
			"expiresAt": now.Add(24 * time.Hour).Format(time.RFC3339Nano)}, []string{fresh("request"), w.requestID}},
		// Settings naming another address and another removal approver, at the
		// store's own settings row.
		"v1:reviews:reviewSettings": {map[string]any{"ownerUserId": w.owner, "storeId": w.store, "accountId": w.account,
			"publicDisplay": true, "notifyEmail": "planted@example.com", "removalApproverUserId": w.outsider},
			[]string{fresh("settings"), reviewspack.SettingsRowID(w.store)}},
		// A legacy moderation action hiding the buyer's review.
		"v1:reviews:moderationAction": {map[string]any{"reviewId": w.reviewRowID, "storeId": w.store, "criterion": "spam",
			"decidedBy": w.owner, "principalKind": "client"}, []string{fresh("moderation")}},
	}
}

// chosenToken is the token a planted link's hash is of.
func (w *writtenWorld) chosenToken() string {
	return "a-token-the-writer-chose-" + w.n + "-0123456789abcdefghijklmnop"
}

// ownedBy is payload with the row's owner field -- decidedBy on the legacy
// moderationAction, ownerUserId on the rest -- naming self.
func ownedBy(concept string, payload map[string]any, self string) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	if concept == "v1:reviews:moderationAction" {
		out["decidedBy"] = self
	} else {
		out["ownerUserId"] = self
	}
	return out
}

// EVERY CONCEPT THE PACK DECLARES refuses a raw insert from every caller,
// whether the row is new or the pack's own, and whether it names the
// merchant as its owner or the caller themselves; no version is written.
func TestARawWriteOfEveryReviewsConceptIsRefused(t *testing.T) {
	w := newWrittenWorld(t, "srvw-raw")
	for concept, row := range w.harmfulRows() {
		for _, caller := range w.callers() {
			variants := map[string]map[string]any{"the merchant's": row.payload}
			if caller.self != "" {
				variants["the caller's own"] = ownedBy(concept, row.payload, caller.self)
			}
			for owned, payload := range variants {
				for _, id := range row.ids {
					before := versionsOf(t, concept, id)
					_, err := w.eng.Execute(caller.ctx, rawReviewsInsert(concept, id, payload))
					want := refusalFor(caller.name)
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Errorf("OPEN: %s raw insert by the %s at %s, owned as %s, answered %v, want %q",
							concept, caller.name, id, owned, err, want)
					}
					if after := versionsOf(t, concept, id); after != before {
						t.Errorf("WROTE: %s raw insert by the %s at %s, owned as %s, wrote %d version(s)",
							concept, caller.name, id, owned, after-before)
					}
				}
			}
		}
	}
}

// THE PLANTED ROWS, REFUSED, CHANGE NOTHING THE PACK SHOWS, each probed on its
// own by the store's owner and by its moderator: a decision that publishes
// publishes nothing, the buyer's review keeps the buyer's words, a link
// carrying a chosen token opens nothing, and the store's settings name no
// planted address.
func TestARefusedPlantChangesNothingThePackShows(t *testing.T) {
	w := newWrittenWorld(t, "srvw-plant")
	rows := w.harmfulRows()
	plant := func(concept string) {
		for _, caller := range w.callers()[:2] {
			for _, id := range rows[concept].ids {
				_, _ = w.eng.Execute(caller.ctx, rawReviewsInsert(concept, id, rows[concept].payload))
			}
		}
	}

	plant("v1:reviews:reviewDecision")
	shown := payloadOf(t, w.eng, asMerchant(w.owner),
		`builtin reviewsPublishedForProduct(storeId: `+quote(w.store)+`, productHandle: `+quote(w.handle)+`)`)
	if shown["count"] != float64(0) {
		t.Errorf("a planted decision published the buyer's review: the public read shows %v", shown["count"])
	}

	plant("v1:reviews:review")
	if got := latestOf(t, "v1:reviews:review", w.reviewID); got["body"] != "first words" || got["authorName"] != "Dana" {
		t.Errorf("a planted row rewrote the buyer's review: %v", got)
	}

	plant("v1:reviews:reviewRequest")
	if rec := postReviewForm(t, w.eng, w.site, w.store, w.owner, reviewForm(w.chosenToken(), w.handle, "through a planted link")); rec.Header().Get("Location") != "/reviews/problem?reason=link_invalid" {
		t.Errorf("a planted link opened: the chosen token's post answered %d to %q, want link_invalid",
			rec.Code, rec.Header().Get("Location"))
	}

	plant("v1:reviews:reviewSettings")
	settings := payloadOf(t, w.eng, w.as(w.moderator), `builtin reviewsSettings(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+`)`)
	values, _ := settings["settings"].(map[string]any)
	if asText(values["notifyEmail"]) != "" || asText(values["removalApproverUserId"]) != "" {
		t.Errorf("planted settings now name %v / %v", values["notifyEmail"], values["removalApproverUserId"])
	}
}

// NO MUTATION OF THE PACK IS CALLABLE BY NAME by any caller with arguments of
// their choosing, and none writes the row it names.
func TestNoReviewsMutationIsCallableByName(t *testing.T) {
	w := newWrittenWorld(t, "srvw-named")
	now := time.Now().UTC()
	at := func(d time.Duration) string { return quote(now.Add(d).Format(time.RFC3339Nano)) }
	fresh := "srvw-named-" + w.n
	calls := []struct {
		mutation, concept, id, call string
	}{
		{"createReview", "v1:reviews:review", fresh + "-create",
			`createReview(reviewId: ` + quote(fresh+"-create") + `, storeId: ` + quote(w.store) + `, productHandle: ` + quote(w.handle) +
				`, body: "Planted praise", authorName: "Somebody Else")`},
		{"submitReview", "v1:reviews:review", fresh + "-submit",
			`submitReview(submissionId: ` + quote(fresh+"-submit") + `, storeId: ` + quote(w.store) + `, siteId: ` + quote(w.site) +
				`, productHandle: ` + quote(w.handle) + `, body: "Planted praise", authorName: "Somebody Else")`},
		{"putReview", "v1:reviews:review", w.reviewID,
			`putReview(reviewId: ` + quote(w.reviewID) + `, storeId: ` + quote(w.store) + `, productHandle: ` + quote(w.handle) +
				`, body: "Planted praise", requestId: ` + quote(w.requestID) + `, verifiedBuyer: true, revision: 9)`},
		{"appendReviewDecision", "v1:reviews:reviewDecision", fresh + "-decision",
			`appendReviewDecision(decisionId: ` + quote(fresh+"-decision") + `, ownerUserId: ` + quote(w.owner) +
				`, reviewId: ` + quote(w.reviewRowID) + `, storeId: ` + quote(w.store) + `, action: "publish", revision: 1, decidedBy: ` +
				quote(w.owner) + `)`},
		{"putReviewRequest", "v1:reviews:reviewRequest", fresh + "-request",
			`putReviewRequest(requestId: ` + quote(fresh+"-request") + `, storeId: ` + quote(w.store) +
				`, orderGid: "gid://shopify/Order/9", productHandle: ` + quote(w.handle) + `, email: "planted@example.com", tokenHash: ` +
				quote(reviewspack.TokenHashForTest(w.chosenToken())) + `, issuedAt: ` + at(0) + `, expiresAt: ` + at(24*time.Hour) + `)`},
		{"putReviewSettings", "v1:reviews:reviewSettings", reviewspack.SettingsRowID(w.store),
			`putReviewSettings(settingsId: ` + quote(reviewspack.SettingsRowID(w.store)) + `, ownerUserId: ` + quote(w.owner) +
				`, storeId: ` + quote(w.store) + `, publicDisplay: true, notifyEmail: "planted@example.com")`},
		{"setReviewDisplay", "v1:reviews:reviewSettings", reviewspack.SettingsRowID(w.store),
			`setReviewDisplay(settingsId: ` + quote(reviewspack.SettingsRowID(w.store)) + `, storeId: ` + quote(w.store) +
				`, publicDisplay: false)`},
	}
	for _, c := range calls {
		for _, caller := range w.callers() {
			before := versionsOf(t, c.concept, c.id)
			_, err := w.eng.Execute(caller.ctx, c.call)
			if err == nil {
				t.Errorf("OPEN: %s by the %s was accepted", c.mutation, caller.name)
			} else {
				t.Logf("refused: %s by the %s: %v", c.mutation, caller.name, err)
			}
			if after := versionsOf(t, c.concept, c.id); after != before {
				t.Errorf("WROTE: %s by the %s wrote %d version(s) of %s", c.mutation, caller.name, after-before, c.id)
			}
		}
	}
}

// A BUYER'S REVIEW AND ITS EDIT THROUGH THE FORM ROUTE: the first post is a
// verified review owned by the merchant in the store's organization; declined,
// the same link edits it into the next revision of the same row, and the edit
// is back in the queue.
func TestTheFormRouteWritesAReviewAndAnEditReturnsItToTheQueue(t *testing.T) {
	w := newWrittenWorld(t, "srvw-form")
	first := latestOf(t, "v1:reviews:review", w.reviewID)
	if first["verifiedBuyer"] != true || first["revision"] != float64(1) || first["body"] != "first words" ||
		first["ownerUserId"] != w.owner || bareAccount(asText(first["accountId"])) != w.account || first["storeId"] != w.store ||
		first["siteId"] != w.site || first["authorName"] != "Dana" {
		t.Fatalf("the first review is %v", first)
	}
	if _, err := w.eng.Execute(w.as(w.moderator),
		`builtin reviewsDecide(reviewId: `+quote(w.reviewRowID)+`, action: "decline", reason: "unrelated", revision: 1)`); err != nil {
		t.Fatalf("decline: %v", err)
	}
	declined := payloadOf(t, w.eng, w.as(w.moderator), `builtin reviewsList(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+`)`)
	if declined["total"] != float64(0) {
		t.Fatalf("a declined review is still waiting: %v", declined)
	}

	redirectedTo(t, postReviewForm(t, w.eng, w.site, w.store, w.owner, reviewForm(w.token, w.handle, "second thoughts")),
		"/reviews/thank-you")
	if got := versionsOf(t, "v1:reviews:review", w.reviewID); got != 2 {
		t.Fatalf("the review has %d versions, want the edit as the second", got)
	}
	edited := latestOf(t, "v1:reviews:review", w.reviewID)
	if edited["revision"] != float64(2) || edited["body"] != "second thoughts" || edited["verifiedBuyer"] != true {
		t.Fatalf("the edit is %v", edited)
	}
	queue := payloadOf(t, w.eng, w.as(w.moderator), `builtin reviewsList(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+`)`)
	list, _ := queue["reviews"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != w.reviewRowID || list[0].(map[string]any)["revision"] != float64(2) {
		t.Fatalf("the queue after the edit is %v, want the review waiting at revision 2", queue)
	}
}

// EACH REFUSAL OF THE FORM keeps its code and its redirect, and writes nothing.
func TestTheFormRouteRefusesByNameAndWritesNothing(t *testing.T) {
	w := newWrittenWorld(t, "srvw-refuse")
	post := func(form url.Values) *httptest.ResponseRecorder {
		return postReviewForm(t, w.eng, w.site, w.store, w.owner, form)
	}
	before := versionsOf(t, "v1:reviews:review", w.reviewID)

	// invalid: a required field missing is the surface's own refusal.
	missing := reviewForm(w.token, w.handle, "")
	missing.Del("body")
	redirectedTo(t, post(missing), "/reviews/problem?reason=invalid")
	// link_invalid: a token that is no link of this store.
	redirectedTo(t, post(reviewForm("forged-"+w.n, w.handle, "nope")), "/reviews/problem?reason=link_invalid")
	// link_invalid: the link, for another product.
	redirectedTo(t, post(reviewForm(w.token, w.handle+"-other", "nope")), "/reviews/problem?reason=link_invalid")
	// link_expired.
	expired := w.token + "-expired"
	putRequest(t, w.eng, w.owner, w.store, w.requestID+"-expired", w.handle+"-x", expired, time.Now().Add(-time.Hour))
	redirectedTo(t, post(reviewForm(expired, w.handle+"-x", "late")), "/reviews/problem?reason=link_expired")
	if got := versionsOf(t, "v1:reviews:review", w.reviewID); got != before {
		t.Fatalf("a refused post wrote the review (%d versions, had %d)", got, before)
	}

	// link_used: published by the store's moderator through the builtin, the
	// link is closed.
	if _, err := w.eng.Execute(w.as(w.moderator),
		`builtin reviewsDecide(reviewId: `+quote(w.reviewRowID)+`, action: "publish", revision: 1)`); err != nil {
		t.Fatalf("publish: %v", err)
	}
	redirectedTo(t, post(reviewForm(w.token, w.handle, "too late")), "/reviews/problem?reason=link_used")
	if got := versionsOf(t, "v1:reviews:review", w.reviewID); got != before {
		t.Fatalf("a post through a used link wrote the review (%d versions, had %d)", got, before)
	}
	shown := payloadOf(t, w.eng, asMerchant(w.owner),
		`builtin reviewsPublishedForProduct(storeId: `+quote(w.store)+`, productHandle: `+quote(w.handle)+`)`)
	if shown["count"] != float64(1) {
		t.Fatalf("the published review is not shown: %v", shown)
	}
}

// A SETTINGS SAVE THROUGH THE PACK'S BUILTIN still writes the store's one row.
func TestASettingsSaveThroughTheBuiltinStillWrites(t *testing.T) {
	w := newWrittenWorld(t, "srvw-settings")
	before := versionsOf(t, "v1:reviews:reviewSettings", reviewspack.SettingsRowID(w.store))
	if _, err := w.eng.Execute(w.as(w.moderator),
		`builtin reviewsUpdateSettings(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+`, notifyEmail: "moderator@example.com")`); err != nil {
		t.Fatalf("the moderator's settings save: %v", err)
	}
	if after := versionsOf(t, "v1:reviews:reviewSettings", reviewspack.SettingsRowID(w.store)); after != before+1 {
		t.Fatalf("the save wrote %d versions, want one", after-before)
	}
	got := latestOf(t, "v1:reviews:reviewSettings", reviewspack.SettingsRowID(w.store))
	if got["notifyEmail"] != "moderator@example.com" || got["publicDisplay"] != true || got["ownerUserId"] != w.owner {
		t.Fatalf("the saved settings are %v", got)
	}
}


// bareAccount is an organization id without its canonical prefix.
func bareAccount(id string) string { return strings.TrimPrefix(id, "v1:accounts:account:") }
