package memql

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// newTestKillSwitch builds a guard with ONLY the cumulative latching
// kill-switch active (loop breaker + rate ceiling disabled) so the latch
// behavior is isolated. A cap of 0 means unlimited for that dimension.
func newTestKillSwitch(maxCalls int, maxCostUSD float64) *llmGuard {
	return &llmGuard{
		enabled:           false, // loop breaker off
		rateEnabled:       false, // rate ceiling off
		killSwitchEnabled: true,
		maxTotalCalls:     maxCalls,
		maxTotalCostUSD:   maxCostUSD,
		costInPerMillion:  defaultCostInputPerMillion,
		costOutPerMillion: defaultCostOutputPerMillion,
		hits:              map[string][]time.Time{},
		tripped:           map[string]time.Time{},
		now:               func() time.Time { return time.Unix(0, 0) },
		logger:            testGuardLogger(),
	}
}

// mkChatReq builds a POST request to an arbitrary guarded endpoint with the
// given body, so tests can interleave /v1/messages and /chat/completions
// (the "different paths share one counter" proof).
func mkChatReq(urlStr, body string) *http.Request {
	u, _ := url.Parse(urlStr)
	return &http.Request{
		Method: "POST",
		URL:    u,
		Header: http.Header{},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

// --- recordAndMaybeLatch unit tests --------------------------------------

func TestKillSwitch_CallCeiling_AdmitsExactlyNThenLatches(t *testing.T) {
	g := newTestKillSwitch(3, 0)
	// First 3 admitted.
	for i := 1; i <= 3; i++ {
		if _, blocked := g.recordAndMaybeLatch([]byte(`{}`)); blocked {
			t.Fatalf("call %d should be admitted (<= maxTotalCalls 3)", i)
		}
	}
	// The 4th latches and is blocked.
	if _, blocked := g.recordAndMaybeLatch([]byte(`{}`)); !blocked {
		t.Fatalf("the 4th call must latch the kill-switch")
	}
	if !g.latched {
		t.Fatalf("guard must be latched after exceeding the call ceiling")
	}
}

// The defining property a rate limiter cannot offer: once latched, it STAYS
// latched. No amount of further calls (or simulated time) re-admits.
func TestKillSwitch_Latched_NeverReadmits(t *testing.T) {
	g := newTestKillSwitch(2, 0)
	g.recordAndMaybeLatch([]byte(`{}`))
	g.recordAndMaybeLatch([]byte(`{}`))
	// Trip it.
	if _, blocked := g.recordAndMaybeLatch([]byte(`{}`)); !blocked {
		t.Fatalf("3rd call must latch")
	}
	// 1000 more calls -- every single one stays blocked. There is no drain.
	for i := 0; i < 1000; i++ {
		if reason, blocked := g.recordAndMaybeLatch([]byte(`{}`)); !blocked || reason == "" {
			t.Fatalf("latched kill-switch must block call %d permanently", i)
		}
	}
}

func TestKillSwitch_CostCeiling_Latches(t *testing.T) {
	// Tiny cost cap; each call carries a large max_tokens so the estimate
	// climbs fast. defaultCostOutputPerMillion=75/M, 100000 tokens -> $7.5
	// estimated output cost per call, so the 2nd call crosses a $5 cap.
	g := newTestKillSwitch(0, 5.0)
	body := `{"model":"claude","max_tokens":100000,"messages":[]}`
	if _, blocked := g.recordAndMaybeLatch([]byte(body)); blocked {
		t.Fatalf("first call must be admitted before any cost accrued")
	}
	if _, blocked := g.recordAndMaybeLatch([]byte(body)); !blocked {
		t.Fatalf("second call must latch once the cumulative cost estimate crosses the cap")
	}
	if !g.latched {
		t.Fatalf("guard must be latched after exceeding the cost ceiling")
	}
}

func TestKillSwitch_Disabled_NeverLatches(t *testing.T) {
	g := newTestKillSwitch(1, 0)
	g.killSwitchEnabled = false
	for i := 0; i < 100; i++ {
		if _, blocked := g.recordAndMaybeLatch([]byte(`{}`)); blocked {
			t.Fatalf("disabled kill-switch must never block (call %d)", i)
		}
	}
	if g.latched {
		t.Fatalf("disabled kill-switch must never latch")
	}
}

func TestKillSwitch_ResetLatch_Clears(t *testing.T) {
	g := newTestKillSwitch(1, 0)
	g.recordAndMaybeLatch([]byte(`{}`))
	g.recordAndMaybeLatch([]byte(`{}`)) // latches
	if !g.latched {
		t.Fatalf("precondition: must be latched")
	}
	g.resetLatch()
	if g.latched || g.totalCalls != 0 || g.totalCostUSD != 0 {
		t.Fatalf("resetLatch must clear latch + cumulative tallies")
	}
	if _, blocked := g.recordAndMaybeLatch([]byte(`{}`)); blocked {
		t.Fatalf("after reset the next call must be admitted again")
	}
}

// --- guardedTransport integration tests ----------------------------------

// The headline acceptance: a runaway that VARIES its body every call (the
// case that defeats the fingerprint breaker AND trickles past the rate
// ceiling forever) is hard-capped at N vendor calls and then permanently
// stopped with a terminal 402.
func TestGuardedTransport_KillSwitch_HardCapsVaryingBodyRunaway(t *testing.T) {
	stub := &stubRoundTripper{}
	gt := &guardedTransport{base: stub, guard: newTestKillSwitch(5, 0)}

	terminal := 0
	const burst = 200
	for i := 0; i < burst; i++ {
		req := mkChatReq("https://api.anthropic.com/v1/messages",
			`{"model":"claude","messages":[{"role":"user","content":"`+strings.Repeat("x", i)+`"}]}`)
		stub.resp = okResponse(req)
		resp, err := gt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip error: %v", err)
		}
		if resp.StatusCode == http.StatusPaymentRequired {
			terminal++
		}
	}
	if stub.calls != 5 {
		t.Fatalf("exactly maxTotalCalls (5) calls may reach the vendor, got %d", stub.calls)
	}
	if terminal != burst-5 {
		t.Fatalf("expected %d terminal 402s, got %d", burst-5, terminal)
	}
}

// Path-agnostic proof: the cumulative counter is global, not per-endpoint or
// per-fingerprint. A "brand-new path" that interleaves a DIFFERENT endpoint
// (/v1/chat/completions) and different bodies still shares the one budget and
// is bounded -- this is the guarantee the piecemeal per-path fixes lacked.
func TestGuardedTransport_KillSwitch_PathAgnostic_NewPathSharesBudget(t *testing.T) {
	stub := &stubRoundTripper{}
	gt := &guardedTransport{base: stub, guard: newTestKillSwitch(4, 0)}

	endpoints := []string{
		"https://api.anthropic.com/v1/messages",
		"https://api.openai.com/v1/chat/completions",
	}
	total := 0
	for i := 0; i < 50; i++ {
		// Alternate endpoints + vary body: a genuinely new caller shape.
		req := mkChatReq(endpoints[i%2], fmt.Sprintf(`{"model":"m","n":%d,"messages":[]}`, i))
		stub.resp = okResponse(req)
		resp, _ := gt.RoundTrip(req)
		if resp.StatusCode != http.StatusPaymentRequired {
			total++
		}
	}
	if stub.calls != 4 {
		t.Fatalf("the shared cumulative budget must cap BOTH endpoints combined at 4, got %d", stub.calls)
	}
	if total != 4 {
		t.Fatalf("only 4 calls (across both paths) should pass through, got %d", total)
	}
}

// The kill-switch must ignore non-chat endpoints (embeddings / TTS), exactly
// like the loop breaker + rate ceiling -- they are never counted or latched.
func TestGuardedTransport_KillSwitch_IgnoresNonChatEndpoints(t *testing.T) {
	stub := &stubRoundTripper{}
	gt := &guardedTransport{base: stub, guard: newTestKillSwitch(2, 0)}
	for i := 0; i < 50; i++ {
		req := mkChatReq("https://api.openai.com/v1/embeddings", `{"input":"x"}`)
		stub.resp = okResponse(req)
		resp, _ := gt.RoundTrip(req)
		if resp.StatusCode == http.StatusPaymentRequired {
			t.Fatalf("embeddings must never hit the kill-switch (call %d)", i)
		}
	}
	if stub.calls != 50 {
		t.Fatalf("all 50 non-chat calls should reach the vendor, got %d", stub.calls)
	}
}

// --- cost estimator unit tests -------------------------------------------

func TestParseMaxOutputTokens(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"max_tokens":1234}`, 1234},
		{`{"max_completion_tokens":555}`, 555},
		{`{"model":"claude","messages":[]}`, defaultEstOutputTokens}, // absent -> fallback
		{`not json`, defaultEstOutputTokens},
		{`{"max_tokens":0}`, defaultEstOutputTokens}, // zero ignored -> fallback
	}
	for _, c := range cases {
		if got := parseMaxOutputTokens([]byte(c.body)); got != c.want {
			t.Fatalf("parseMaxOutputTokens(%q) = %d, want %d", c.body, got, c.want)
		}
	}
}

func TestEstimateRequestCostUSD_Conservative(t *testing.T) {
	// A bigger body and a bigger max_tokens must never cost less.
	small := estimateRequestCostUSD([]byte(`{"max_tokens":100,"messages":[]}`), 15, 75)
	big := estimateRequestCostUSD([]byte(`{"max_tokens":100000,"messages":[`+strings.Repeat("x", 5000)+`]}`), 15, 75)
	if big <= small {
		t.Fatalf("cost estimate must be monotonic in size + max_tokens (small=%v big=%v)", small, big)
	}
	if small <= 0 {
		t.Fatalf("a real chat request must carry a positive cost estimate, got %v", small)
	}
}
