package work

import "testing"

// spend_test.go -- AddCall, the dollar/loop split as a fold (memql#5580).

// EVERY ANSWER IS ONE MODEL CALL, whoever answered it. This is the property a
// loop cap rests on: a run that spins on warm cache hits or replayed journal
// answers is still spinning, and a cap that counts only the calls MemQL paid
// for is a hole the cheapest path walks straight through.
func TestAddCall_LoopCountRisesForEveryAnswer(t *testing.T) {
	var s Spent
	for _, served := range []string{ServeLive, ServeJournal, ServedLocal, ServedSubscription, ServedCache, "something nobody declared"} {
		s = AddCall(s, CallSpend{Served: served, InputTokens: 10, OutputTokens: 10, Cost: 1})
	}
	if s.ModelCalls != 6 {
		t.Fatalf("ModelCalls = %d, want 6: every answer is one call", s.ModelCalls)
	}
}

// THE DOLLAR BUCKETS TAKE ONLY WHAT MEMQL WAS BILLED FOR, and each other
// answer lands in its own bucket or in none.
func TestAddCall_TokensLandInTheBucketThatWasBilled(t *testing.T) {
	for _, tc := range []struct {
		served                      string
		tokens, local, subscription int
		cost                        float64
	}{
		{ServeLive, 30, 0, 0, 2},
		{ServedLocal, 0, 30, 0, 0},
		{ServedSubscription, 0, 0, 30, 0},
		// A replayed answer cost nothing and its token counts belong to the
		// ORIGINAL call, so charging them here would bill one run for
		// another run's work. The model seam already records cost: 0 for a
		// journal hit for exactly this reason.
		{ServeJournal, 0, 0, 0, 0},
		// A cache hit is the same argument, one layer up.
		{ServedCache, 0, 0, 0, 0},
	} {
		s := AddCall(Spent{}, CallSpend{Served: tc.served, InputTokens: 10, OutputTokens: 20, Cost: 2})
		if s.Tokens != tc.tokens || s.TokensLocal != tc.local || s.TokensSubscription != tc.subscription || s.Cost != tc.cost {
			t.Errorf("%s: got tokens=%d local=%d subscription=%d cost=%v; want %d/%d/%d/%v",
				tc.served, s.Tokens, s.TokensLocal, s.TokensSubscription, s.Cost,
				tc.tokens, tc.local, tc.subscription, tc.cost)
		}
	}
}

// AN UNDECLARED ORIGIN COUNTS AS METERED. The fail-closed direction: an answer
// whose origin nobody named must not be free, or a new served value added
// somewhere else becomes a way to spend without spending.
func TestAddCall_AnUnknownOriginIsCountedAsMetered(t *testing.T) {
	s := AddCall(Spent{}, CallSpend{Served: "app-of-the-future", InputTokens: 5, OutputTokens: 5, Cost: 3})
	if s.Tokens != 10 || s.Cost != 3 {
		t.Fatalf("an unrecognised origin must count as metered; got %+v", s)
	}
}

// The fold and the check agree: a run whose whole spend was local or replayed
// never breaches a DOLLAR ceiling and does breach the LOOP cap.
func TestAddCall_AgreesWithCheckCeilings(t *testing.T) {
	var s Spent
	for i := 0; i < 5; i++ {
		s = AddCall(s, CallSpend{Served: ServedLocal, InputTokens: 1000, OutputTokens: 1000})
		s = AddCall(s, CallSpend{Served: ServeJournal, InputTokens: 1000, OutputTokens: 1000})
	}
	if b := CheckCeilings(Ceilings{TokenBudget: 100, CostCeiling: 0.01}, s, 0); b != nil {
		t.Fatalf("nothing here was billed, so no dollar ceiling may breach; got %+v", b)
	}
	if b := CheckCeilings(Ceilings{MaxModelCalls: 10}, s, 0); b == nil || b.Ceiling != CeilingModelCalls {
		t.Fatalf("ten answers must reach a ten-call cap; got %+v", b)
	}
}

// `unevaluated` is NOT a ceiling and must not read like one that was set.
func TestUnevaluatedBreach_NamesItselfAndIsNotACeiling(t *testing.T) {
	b := UnevaluatedBreach("the goal row could not be read")
	if b == nil || b.Ceiling != CeilingUnevaluated {
		t.Fatalf("breach = %+v, want ceiling %q", b, CeilingUnevaluated)
	}
	if b.Reason == "" {
		t.Fatal("a refusal nobody can explain is not one a person can act on")
	}
	for _, real := range []string{CeilingTokens, CeilingCost, CeilingWallClock, CeilingModelCalls, CeilingRetries, CeilingEvents} {
		if CeilingUnevaluated == real {
			t.Fatalf("the unevaluated sentinel collides with the real ceiling %q", real)
		}
	}
}
