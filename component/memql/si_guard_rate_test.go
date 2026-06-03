package memql

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestRateGuard builds a guard with ONLY the global rate ceiling
// active (the per-fingerprint loop breaker disabled) and a controllable
// clock, so the rate-ceiling behavior is isolated in tests.
func newTestRateGuard(rateMax int, rateWindow time.Duration, clock *time.Time) *llmGuard {
	return &llmGuard{
		enabled:     false, // loop breaker off -- isolate the rate ceiling
		rateEnabled: true,
		rateMax:     rateMax,
		rateWindow:  rateWindow,
		hits:        map[string][]time.Time{},
		tripped:     map[string]time.Time{},
		now:         func() time.Time { return *clock },
		logger:      testGuardLogger(),
	}
}

// mkMessagesReq builds a POST /v1/messages request with the given body.
func mkMessagesReq(body string) *http.Request {
	u, _ := url.Parse("https://api.anthropic.com/v1/messages")
	return &http.Request{
		Method: "POST",
		URL:    u,
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

// --- admitRate unit tests -------------------------------------------------

func TestAdmitRate_AdmitsUpToCeilingThenBlocks(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestRateGuard(5, 10*time.Second, &now)
	// First 5 are admitted.
	for i := 1; i <= 5; i++ {
		if open, _ := g.admitRate(); open {
			t.Fatalf("call %d should be admitted (<= rateMax 5)", i)
		}
	}
	// The 6th and beyond are blocked.
	for i := 6; i <= 10; i++ {
		if open, _ := g.admitRate(); !open {
			t.Fatalf("call %d must be blocked (> rateMax 5)", i)
		}
	}
}

func TestAdmitRate_Disabled_NeverBlocks(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestRateGuard(2, 10*time.Second, &now)
	g.rateEnabled = false
	for i := 0; i < 100; i++ {
		if open, _ := g.admitRate(); open {
			t.Fatalf("disabled rate guard must never block (call %d)", i)
		}
	}
}

// Acceptance #3 -- the window slides: old admitted calls are evicted, so
// a sustained-but-paced caller is never throttled.
func TestAdmitRate_WindowSlides_EvictsOldCalls(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestRateGuard(3, 10*time.Second, &now)
	// Fill the window.
	for i := 0; i < 3; i++ {
		if open, _ := g.admitRate(); open {
			t.Fatalf("call %d within ceiling should be admitted", i)
		}
	}
	if open, _ := g.admitRate(); !open {
		t.Fatalf("4th call within the window must be blocked")
	}
	// Advance past the window so the 3 old calls are evicted.
	now = now.Add(11 * time.Second)
	if open, _ := g.admitRate(); open {
		t.Fatalf("after the window slides, the next call must be admitted again")
	}
}

// Acceptance #2 -- calls paced one-per-window are all admitted (no false
// throttling of normal traffic).
func TestAdmitRate_PacedCalls_NeverThrottled(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestRateGuard(2, 10*time.Second, &now)
	for i := 0; i < 50; i++ {
		if open, _ := g.admitRate(); open {
			t.Fatalf("paced call %d must never be throttled", i)
		}
		now = now.Add(11 * time.Second) // one call per window
	}
}

// Blocked calls must NOT be recorded -- otherwise a hammering loop would
// keep its own window full forever and starve real traffic even after it
// stops. Once the loop pauses past the window, traffic resumes.
func TestAdmitRate_BlockedCallsNotRecorded_WindowDrains(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestRateGuard(2, 10*time.Second, &now)
	g.admitRate()
	g.admitRate()
	// Hammer 100 blocked calls at t0 -- none recorded.
	for i := 0; i < 100; i++ {
		if open, _ := g.admitRate(); !open {
			t.Fatalf("call should be blocked while window is full")
		}
	}
	// Advance just past the window from the ORIGINAL two admits. If
	// blocked calls had been recorded, the window would still be full.
	now = now.Add(11 * time.Second)
	if open, _ := g.admitRate(); open {
		t.Fatalf("after the original admits age out, traffic must resume (blocked calls must not extend the window)")
	}
}

// --- guardedTransport integration tests -----------------------------------

// Acceptance #1 (the case #825 misses): a burst of N+ DISTINCT requests
// (different bodies) within the window is capped -- base transport hit at
// most rateMax times, the rest get a synthetic 429.
func TestGuardedTransport_RateCeiling_CapsDistinctBurst(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	stub := &stubRoundTripper{}
	gt := &guardedTransport{
		base:  stub,
		guard: newTestRateGuard(5, 10*time.Second, &now),
	}

	blocked := 0
	const burst = 40
	for i := 0; i < burst; i++ {
		// Every request body is DISTINCT -- the loop breaker (disabled
		// here anyway) would never catch these; only the rate ceiling can.
		req := mkMessagesReq(`{"model":"claude","messages":[{"role":"user","content":"distinct-` + strings.Repeat("x", i) + `"}]}`)
		stub.resp = okResponse(req)
		resp, err := gt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip returned error: %v", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			blocked++
		}
	}
	if stub.calls > 5 {
		t.Fatalf("base transport must be hit at most rateMax (5) times for a distinct burst, got %d", stub.calls)
	}
	if blocked != burst-5 {
		t.Fatalf("expected %d blocked (burst %d - rateMax 5), got %d", burst-5, burst, blocked)
	}
}

// Acceptance #5 -- the ceiling only applies to chat/messages endpoints;
// embeddings / TTS are unaffected even far past the ceiling.
func TestGuardedTransport_RateCeiling_IgnoresNonChatEndpoints(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	stub := &stubRoundTripper{}
	gt := &guardedTransport{
		base:  stub,
		guard: newTestRateGuard(3, 10*time.Second, &now),
	}
	nonChat := []string{"/v1/embeddings", "/v1/audio/speech", "/v1/moderations"}
	for i := 0; i < 30; i++ {
		u, _ := url.Parse("https://api.openai.com" + nonChat[i%len(nonChat)])
		req := &http.Request{Method: "POST", URL: u, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}
		stub.resp = okResponse(req)
		resp, _ := gt.RoundTrip(req)
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("non-chat endpoint %s must never hit the rate ceiling", nonChat[i%len(nonChat)])
		}
	}
	if stub.calls != 30 {
		t.Fatalf("all 30 non-chat calls should reach the base transport, got %d", stub.calls)
	}
}

