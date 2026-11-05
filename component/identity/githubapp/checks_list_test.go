package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fixedClockClient is a test client whose clock reads one fixed instant, so
// a listing's request and its pagination do not depend on when it ran.
func fixedClockClient(t *testing.T, hub *fakeHub) *Client {
	t.Helper()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	return testClient(t, hub, &now)
}

// A driver taking over a run, a cancel and recovery find the step checks a
// run already has by listing the commit's check runs: every attempt, across
// pages, with each one's external id.
func TestCheckRunsForCommitListsEveryPage(t *testing.T) {
	hub := newHub().on("/repos/acme/widget/commits/abc123/check-runs", func(r *http.Request) (int, string) {
		q := r.URL.Query()
		if q.Get("filter") != "all" || q.Get("per_page") != "100" {
			return http.StatusBadRequest, `{"message":"unexpected query ` + r.URL.RawQuery + `"}`
		}
		var runs []string
		start := 1
		if q.Get("page") == "2" {
			start = 101
		}
		count := 100
		if q.Get("page") == "2" {
			count = 3
		}
		for i := start; i < start+count; i++ {
			runs = append(runs, fmt.Sprintf(`{"id":%d,"name":"MemQL / memql / tests / go-unit #%d","external_id":"run1:tests.go-unit#%d","status":"in_progress","app":{"id":4822538}}`, i, i, i))
		}
		return http.StatusOK, `{"total_count":103,"check_runs":[` + strings.Join(runs, ",") + `]}`
	})
	got, err := fixedClockClient(t, hub).CheckRunsForCommit(context.Background(), "tok", "acme", "widget", "abc123")
	if err != nil {
		t.Fatalf("CheckRunsForCommit: %v", err)
	}
	if len(got) != 103 || got[0].ID != 1 || got[0].ExternalID != "run1:tests.go-unit#1" || got[0].Status != "in_progress" || got[0].AppID != 4822538 || got[102].ID != 103 {
		t.Fatalf("CheckRunsForCommit = %d runs, first %+v", len(got), got[0])
	}
}
