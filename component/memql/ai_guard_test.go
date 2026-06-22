package memql

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestGuard builds a guard with a controllable clock and tight
// thresholds for deterministic tests.
func newTestGuard(maxRepeat int, window, cooldown time.Duration, clock *time.Time) *llmGuard {
	return &llmGuard{
		enabled:   true,
		maxRepeat: maxRepeat,
		window:    window,
		cooldown:  cooldown,
		hits:      map[string][]time.Time{},
		tripped:   map[string]time.Time{},
		now:       func() time.Time { return *clock },
		logger:    testGuardLogger(),
	}
}

func testGuardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLLMGuard_IdenticalRequest_TripsAfterThreshold(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestGuard(8, 60*time.Second, 30*time.Second, &now)
	fp := "same-fingerprint"

	// First 8 are admitted; the 9th (one past maxRepeat) trips.
	for i := 1; i <= 8; i++ {
		if open, _ := g.admit(fp); open {
			t.Fatalf("call %d should be admitted (<= maxRepeat 8)", i)
		}
	}
	if open, repeats := g.admit(fp); !open {
		t.Fatalf("the 9th identical call must trip the breaker (repeats=%d)", repeats)
	}
}

func TestLLMGuard_DistinctRequests_NeverThrottled(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestGuard(3, 60*time.Second, 30*time.Second, &now)
	// 100 DISTINCT requests in the same window -> none blocked.
	for i := 0; i < 100; i++ {
		fp := "fp-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		if open, _ := g.admit(fp); open {
			t.Fatalf("distinct request %d must never be throttled", i)
		}
	}
}

func TestLLMGuard_WindowEviction_AllowsAgain(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestGuard(3, 10*time.Second, 5*time.Second, &now)
	fp := "fp"
	// 3 within window: admitted.
	for i := 0; i < 3; i++ {
		if open, _ := g.admit(fp); open {
			t.Fatalf("call %d within threshold should be admitted", i)
		}
	}
	// Advance past the window so prior hits are evicted; the next call
	// is effectively the first again and must be admitted (not tripped).
	now = now.Add(11 * time.Second)
	if open, _ := g.admit(fp); open {
		t.Fatalf("after the window elapses, a repeat must be admitted again")
	}
}

func TestLLMGuard_Cooldown_ReleasesAfterElapsed(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestGuard(2, 60*time.Second, 30*time.Second, &now)
	fp := "fp"
	g.admit(fp)
	g.admit(fp)
	if open, _ := g.admit(fp); !open {
		t.Fatalf("3rd call must trip (maxRepeat=2)")
	}
	// Still tripped during cooldown.
	if open, _ := g.admit(fp); !open {
		t.Fatalf("must stay tripped during cooldown")
	}
	// After cooldown elapses, the fingerprint resets and is admitted.
	now = now.Add(31 * time.Second)
	if open, _ := g.admit(fp); open {
		t.Fatalf("after cooldown the breaker must release")
	}
}

func TestLLMGuard_Disabled_NeverTrips(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	g := newTestGuard(1, 60*time.Second, 30*time.Second, &now)
	g.enabled = false
	for i := 0; i < 50; i++ {
		if open, _ := g.admit("fp"); open {
			t.Fatalf("disabled guard must never trip")
		}
	}
}

func TestIsGuardedLLMPath(t *testing.T) {
	guarded := []string{"/v1/chat/completions", "/v1/messages", "/messages"}
	for _, p := range guarded {
		if !isGuardedLLMPath(p) {
			t.Fatalf("%q should be guarded", p)
		}
	}
	notGuarded := []string{"/v1/embeddings", "/v1/audio/speech", "/v1/moderations", "/healthz"}
	for _, p := range notGuarded {
		if isGuardedLLMPath(p) {
			t.Fatalf("%q must NOT be guarded", p)
		}
	}
}

func TestFingerprintRequest_BodySensitiveSamePath(t *testing.T) {
	a, okA := fingerprintRequest("POST", "/v1/messages", []byte(`{"m":"a"}`))
	b, okB := fingerprintRequest("POST", "/v1/messages", []byte(`{"m":"b"}`))
	same, _ := fingerprintRequest("POST", "/v1/messages", []byte(`{"m":"a"}`))
	if !okA || !okB {
		t.Fatalf("messages path should be guarded")
	}
	if a == b {
		t.Fatalf("different bodies must fingerprint differently")
	}
	if a != same {
		t.Fatalf("identical request must fingerprint identically")
	}
	if _, ok := fingerprintRequest("POST", "/v1/embeddings", []byte(`{}`)); ok {
		t.Fatalf("embeddings must not be guarded")
	}
}

// stubRoundTripper records how many times it was actually invoked.
type stubRoundTripper struct {
	calls int
	resp  *http.Response
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	if req.Body != nil {
		_, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	return s.resp, nil
}

func okResponse(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Header:     http.Header{},
		Request:    req,
	}
}

// TestGuardedTransport_BlocksRunawayLoop is the acceptance test: a loop
// firing the IDENTICAL chat request gets cut off at the transport
// (synthetic 429) and the base transport stops being hit.
func TestGuardedTransport_BlocksRunawayLoop(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	stub := &stubRoundTripper{}
	gt := &guardedTransport{
		base:  stub,
		guard: newTestGuard(5, 60*time.Second, 30*time.Second, &now),
	}
	stub.resp = nil

	body := `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`
	mkReq := func() *http.Request {
		u, _ := url.Parse("https://api.anthropic.com/v1/messages")
		return &http.Request{
			Method: "POST",
			URL:    u,
			Header: http.Header{},
			Body:   io.NopCloser(strings.NewReader(body)),
		}
	}

	blocked := 0
	for i := 0; i < 20; i++ {
		req := mkReq()
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
		t.Fatalf("base transport should be hit at most maxRepeat (5) times, got %d", stub.calls)
	}
	if blocked == 0 {
		t.Fatalf("the runaway identical-request loop must be blocked with 429s")
	}
}

func TestGuardedTransport_DistinctRequestsPassThrough(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	stub := &stubRoundTripper{}
	gt := &guardedTransport{
		base:  stub,
		guard: newTestGuard(3, 60*time.Second, 30*time.Second, &now),
	}
	for i := 0; i < 30; i++ {
		u, _ := url.Parse("https://api.anthropic.com/v1/messages")
		body := `{"model":"claude","messages":[{"role":"user","content":"msg-` + string(rune('A'+i)) + `"}]}`
		req := &http.Request{Method: "POST", URL: u, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		stub.resp = okResponse(req)
		resp, _ := gt.RoundTrip(req)
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("distinct request %d must pass through, got 429", i)
		}
	}
	if stub.calls != 30 {
		t.Fatalf("all 30 distinct requests should reach the base transport, got %d", stub.calls)
	}
}
