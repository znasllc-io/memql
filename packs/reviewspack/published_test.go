package reviewspack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// fixtureReader answers the pack's own queries from fixtures, so the gate,
// the exclusion and the bound are exercised as decisions over rows rather
// than over an engine envelope.
type fixtureReader struct {
	settings map[string][]map[string]any // storeId -> rows
	reviews  map[string][]map[string]any // storeId|handle -> rows
	moder    map[string][]map[string]any // storeId -> rows
	asked    []string
}

func (f *fixtureReader) Rows(_ context.Context, query string, args map[string]string) ([]map[string]any, error) {
	f.asked = append(f.asked, query)
	switch query {
	case "reviewSettingsForStore":
		return f.settings[args["storeId"]], nil
	case "reviewsForProduct":
		return f.reviews[args["storeId"]+"|"+args["productHandle"]], nil
	case "moderationActionsForStore":
		return f.moder[args["storeId"]], nil
	}
	return nil, nil
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
		if q == "reviewsForProduct" {
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
		settings: map[string][]map[string]any{"store-live": {{"publicDisplay": true}}},
		reviews:  map[string][]map[string]any{"store-live|boot": reviewRows("r1")},
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
		settings: map[string][]map[string]any{"store-live": {{"publicDisplay": true}}},
		reviews:  map[string][]map[string]any{"store-live|boot": reviewRows(ids...)},
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
func TestTheDeclaredShopperSurfaceIsExactlyTwoRoutes(t *testing.T) {
	t.Cleanup(memql.ResetShopperSurfaceForTest)
	memql.ResetShopperSurfaceForTest()
	registerShopperSurface()

	surface := memql.ShopperSurface()
	if len(surface) != 2 {
		t.Fatalf("the reviews pack declares %d shopper routes, want exactly 2 "+
			"(the review form and the published read): %+v", len(surface), surface)
	}
	if memql.ShopperFormFor(Domain, "review") == nil {
		t.Fatal("the review form must be declared")
	}
	if memql.ShopperReadFor(Domain, "published") == nil {
		t.Fatal("the published read must be declared")
	}
	// NOTHING ELSE. createReview, setReviewDisplay, the moderation
	// capability and the merchant's own lists stay unreachable.
	for _, name := range []string{"createReview", "setReviewDisplay", "recordModerationAction",
		"reviewsForStore", "moderationActionsForStore"} {
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
