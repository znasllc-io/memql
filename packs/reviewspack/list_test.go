package reviewspack

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// list_test.go -- THE MODERATION LIST (reviewsList).
//
// The screen a moderator works from: one store's reviews, each with the state
// the decision log gives it and the last decision recorded. The queue (state
// waiting) is oldest first, because a review's wait is what the store's
// timeline is measured against; every other view is newest first.

type listReader struct {
	reviews   []map[string]any
	decisions []map[string]any
	asked     map[string]map[string]string
}

func (l *listReader) Rows(_ context.Context, query string, args map[string]string) ([]map[string]any, error) {
	if l.asked == nil {
		l.asked = map[string]map[string]string{}
	}
	l.asked[query] = args
	if query == "reviewsInStore" {
		return l.reviews, nil
	}
	return nil, nil
}

func (l *listReader) RowsWithList(_ context.Context, query string, _ map[string]string, _ string, list []string) ([]map[string]any, error) {
	if query != "reviewDecisionsForReviews" {
		return nil, nil
	}
	within := map[string]bool{}
	for _, id := range list {
		within[canonicalReviewID(id)] = true
	}
	var out []map[string]any
	for _, d := range l.decisions {
		if within[canonicalReviewID(asString(d["reviewId"]))] {
			out = append(out, d)
		}
	}
	return out, nil
}

func listRow(id string, minutes int) map[string]any {
	return map[string]any{"id": id, "storeId": "store-live", "productHandle": "boot", "body": "b",
		"authorEmail": "w@example.com", "revision": 1,
		"createdAt": t0.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339Nano)}
}

type listed struct {
	Reviews []struct {
		ID           string `json:"id"`
		State        string `json:"state"`
		AuthorEmail  string `json:"authorEmail"`
		LastDecision *struct {
			Action    string `json:"action"`
			Reason    string `json:"reason"`
			DecidedBy string `json:"decidedBy"`
		} `json:"lastDecision"`
	} `json:"reviews"`
	Count int    `json:"count"`
	Total int    `json:"total"`
	Next  string `json:"next"`
}

func list(t *testing.T, r *listReader, args map[string]any) listed {
	t.Helper()
	nodes, err := (&Provider{reader: r}).list(context.Background(), args, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var out listed
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func ids(l listed) []string {
	out := []string{}
	for _, r := range l.Reviews {
		out = append(out, r.ID)
	}
	return out
}

func TestTheQueueIsWaitingReviewsOldestFirst(t *testing.T) {
	r := &listReader{
		reviews:   []map[string]any{listRow("newer", 60), listRow("older", 0), listRow("shown", 30)},
		decisions: []map[string]any{decisionRow("shown", ActionPublish, 1, 0)},
	}
	got := list(t, r, map[string]any{"storeId": "store-live"})
	if want := []string{"older", "newer"}; len(got.Reviews) != 2 || ids(got)[0] != want[0] || ids(got)[1] != want[1] {
		t.Fatalf("queue = %v, want %v (waiting only, oldest first)", ids(got), want)
	}
	if got.Reviews[0].State != "waiting" || got.Reviews[0].AuthorEmail == "" {
		t.Fatalf("row = %+v: a moderator sees the state and the writer's email", got.Reviews[0])
	}
}

func TestEveryOtherViewIsNewestFirstWithItsLastDecision(t *testing.T) {
	r := &listReader{
		reviews: []map[string]any{listRow("a", 0), listRow("b", 10)},
		decisions: []map[string]any{
			decisionRow("a", ActionPublish, 1, 0),
			decisionRow("b", ActionPublish, 1, 1),
		},
	}
	got := list(t, r, map[string]any{"storeId": "store-live", "state": "published"})
	if len(got.Reviews) != 2 || ids(got)[0] != "b" {
		t.Fatalf("published = %v, want newest first", ids(got))
	}
	if got.Reviews[0].LastDecision == nil || got.Reviews[0].LastDecision.Action != "publish" ||
		got.Reviews[0].LastDecision.DecidedBy != "moderator-1" {
		t.Fatalf("lastDecision = %+v", got.Reviews[0].LastDecision)
	}
	all := list(t, r, map[string]any{"storeId": "store-live", "state": "all"})
	if all.Total != 2 {
		t.Fatalf("all: total = %d, want 2", all.Total)
	}
}

func TestTheListIsPagedWithACursor(t *testing.T) {
	rows := []map[string]any{}
	for i := 0; i < 5; i++ {
		rows = append(rows, listRow(string(rune('a'+i)), i))
	}
	r := &listReader{reviews: rows}
	first := list(t, r, map[string]any{"storeId": "store-live", "limit": 2})
	if first.Count != 2 || first.Total != 5 || first.Next == "" {
		t.Fatalf("first page = %+v", first)
	}
	second := list(t, r, map[string]any{"storeId": "store-live", "limit": 2, "after": first.Next})
	if second.Count != 2 || ids(second)[0] != "c" {
		t.Fatalf("second page = %v, want it to start at c", ids(second))
	}
}

func TestTheListRefusesAnUnknownStateOrNoStore(t *testing.T) {
	p := &Provider{reader: &listReader{}}
	if _, err := p.list(context.Background(), map[string]any{"storeId": "store-live", "state": "hidden"}, 0); err == nil {
		t.Fatal("an unknown state was accepted")
	}
	if _, err := p.list(context.Background(), map[string]any{}, 0); err == nil {
		t.Fatal("a list with no store was accepted")
	}
}
