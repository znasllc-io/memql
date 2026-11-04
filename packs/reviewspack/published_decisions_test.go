package reviewspack

import (
	"encoding/json"
	"testing"
)

// published_decisions_test.go -- THE PUBLIC READ SHOWS ONLY WHAT A PERSON
// PUBLISHED.
//
// Until the pack had a pending state, a review was public the moment it was
// stored. Now a review is shown only when a publish decision covers the
// revision it holds, and the read carries the two facts a storefront draws
// from it: whether the writer was a verified buyer, and the product's score
// over every shown review.

func liveReader(reviews []map[string]any, decisions []map[string]any) *fixtureReader {
	return &fixtureReader{
		settings:  map[string][]map[string]any{"store-live": {{"publicDisplay": true}}},
		reviews:   map[string][]map[string]any{"store-live|boot": reviews},
		decisions: map[string][]map[string]any{"store-live": decisions},
	}
}

func liveRead(t *testing.T, eng *fixtureReader, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"storeId": "store-live", "productHandle": "boot"}
	for k, v := range extra {
		args[k] = v
	}
	return publishedFrom(t, &Provider{reader: eng}, args)
}

func idsOf(t *testing.T, out map[string]any) []string {
	t.Helper()
	list, _ := out["reviews"].([]any)
	ids := make([]string, 0, len(list))
	for _, r := range list {
		ids = append(ids, r.(map[string]any)["id"].(string))
	}
	return ids
}

func TestAWaitingReviewIsNotPublic(t *testing.T) {
	out := liveRead(t, liveReader(reviewRows("r1"), nil), nil)
	if out["count"].(float64) != 0 {
		t.Fatalf("count = %v, want 0 -- a review nobody has published must not reach a storefront", out["count"])
	}
}

func TestOnlyPublishedReviewsArePublic(t *testing.T) {
	eng := liveReader(reviewRows("waiting", "shown", "declined", "removed", "erased"), []map[string]any{
		decisionRow("shown", ActionPublish, 1, 0),
		decisionRow("declined", ActionDecline, 1, 0),
		decisionRow("removed", ActionPublish, 1, 0),
		decisionRow("removed", ActionRemove, 1, 9),
		decisionRow("erased", ActionPublish, 1, 0),
		decisionRow("erased", ActionErase, 1, 9),
	})
	got := idsOf(t, liveRead(t, eng, nil))
	if len(got) != 1 || got[0] != "shown" {
		t.Fatalf("shown = %v, want only [shown]", got)
	}
}

// AN EDIT AFTER A PUBLISH IS WORDS NOBODY READ. The link closes on publish, so
// this is a race; the read fails closed and shows nothing.
func TestARevisionNoPublishCoversIsNotPublic(t *testing.T) {
	rows := reviewRows("r1")
	rows[0]["revision"] = 2
	out := liveRead(t, liveReader(rows, []map[string]any{decisionRow("r1", ActionPublish, 1, 0)}), nil)
	if out["count"].(float64) != 0 {
		t.Fatalf("count = %v, want 0 -- revision 2 was never published", out["count"])
	}
}

// "VERIFIED BUYER" IS THE ENGINE'S WORD. Only a review written through a
// buyer's link carries it, and only the boolean true is sent.
func TestVerifiedBuyerIsProjectedOnlyWhenStamped(t *testing.T) {
	rows := reviewRows("verified", "plain")
	rows[0]["verifiedBuyer"] = true
	out := liveRead(t, liveReader(rows, published("verified", "plain")), nil)
	for _, r := range out["reviews"].([]any) {
		m := r.(map[string]any)
		v, present := m["verifiedBuyer"]
		switch m["id"] {
		case "verified":
			if v != true {
				t.Fatalf("verified review: verifiedBuyer = %v, want true", v)
			}
		case "plain":
			if present {
				t.Fatalf("unverified review carries verifiedBuyer = %v; it must be absent", v)
			}
		}
	}
}

// THE SCORE IS OVER EVERY SHOWN REVIEW, NOT THE PAGE. A product with 300
// reviews shows 50 on a page and must still be scored on 300.
func TestTheSummaryIsOverEveryShownReview(t *testing.T) {
	rows := reviewRows("a", "b", "c", "d", "hidden")
	rows[0]["rating"] = 5
	rows[1]["rating"] = 4
	delete(rows[2], "rating") // a review may carry no rating
	rows[3]["rating"] = 4
	rows[4]["rating"] = 1 // waiting: not shown, not scored
	eng := liveReader(rows, published("a", "b", "c", "d"))
	out := liveRead(t, eng, map[string]any{"limit": 2})

	if out["count"].(float64) != 2 {
		t.Fatalf("count = %v, want the 2 asked for", out["count"])
	}
	raw, _ := json.Marshal(out["summary"])
	var s struct {
		Average     float64        `json:"average"`
		RatingCount int            `json:"ratingCount"`
		ReviewCount int            `json:"reviewCount"`
		Stars       map[string]int `json:"stars"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("summary: %v (%s)", err, raw)
	}
	if s.ReviewCount != 4 || s.RatingCount != 3 {
		t.Fatalf("reviewCount=%d ratingCount=%d, want 4 and 3", s.ReviewCount, s.RatingCount)
	}
	if s.Average != 4.33 {
		t.Fatalf("average = %v, want 4.33 (13/3, to two places)", s.Average)
	}
	want := map[string]int{"1": 0, "2": 0, "3": 0, "4": 2, "5": 1}
	for k, v := range want {
		got, ok := s.Stars[k]
		if !ok || got != v {
			t.Fatalf("stars = %v, want every key 1-5 with %v", s.Stars, want)
		}
	}
}

func TestNoSummaryWhenNothingShownIsRated(t *testing.T) {
	rows := reviewRows("a")
	delete(rows[0], "rating")
	out := liveRead(t, liveReader(rows, published("a")), nil)
	if _, present := out["summary"]; present {
		t.Fatalf("summary = %v, want none: a storefront must not draw a score from no ratings", out["summary"])
	}
}

func TestNoSummaryWhileDisplayIsOff(t *testing.T) {
	eng := liveReader(reviewRows("a"), published("a"))
	eng.settings["store-live"] = []map[string]any{{"publicDisplay": false}}
	out := liveRead(t, eng, nil)
	if _, present := out["summary"]; present || out["count"].(float64) != 0 {
		t.Fatalf("display off answered %v", out)
	}
}

// NEWEST FIRST, AND THE PAGE IS THE NEWEST. The query answers the whole set
// unordered (a sorted query would carry the implicit row cap), so the order is
// the read's own.
func TestThePublicReadIsNewestFirstAndCutsTheOldest(t *testing.T) {
	rows := reviewRows("old", "newest", "middle")
	rows[0]["createdAt"] = "2026-11-01T10:00:00Z"
	rows[1]["createdAt"] = "2026-11-03T10:00:00Z"
	rows[2]["createdAt"] = "2026-11-02T10:00:00Z"
	out := liveRead(t, liveReader(rows, published("old", "newest", "middle")), map[string]any{"limit": 2})
	got := idsOf(t, out)
	if len(got) != 2 || got[0] != "newest" || got[1] != "middle" {
		t.Fatalf("page = %v, want [newest middle]", got)
	}
	if out["summary"].(map[string]any)["reviewCount"].(float64) != 3 {
		t.Fatalf("summary = %v, want it over all three", out["summary"])
	}
}