// Acceptance #4 -- the disabled flag bypasses the rate ceiling entirely.
func TestGuardedTransport_RateCeiling_DisabledBypasses(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	guard := newTestRateGuard(2, 10*time.Second, &now)
	guard.rateEnabled = false
	stub := &stubRoundTripper{}
	gt := &guardedTransport{base: stub, guard: guard}
	for i := 0; i < 25; i++ {
		req := mkMessagesReq(`{"model":"claude","messages":[{"role":"user","content":"d-` + strings.Repeat("y", i) + `"}]}`)
		stub.resp = okResponse(req)
		resp, _ := gt.RoundTrip(req)
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("with the rate guard disabled, call %d must pass through", i)
		}
	}
	if stub.calls != 25 {
		t.Fatalf("all 25 calls should reach the base transport when disabled, got %d", stub.calls)
	}
}

// Acceptance #6 -- combined with the identical-request breaker, a runaway
// is stopped within a few calls regardless of whether it repeats or
// varies its body. Here BOTH guards are enabled (as in production).
func TestGuardedTransport_BothGuards_StopRunawayFast(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	// Loop breaker maxRepeat=8, rate ceiling rateMax=5 -- the rate
	// ceiling is the tighter bound for a varying-body loop.
	guard := &llmGuard{
		enabled:     true,
		maxRepeat:   8,
		window:      60 * time.Second,
		cooldown:    30 * time.Second,
		rateEnabled: true,
		rateMax:     5,
		rateWindow:  10 * time.Second,
		hits:        map[string][]time.Time{},
		tripped:     map[string]time.Time{},
		now:         func() time.Time { return now },
		logger:      testGuardLogger(),
	}
	stub := &stubRoundTripper{}
	gt := &guardedTransport{base: stub, guard: guard}

	// A loop that VARIES its body every call (growing context) -- defeats
	// the fingerprint breaker, caught only by the rate ceiling.
	for i := 0; i < 50; i++ {
		req := mkMessagesReq(`{"model":"claude","messages":[{"role":"user","content":"ctx` + strings.Repeat("z", i) + `"}]}`)
		stub.resp = okResponse(req)
		if _, err := gt.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip error: %v", err)
		}
	}
	if stub.calls > 5 {
		t.Fatalf("a varying-body runaway must be capped at rateMax (5) vendor calls, got %d", stub.calls)
	}
}
