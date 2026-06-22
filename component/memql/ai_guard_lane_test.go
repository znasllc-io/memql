package memql

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// newTestLaneGuard builds a guard with BOTH rate buckets active and a
// controllable clock, so the per-lane isolation can be exercised in tests.
func newTestLaneGuard(interactiveMax, backgroundMax int, window time.Duration, clock *time.Time) *llmGuard {
	return &llmGuard{
		enabled:       false, // loop breaker off -- isolate the rate ceilings
		rateEnabled:   true,
		rateMax:       interactiveMax,
		rateWindow:    window,
		bgRateEnabled: true,
		bgRateMax:     backgroundMax,
		bgRateWindow:  window,
		hits:          map[string][]time.Time{},
		tripped:       map[string]time.Time{},
		now:           func() time.Time { return *clock },
		logger:        testGuardLogger(),
	}
}

// memql#897 acceptance: the two lanes have INDEPENDENT budgets. Saturating
// the interactive ceiling must not block background calls, and vice versa.
func TestAdmitRate_LanesAreIndependent(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestLaneGuard(2 /*interactive*/, 3 /*background*/, 10*time.Second, &now)

	// Saturate the interactive bucket (max 2).
	for i := 1; i <= 2; i++ {
		if open, _ := g.admitRate(false); open {
			t.Fatalf("interactive call %d should be admitted (<= 2)", i)
		}
	}
	if open, _ := g.admitRate(false); !open {
		t.Fatalf("interactive call 3 must be blocked (> 2)")
	}

	// The background bucket is untouched: it admits its full ceiling (3)
	// even though interactive is saturated.
	for i := 1; i <= 3; i++ {
		if open, _ := g.admitRate(true); open {
			t.Fatalf("background call %d should be admitted (<= 3) despite interactive being full", i)
		}
	}
	if open, _ := g.admitRate(true); !open {
		t.Fatalf("background call 4 must be blocked (> 3)")
	}
}

// The reverse direction: a saturated background lane must never throttle
// interactive (live chat / voice) traffic. This is the core of the epic --
// a burst of task executions can't degrade live conversation.
func TestAdmitRate_BackgroundBurstDoesNotStarveInteractive(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestLaneGuard(5 /*interactive*/, 3 /*background*/, 10*time.Second, &now)

	// Hammer the background lane far past its ceiling.
	for i := 0; i < 50; i++ {
		g.admitRate(true)
	}
	// Interactive still has its full, independent budget available.
	for i := 1; i <= 5; i++ {
		if open, _ := g.admitRate(false); open {
			t.Fatalf("interactive call %d must be admitted despite a background burst", i)
		}
	}
}

// Integration: guardedTransport routes a request to the background bucket
// when its context is tagged via ContextWithBackgroundLane, so a saturated
// interactive ceiling still lets background calls through.
func TestGuardedTransport_BackgroundLaneRoutesToBackgroundBucket(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestLaneGuard(1 /*interactive*/, 5 /*background*/, 10*time.Second, &now)
	stub := &stubRoundTripper{}
	gt := &guardedTransport{base: stub, guard: g}

	// Saturate the interactive bucket with an untagged request (the one
	// admit), then confirm a second untagged request is blocked.
	req := mkMessagesReq(`{"model":"claude","messages":[{"role":"user","content":"i-1"}]}`)
	stub.resp = okResponse(req)
	if resp, _ := gt.RoundTrip(req); resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("first interactive call should pass")
	}
	req2 := mkMessagesReq(`{"model":"claude","messages":[{"role":"user","content":"i-2"}]}`)
	stub.resp = okResponse(req2)
	if resp, _ := gt.RoundTrip(req2); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatal("second interactive call must be blocked (interactive ceiling 1)")
	}

	// A background-tagged request still passes -- different bucket, room to spare.
	bgReq := mkMessagesReq(`{"model":"claude","messages":[{"role":"user","content":"bg-1"}]}`)
	bgReq = bgReq.WithContext(ContextWithBackgroundLane(context.Background()))
	stub.resp = okResponse(bgReq)
	if resp, _ := gt.RoundTrip(bgReq); resp.StatusCode == http.StatusTooManyRequests {
		t.Fatal("background-tagged call must pass while only the interactive bucket is saturated")
	}
}

func TestBackgroundLaneFromContext(t *testing.T) {
	if backgroundLaneFromContext(context.Background()) {
		t.Fatal("untagged context must report interactive (false)")
	}
	if !backgroundLaneFromContext(ContextWithBackgroundLane(context.Background())) {
		t.Fatal("tagged context must report background (true)")
	}
	//nolint:staticcheck // explicitly testing the nil-context guard
	if backgroundLaneFromContext(nil) {
		t.Fatal("nil context must report interactive (false)")
	}
}
