package shopify

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// admin_test.go -- the cost-aware client (#4391).
//
// The Admin API is limited by a leaky bucket of query-cost points, not by
// requests. A client that ignores the bucket discovers the limit by being
// refused, and a backfill that discovers it that way turns a five-minute job
// into an hour of retries.

func TestTheClientPacesOnTheReportedBucket(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	ctx := context.Background()
	h.admin.reply("Probe", map[string]any{"shop": map[string]any{"id": "gid://shopify/Shop/1"}})

	// First call: no bucket is known yet, so nothing is waited for.
	if _, err := h.conn.adminCall(ctx, store, "query Probe { shop { id } }", "Probe", nil); err != nil {
		t.Fatal(err)
	}
	if len(h.slept) != 0 {
		t.Fatalf("waited %v before learning the bucket", h.slept)
	}

	// The response said the bucket is nearly empty. The NEXT call waits
	// before going out, rather than after being refused.
	h.admin.setThrottle(ThrottleStatus{MaximumAvailable: 2000, CurrentlyAvailable: 20, RestoreRate: 100})
	if _, err := h.conn.adminCall(ctx, store, "query Probe { shop { id } }", "Probe", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.conn.adminCall(ctx, store, "query Probe { shop { id } }", "Probe", nil); err != nil {
		t.Fatal(err)
	}
	if len(h.slept) == 0 {
		t.Fatal("the client did not wait with 20 of 2000 points available")
	}
	// (100 - 20) / 100 points per second = 0.8s.
	if got := h.slept[len(h.slept)-1]; got < 700*time.Millisecond || got > time.Second {
		t.Errorf("waited %v, want about 800ms of restore", got)
	}
}

func TestTheClientDoesNotWaitWithAFullBucket(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("Probe", map[string]any{"shop": map[string]any{"id": "x"}})
	h.admin.setThrottle(ThrottleStatus{MaximumAvailable: 2000, CurrentlyAvailable: 1900, RestoreRate: 100})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := h.conn.adminCall(ctx, h.store(t), "query Probe { shop { id } }", "Probe", nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.slept) != 0 {
		t.Errorf("waited %v with a full bucket", h.slept)
	}
}

func TestAThrottledErrorRetries(t *testing.T) {
	h := newHarness(t)
	h.admin.graphQLError("Probe", "THROTTLED", "Throttled")
	h.conn.admin.MaxRetries = 2
	_, err := h.conn.adminCall(context.Background(), h.store(t), "query Probe { shop { id } }", "Probe", nil)
	if err == nil {
		t.Fatal("expected the retries to be exhausted")
	}
	if got := h.admin.countOp("Probe"); got != 3 {
		t.Errorf("called %d times, want the initial try plus two retries", got)
	}
}

// A validation error, a missing scope and a 404 fail identically forever.
// Retrying them is how a backfill never finishes.
func TestANonRetryableErrorIsNotRetried(t *testing.T) {
	h := newHarness(t)
	h.admin.graphQLError("Probe", "ACCESS_DENIED", "Required access: read_orders")
	_, err := h.conn.adminCall(context.Background(), h.store(t), "query Probe { shop { id } }", "Probe", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := h.admin.countOp("Probe"); got != 1 {
		t.Errorf("called %d times; ACCESS_DENIED will never start succeeding", got)
	}
}

func TestA429HonoursRetryAfter(t *testing.T) {
	if d := retryAfter("2.5"); d != 2500*time.Millisecond {
		t.Errorf("numeric Retry-After = %v", d)
	}
	if d := retryAfter(""); d != 0 {
		t.Errorf("absent Retry-After = %v", d)
	}
	if d := retryAfter("not a number"); d != 0 {
		t.Errorf("unparseable Retry-After = %v", d)
	}
}

func TestAdminErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		err       *AdminError
		retryable bool
	}{
		{"429", &AdminError{StatusCode: http.StatusTooManyRequests}, true},
		{"500", &AdminError{StatusCode: http.StatusInternalServerError}, true},
		{"throttled", &AdminError{StatusCode: 200, Errors: []GraphQLError{{Extensions: map[string]any{"code": "THROTTLED"}}}}, true},
		{"validation", &AdminError{StatusCode: 200, Errors: []GraphQLError{{Extensions: map[string]any{"code": "GRAPHQL_VALIDATION_FAILED"}}}}, false},
		{"404", &AdminError{StatusCode: http.StatusNotFound}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Retryable(); got != tc.retryable {
				t.Errorf("Retryable() = %v, want %v", got, tc.retryable)
			}
		})
	}
}

func TestTheAdminEndpointIsComposedFromTheStore(t *testing.T) {
	got := AdminEndpoint("acme.myshopify.com", "2026-07")
	if got != "https://acme.myshopify.com/admin/api/2026-07/graphql.json" {
		t.Errorf("endpoint = %q", got)
	}
}

func TestTheStoreTokenIsSentAndNeverTheStoreRow(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("Probe", map[string]any{"shop": map[string]any{"id": "x"}})
	if _, err := h.conn.adminCall(context.Background(), h.store(t), "query Probe { shop { id } }", "Probe", nil); err != nil {
		t.Fatal(err)
	}
	seen := h.admin.seen()
	if len(seen) != 1 || seen[0].Token != "shpat_test" {
		t.Fatalf("token = %q, want the resolved secret", seen[0].Token)
	}
}
