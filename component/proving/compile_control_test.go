package proving

import "testing"

// The cheapest and most checkable of the epic's claims, with the control that
// makes it mean anything.
//
// The pattern is inherited from integrations/planner's compile tests and its
// reason is written down there: A COUNTER THAT IS NEVER INCREMENTED ON ANY
// PATH READS AS ZERO FOREVER. A "zero model calls" assertion with no companion
// proving the counter can rise is an assertion that would still pass if the
// instrument were unplugged.

func TestAnExactCatalogHitReachesNoModel(t *testing.T) {
	if got := CompileCallsOnCatalogHit("Produce the weekly ledger reconciliation", []string{"account"}); got != 0 {
		t.Fatalf("an exact catalog hit made %d model call(s), want 0", got)
	}
}

func TestTheControlProvesTheCounterCanRise(t *testing.T) {
	got := CompileCallsOnCatalogMiss("Draft an unprecedented settlement narrative in the style of a court filing", []string{"account"})
	if got == 0 {
		t.Fatal("a catalog MISS also made zero model calls. " +
			"That means the counter never rises on any path, so the zero in the test above proves nothing " +
			"and every figure derived from it is worthless. Fix the instrument before believing the suite.")
	}
}

func TestTheClaimIsAPropertyOfTheReturnedValueAndNotOfAStub(t *testing.T) {
	// component/work.Decide is a pure function over values, so "an exact hit
	// needs no model" is decidable without a provider, a database or a
	// network. Routing it through a stub provider would measure the stub.
	//
	// Asserting the argument order matters too: the same statement with
	// different argument names is a DIFFERENT goal signature, and a catalog
	// keyed on an order-sensitive signature would miss on a caller's spelling.
	a := CompileCallsOnCatalogHit("Reconcile the ledger", []string{"account", "period"})
	b := CompileCallsOnCatalogHit("Reconcile the ledger", []string{"period", "account"})
	if a != 0 || b != 0 {
		t.Fatalf("argument ORDER changed the answer: %d and %d", a, b)
	}
}
