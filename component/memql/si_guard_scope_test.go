package memql

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// newTestScopeGuard builds a guard with ONLY the per-scope latch active
// (process kill-switch + loop breaker + rate ceiling off) so scope behavior
// is isolated.
func newTestScopeGuard(scopeMaxCalls int, scopeMaxCostUSD float64) *llmGuard {
	return &llmGuard{
		enabled:           false,
		rateEnabled:       false,
		killSwitchEnabled: false,
		scopeEnabled:      true,
		scopeMaxCalls:     scopeMaxCalls,
		scopeMaxCostUSD:   scopeMaxCostUSD,
		scopeIdleTTL:      time.Hour,
		costInPerMillion:  defaultCostInputPerMillion,
		costOutPerMillion: defaultCostOutputPerMillion,
		scopes:            map[string]*scopeBudget{},
		hits:              map[string][]time.Time{},
		tripped:           map[string]time.Time{},
		now:               func() time.Time { return time.Unix(0, 0) },
		logger:            testGuardLogger(),
	}
}

// --- context plumbing ----------------------------------------------------

func TestContextWithBudgetScope_MergesDedupsDropsEmpty(t *testing.T) {
	ctx := ContextWithBudgetScope(context.Background(), "space:A", "", "plan:B")
	got := budgetScopesFromContext(ctx)
	if len(got) != 2 || got[0] != "space:A" || got[1] != "plan:B" {
		t.Fatalf("expected [space:A plan:B], got %v", got)
	}
	// Composing: a second stamp merges + dedups.
	ctx = ContextWithBudgetScope(ctx, "space:A", "agent:C")
	got = budgetScopesFromContext(ctx)
	if len(got) != 3 {
		t.Fatalf("expected 3 deduped scopes, got %v", got)
	}
	// All-empty stamp is a no-op (returns ctx unchanged, scopes intact).
	if got2 := budgetScopesFromContext(ContextWithBudgetScope(ctx, "", "")); len(got2) != 3 {
		t.Fatalf("all-empty stamp must not change scopes, got %v", got2)
	}
}

// --- per-scope latch unit tests ------------------------------------------

func TestScope_CallCeiling_LatchesPerScope_Isolated(t *testing.T) {
	g := newTestScopeGuard(3, 0)
	runaway := []string{"space:runaway"}
	calm := []string{"space:calm"}

	// 3 admitted on the runaway scope, 4th latches it.
	for i := 0; i < 3; i++ {
		if _, blocked := g.recordAndMaybeLatch(runaway, []byte(`{}`)); blocked {
			t.Fatalf("runaway call %d should be admitted (<= 3)", i)
		}
	}
	if _, blocked := g.recordAndMaybeLatch(runaway, []byte(`{}`)); !blocked {
		t.Fatalf("4th runaway call must latch its scope")
	}
	// A DIFFERENT scope is completely unaffected -- the whole point of
	// per-scope isolation.
	for i := 0; i < 3; i++ {
		if _, blocked := g.recordAndMaybeLatch(calm, []byte(`{}`)); blocked {
			t.Fatalf("the calm scope must not be affected by another scope's latch (call %d)", i)
		}
	}
}

func TestScope_Latched_NeverReadmits(t *testing.T) {
	g := newTestScopeGuard(1, 0)
	s := []string{"space:x"}
	g.recordAndMaybeLatch(s, []byte(`{}`))
	if _, blocked := g.recordAndMaybeLatch(s, []byte(`{}`)); !blocked {
		t.Fatalf("2nd call must latch the scope")
	}
	for i := 0; i < 500; i++ {
		if reason, blocked := g.recordAndMaybeLatch(s, []byte(`{}`)); !blocked || reason == "" {
			t.Fatalf("latched scope must block call %d permanently", i)
		}
	}
}

func TestScope_CostCeiling_Latches(t *testing.T) {
	g := newTestScopeGuard(0, 5.0)
	s := []string{"plan:p"}
	body := []byte(`{"model":"claude","max_tokens":100000,"messages":[]}`) // ~$7.5 est/call
	if _, blocked := g.recordAndMaybeLatch(s, body); blocked {
		t.Fatalf("first call must be admitted before any cost accrued")
	}
	if _, blocked := g.recordAndMaybeLatch(s, body); !blocked {
		t.Fatalf("second call must latch once the scope cost estimate crosses $5")
	}
}

