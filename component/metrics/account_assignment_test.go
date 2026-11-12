package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The counter is read off /metrics, not off the vector, so a counter that was
// declared but never put in the registry list fails here.
func TestAccountAssignmentRowsReachTheScrape(t *testing.T) {
	before := AccountAssignmentRowsValue(AccountAssignmentAssigned)
	AccountAssignmentRows(AccountAssignmentAssigned, 3)
	AccountAssignmentRows(AccountAssignmentAssigned, 0)
	if got := AccountAssignmentRowsValue(AccountAssignmentAssigned) - before; got != 3 {
		t.Fatalf("assigned moved by %v, want 3", got)
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `memql_account_assignment_rows_total{outcome="assigned"}`) {
		t.Fatalf("/metrics does not expose memql_account_assignment_rows_total")
	}
}
