package work

import "testing"

func TestCheckCeilings_TokenBudgetExcludesSubscriptionAndLocal(t *testing.T) {
	c := Ceilings{TokenBudget: 1000}
	// 900 metered + 5000 subscription + 5000 local. The DOLLAR ceiling
	// must see 900: MemQL was not billed for the other two.
	s := Spent{Tokens: 900, TokensSubscription: 5000, TokensLocal: 5000}
	if b := CheckCeilings(c, s, 50); b != nil {
		t.Fatalf("subscription and local spend must NOT burn the token budget; breached on %+v", b)
	}
	if b := CheckCeilings(c, s, 200); b == nil || b.Ceiling != CeilingTokens {
		t.Fatalf("900 + 200 > 1000 must breach the token budget; got %+v", b)
	}
}

func TestCheckCeilings_LoopCapsIncludeEveryCall(t *testing.T) {
	c := Ceilings{MaxModelCalls: 3}
	// A runaway loop that happens to route through a subscription is
	// still a runaway loop.
	if b := CheckCeilings(c, Spent{ModelCalls: 3}, 0); b == nil || b.Ceiling != CeilingModelCalls {
		t.Fatalf("the loop cap counts every call regardless of who was billed; got %+v", b)
	}
}

func TestCheckCeilings_EachCeiling(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Ceilings
		s    Spent
		want string
	}{
		{"cost", Ceilings{CostCeiling: 1.5}, Spent{Cost: 1.5}, CeilingCost},
		{"wallClock", Ceilings{WallClockMs: 1000}, Spent{WallClockMs: 1000}, CeilingWallClock},
		{"retries", Ceilings{MaxRetries: 2}, Spent{Retries: 2}, CeilingRetries},
		{"events", Ceilings{MaxEvents: 10}, Spent{Events: 10}, CeilingEvents},
	} {
		b := CheckCeilings(tc.c, tc.s, 0)
		if b == nil || b.Ceiling != tc.want {
			t.Errorf("%s: breach = %+v, want %s", tc.name, b, tc.want)
		}
	}
}

// A ceiling of zero is "not configured", never "nothing allowed". The
// opposite reading would park every run on a goal that set no ceilings.
func TestCheckCeilings_ZeroMeansUnset(t *testing.T) {
	if b := CheckCeilings(Ceilings{}, Spent{Tokens: 1 << 30, Cost: 1e9, ModelCalls: 1 << 20}, 1<<20); b != nil {
		t.Fatalf("an unset ceiling must not park a run; got %+v", b)
	}
}

func TestCheckCeilings_BreachNamesTheNumbers(t *testing.T) {
	b := CheckCeilings(Ceilings{TokenBudget: 100}, Spent{Tokens: 90}, 20)
	if b == nil {
		t.Fatal("expected a breach")
	}
	if b.Limit == "" || b.Actual == "" || b.Reason == "" {
		t.Fatalf("a budget approval shows a person WHY it parked; got %+v", b)
	}
}

func TestCheckCeilings_CostIsMeteredOnly(t *testing.T) {
	// Local and subscription tokens have no dollar cost, so a run that
	// spent everything on them must not breach the cost ceiling.
	if b := CheckCeilings(Ceilings{CostCeiling: 5}, Spent{Cost: 0, TokensSubscription: 1 << 20, TokensLocal: 1 << 20}, 0); b != nil {
		t.Fatalf("cost is metered spend only; got %+v", b)
	}
}
