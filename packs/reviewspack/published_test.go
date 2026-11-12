package reviewspack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// fixtureReader answers the pack's own queries from fixtures, so the gate,
// the exclusion and the bound are exercised as decisions over rows rather
// than over an engine envelope.
type fixtureReader struct {
	settings   map[string][]map[string]any // storeId -> rows
	reviews    map[string][]map[string]any // storeId|handle -> rows
	moder      map[string][]map[string]any // storeId -> legacy moderationAction rows
	decisions  map[string][]map[string]any // storeId -> reviewDecision rows
	asked      []string
	askedAbout map[string][]string // query -> the ids it was asked about
}

func (f *fixtureReader) Rows(_ context.Context, query string, args map[string]string) ([]map[string]any, error) {
	f.asked = append(f.asked, query)
	switch query {
	case "reviewSettingsForStore":
		return f.settings[args["storeId"]], nil
	case "publishedReviewsForProduct":
		return f.reviews[args["storeId"]+"|"+args["productHandle"]], nil
	}
	return nil, nil
}

// RowsWithList records the ids each list-bounded read was ASKED ABOUT, which
// is what makes "bounded by the caller's list" assertable rather than claimed.
func (f *fixtureReader) RowsWithList(_ context.Context, query string, args map[string]string,
	_ string, list []string) ([]map[string]any, error) {
	f.asked = append(f.asked, query)
	if f.askedAbout == nil {
		f.askedAbout = map[string][]string{}
	}
	f.askedAbout[query] = append(f.askedAbout[query], list...)
	var source []map[string]any
	switch query {
	case "moderationActionsForReviews":
		source = f.moder[args["storeId"]]
	case "reviewDecisionsForReviews":
		source = f.decisions[args["storeId"]]
	default:
		return nil, nil
	}
	// THE QUERY MATCHES THE STORED SPELLING. Decisions are stored at the
	// full review id, so the fixture compares both sides in that form; the
	// legacy moderation rows are compared as they come.
	norm := func(id string) string { return id }
	if query == "reviewDecisionsForReviews" {
		norm = canonicalReviewID
	}
	var out []map[string]any
	within := map[string]struct{}{}
	for _, id := range list {
		within[norm(id)] = struct{}{}
	}
	for _, row := range source {
		id, _ := row["reviewId"].(string)
		if _, ok := within[norm(id)]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// decisionRow is one reviewDecision row as the pack's query projects it.
func decisionRow(reviewID, action string, revision, second int) map[string]any {
	return map[string]any{
		"id": "d-" + reviewID + "-" + action, "reviewId": reviewID, "action": action, "revision": revision,
		"decidedBy": "moderator-1", "createdAt": t0.Add(time.Duration(second) * time.Second).Format(time.RFC3339Nano),
	}
}

// published gives each id a publish decision on revision 1: what a person
// records when they have read a review and let it go up.
func published(ids ...string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for i, id := range ids {
		out = append(out, decisionRow(id, ActionPublish, 1, i))
	}
	return out
}

func publishedFrom(t *testing.T, p *Provider, args map[string]any) map[string]any {
	t.Helper()
	nodes, err := p.publishedForProduct(context.Background(), args, 0)
	if err != nil {
		t.Fatalf("publishedForProduct: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1 -- a builtin replies with one node", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return out
}

func reviewRows(ids ...string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{
			"id": id, "productHandle": "boot", "body": "Great boots",
			"authorName": "Sam", "authorEmail": "sam@example.com", "rating": 5,
		})
	}
	return out
}

// THE GATE IS FIRST AND IT IS FAIL-CLOSED. A store whose merchant has never
// touched the setting has not asked for reviews to be public.
func TestAbsentSettingsPublishNothing(t *testing.T) {
	p := &Provider{reader: &fixtureReader{
		reviews: map[string][]map[string]any{"store-live|boot": reviewRows("r1", "r2")},
	}}
	out := publishedFrom(t, p, map[string]any{"storeId": "store-live", "productHandle": "boot"})
	if out["count"].(float64) != 0 {
		t.Fatalf("count = %v, want 0 -- an absent settings row must not publish reviews", out["count"])
	}
}

func TestPublicDisplayOffPublishesNothingAndReadsNoReviews(t *testing.T) {
	eng := &fixtureReader{
		settings: map[string][]map[string]any{"store-live": {{"publicDisplay": false}}},
		reviews:  map[string][]map[string]any{"store-live|boot": reviewRows("r1")},
	}
	out := publishedFrom(t, &Provider{reader: eng}, map[string]any{"storeId": "store-live", "productHandle": "boot"})
	if out["count"].(float64) != 0 {
		t.Fatalf("count = %v, want 0", out["count"])
	}
	for _, q := range eng.asked {
		if q == "publishedReviewsForProduct" {
			t.Fatal("the gate must run FIRST: a store with reviews off must not read every review " +
				"and then discard them")
		}
	}
}

// A MODERATION DECISION HIDES. Any criterion, because every member of the
// closed set is a reason to take a review down and there is no approve.
func TestAModeratedReviewIsExcluded(t *testing.T) {
	p := &Provider{reader: &fixtureReader{
		settings: map[string][]map[string]any{"store-live": {{"publicDisplay": true}}},
		reviews:  map[string][]map[string]any{"store-live|boot": reviewRows("r1", "r2", "r3")},
		moder:    map[string][]map[string]any{"store-live": {{"reviewId": "r2", "criterion": "spam"}}},
		// All three were published; r2 also carries a legacy moderation row,
		// which still hides it.
		decisions: map[string][]map[string]any{"store-live": published("r1", "r2", "r3")},
	}}
	out := publishedFrom(t, p, map[string]any{"storeId": "store-live", "productHandle": "boot"})
	if out["count"].(float64) != 2 {
		t.Fatalf("count = %v, want 2", out["count"])
	}
	body, _ := json.Marshal(out["reviews"])
	if strings.Contains(string(body), `"r2"`) {
		t.Fatal("a moderated review reached the public read")
	}
}

// A ROW WRITTEN UNDER ONE storeId IS INVISIBLE UNDER ANOTHER (design D9).
// This is the assertion the whole epic turns on: a review submitted while
// previewing a storefront must not appear on the live one.
func TestAReviewUnderOneStoreIsInvisibleUnderAnother(t *testing.T) {
	eng := &fixtureReader{
		settings: map[string][]map[string]any{
			"store-live": {{"publicDisplay": true}},
			"store-dev":  {{"publicDisplay": true}},
		},
		reviews: map[string][]map[string]any{
			"store-dev|boot": reviewRows("written-under-preview"),
		},
		decisions: map[string][]map[string]any{"store-dev": published("written-under-preview")},
	}
	p := &Provider{reader: eng}

	live := publishedFrom(t, p, map[string]any{"storeId": "store-live", "productHandle": "boot"})
	if live["count"].(float64) != 0 {
		t.Fatalf("the live store sees %v reviews; a row written under preview reached shoppers",
			live["count"])
	}

	dev := publishedFrom(t, p, map[string]any{"storeId": "store-dev", "productHandle": "boot"})
	if dev["count"].(float64) != 1 {
		t.Fatalf("the development store sees %v reviews, want 1 -- the row must still be "+
			"readable where it was written", dev["count"])
	}
}

// THE PUBLIC READ MUST NOT PROJECT THE SHOPPER'S EMAIL. It exists so a
// merchant can reply, not so the internet can harvest it, and this is the
// one read whose output reaches an unauthenticated browser.
func TestThePublicReadNeverProjectsTheAuthorEmail(t *testing.T) {
	p := &Provider{reader: &fixtureReader{
		settings:  map[string][]map[string]any{"store-live": {{"publicDisplay": true}}},
		reviews:   map[string][]map[string]any{"store-live|boot": reviewRows("r1")},
		decisions: map[string][]map[string]any{"store-live": published("r1")},
	}}
	nodes, err := p.publishedForProduct(context.Background(),
		map[string]any{"storeId": "store-live", "productHandle": "boot"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(nodes[0].Payload), "sam@example.com") ||
		strings.Contains(string(nodes[0].Payload), "authorEmail") {
		t.Fatalf("the public read projected the author email: %s", nodes[0].Payload)
	}
	if !strings.Contains(string(nodes[0].Payload), "Sam") {
		t.Fatal("the public read should still carry the author's display name")
	}
}

func TestThePublicReadIsBounded(t *testing.T) {
	ids := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		ids = append(ids, string(rune('a'+i%26))+strings.Repeat("x", i%7+1))
	}
	p := &Provider{reader: &fixtureReader{
		settings:  map[string][]map[string]any{"store-live": {{"publicDisplay": true}}},
		reviews:   map[string][]map[string]any{"store-live|boot": reviewRows(ids...)},
		decisions: map[string][]map[string]any{"store-live": published(ids...)},
	}}

	def := publishedFrom(t, p, map[string]any{"storeId": "store-live", "productHandle": "boot"})
	if def["count"].(float64) != defaultPublishedLimit {
		t.Fatalf("no limit given: count = %v, want the default %d", def["count"], defaultPublishedLimit)
	}
	huge := publishedFrom(t, p, map[string]any{
		"storeId": "store-live", "productHandle": "boot", "limit": 100000})
	if huge["count"].(float64) != maxPublishedLimit {
		t.Fatalf("limit=100000: count = %v, want it clamped to %d", huge["count"], maxPublishedLimit)
	}
}

func TestTheReadRefusesWithoutAStore(t *testing.T) {
	p := &Provider{reader: &fixtureReader{}}
	if _, err := p.publishedForProduct(context.Background(),
		map[string]any{"productHandle": "boot"}, 0); err == nil {
		t.Fatal("a read with no storeId must be refused, not answered from every store")
	}
}

// The pack's declared shopper surface is what a member of the public can
// reach, and it must not grow by accident.
func TestTheDeclaredShopperSurfaceIsExactlyThreeRoutes(t *testing.T) {
	t.Cleanup(memql.ResetShopperSurfaceForTest)
	memql.ResetShopperSurfaceForTest()
	registerShopperSurface()

	surface := memql.ShopperSurface()
	if len(surface) != 3 {
		t.Fatalf("the reviews pack declares %d shopper routes, want exactly 3 "+
			"(the review form, the published read and the draft read): %+v", len(surface), surface)
	}
	form := memql.ShopperFormFor(Domain, "review")
	if form == nil || form.Construct != "reviewsSubmitFromLink" || form.Kind != memql.ShopperReadKindBuiltin {
		t.Fatalf("the review form must be the link builtin: %+v", form)
	}
	if strings.Join(form.Refusals, ",") != "link_expired,link_used,link_invalid" {
		t.Fatalf("the form declares refusals %v, want the link's three", form.Refusals)
	}
	if memql.ShopperReadFor(Domain, "published") == nil {
		t.Fatal("the published read must be declared")
	}
	if draft := memql.ShopperReadFor(Domain, "draft"); draft == nil || draft.Construct != "reviewsDraftForLink" || !draft.Private {
		t.Fatal("the draft read must be declared, and private: its answer is one buyer's own review")
	}
	if memql.ShopperReadFor(Domain, "published").Private {
		t.Fatal("the published read is the same for everyone and must stay cacheable")
	}
	// NOTHING ELSE. The pack's builtins, its mutations (and the names of the
	// ones removed) and the merchant's own lists stay unreachable.
	for _, name := range []string{"createReview", "setReviewDisplay", "recordModerationAction",
		"reviewsForStore", "moderationActionsForStore", "reviewsDecide", "reviewsList", "reviewsIssueRequest",
		"putReview", "putReviewRequest", "appendReviewDecision", "submitReview"} {
		if memql.ShopperFormFor(Domain, name) != nil || memql.ShopperReadFor(Domain, name) != nil {
			t.Fatalf("%s is reachable by the public and must not be", name)
		}
	}
}

// A STOREFRONT PACK SHIPS DISABLED (issue memql#5549).
func TestTheReviewsPackShipsDisabled(t *testing.T) {
	if DefaultEnabled {
		t.Fatal("a storefront pack must ship disabled: enabling it publishes a write endpoint " +
			"and a public read on every deployable whose shopperForms is on, which is an " +
			"operator's decision rather than an upgrade's")
	}
}

// THE MODERATION READ IS ASKED ONLY ABOUT THE REVIEWS IN PLAY. A read of the
// whole store's decisions would have to be complete or risk letting a
// moderated review through; bounding it by the caller's list is what makes
// completeness cheap.
func TestTheModerationReadIsBoundedByTheReviewsInPlay(t *testing.T) {
	eng := &fixtureReader{
		settings: map[string][]map[string]any{"store-live": {{"publicDisplay": true}}},
		reviews:  map[string][]map[string]any{"store-live|boot": reviewRows("r1", "r2")},
		moder: map[string][]map[string]any{"store-live": {
			{"reviewId": "r2", "criterion": "spam"},
			{"reviewId": "elsewhere", "criterion": "spam"},
		}},
		decisions: map[string][]map[string]any{"store-live": published("r1", "r2", "elsewhere")},
	}
	out := publishedFrom(t, &Provider{reader: eng}, map[string]any{
		"storeId": "store-live", "productHandle": "boot"})

	if out["count"].(float64) != 1 {
		t.Fatalf("count = %v, want 1", out["count"])
	}
	for _, q := range []string{"moderationActionsForReviews", "reviewDecisionsForReviews"} {
		if len(eng.askedAbout[q]) != 2 {
			t.Fatalf("%s was asked about %v, want exactly the two reviews in play", q, eng.askedAbout[q])
		}
	}
}

// A product with no reviews costs no moderation read at all.
func TestNoCandidatesCostsNoModerationRead(t *testing.T) {
	eng := &fixtureReader{
		settings: map[string][]map[string]any{"store-live": {{"publicDisplay": true}}},
	}
	publishedFrom(t, &Provider{reader: eng}, map[string]any{
		"storeId": "store-live", "productHandle": "boot"})

	for _, q := range eng.asked {
		if q == "moderationActionsForReviews" || q == "reviewDecisionsForReviews" {
			t.Fatalf("a product with no reviews must not ask about moderation (%s)", q)
		}
	}
}