// A call that belongs to MULTIPLE scopes is bounded by whichever scope's cap
// it hits first, and a blocked call charges NOTHING (the other scope's tally
// is untouched).
func TestScope_MultiScope_TighterCapWins_NoChargeOnBlock(t *testing.T) {
	g := newTestScopeGuard(2, 0)
	// space:S has room; plan:P is already at its cap via prior calls.
	g.recordAndMaybeLatch([]string{"plan:P"}, []byte(`{}`))
	g.recordAndMaybeLatch([]string{"plan:P"}, []byte(`{}`)) // plan:P now at 2 (cap)
	before := g.scopes["space:S"]
	if before != nil {
		t.Fatalf("space:S should not exist yet")
	}
	// A call in BOTH scopes is blocked by plan:P and must NOT charge space:S.
	if _, blocked := g.recordAndMaybeLatch([]string{"space:S", "plan:P"}, []byte(`{}`)); !blocked {
		t.Fatalf("call must be blocked by the already-capped plan:P scope")
	}
	if sb := g.scopes["space:S"]; sb != nil && sb.calls != 0 {
		t.Fatalf("a blocked call must not charge the un-capped scope, got space:S calls=%d", sb.calls)
	}
}

func TestScope_Disabled_NeverLatches(t *testing.T) {
	g := newTestScopeGuard(1, 0)
	g.scopeEnabled = false
	for i := 0; i < 100; i++ {
		if _, blocked := g.recordAndMaybeLatch([]string{"space:x"}, []byte(`{}`)); blocked {
			t.Fatalf("disabled scope guard must never block (call %d)", i)
		}
	}
}

// --- guardedTransport integration ----------------------------------------

// A runaway in ONE conversation is hard-capped at its scope's cap while a
// different conversation keeps flowing -- the everyday, on-by-default
// protection that doesn't require a process restart and doesn't touch other
// users.
func TestGuardedTransport_Scope_CapsOneConversation_OthersFlow(t *testing.T) {
	stub := &stubRoundTripper{}
	gt := &guardedTransport{base: stub, guard: newTestScopeGuard(5, 0)}

	mk := func(scope, body string) *http.Request {
		req := mkChatReq("https://api.anthropic.com/v1/messages", body)
		return req.WithContext(ContextWithBudgetScope(context.Background(), scope))
	}

	// Hammer space:bad with 50 varying-body calls -> capped at 5.
	for i := 0; i < 50; i++ {
		req := mk("space:bad", `{"messages":[{"content":"`+string(rune('a'+i%26))+`"}]}`)
		stub.resp = okResponse(req)
		gt.RoundTrip(req)
	}
	// Meanwhile space:good makes 5 calls -> all flow (its own budget).
	good := 0
	for i := 0; i < 5; i++ {
		req := mk("space:good", `{"messages":[{"content":"good`+string(rune('a'+i))+`"}]}`)
		stub.resp = okResponse(req)
		resp, _ := gt.RoundTrip(req)
		if resp.StatusCode != http.StatusPaymentRequired {
			good++
		}
	}
	if good != 5 {
		t.Fatalf("the healthy conversation must not be throttled by another's runaway, got %d/5", good)
	}
	// Total vendor calls = 5 (bad, capped) + 5 (good) = 10.
	if stub.calls != 10 {
		t.Fatalf("expected 5 (capped bad) + 5 (good) = 10 vendor calls, got %d", stub.calls)
	}
}

func TestPruneIdleScopesLocked(t *testing.T) {
	g := newTestScopeGuard(0, 0)
	g.scopeIdleTTL = 60 * time.Second
	now := time.Unix(10000, 0)
	// 300 scopes (> the 256 prune threshold); half idle, half fresh.
	for i := 0; i < 300; i++ {
		seen := now
		if i%2 == 0 {
			seen = now.Add(-120 * time.Second) // idle past the TTL
		}
		g.scopes[string(rune(i))+"_"+time.Duration(i).String()] = &scopeBudget{lastSeen: seen}
	}
	g.pruneIdleScopesLocked(now)
	if len(g.scopes) != 150 {
		t.Fatalf("idle scopes past the TTL must be pruned; expected 150 fresh, got %d", len(g.scopes))
	}
}
