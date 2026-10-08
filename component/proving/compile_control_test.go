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
	if got, err := CompileCallsOnCatalogHit("Produce the weekly ledger reconciliation", []string{"account"}); err != nil || got != 0 {
		t.Fatalf("an exact catalog hit made %d model call(s), want 0", got)
	}
}

func TestTheControlProvesTheCounterCanRise(t *testing.T) {
	got, err := CompileCallsOnCatalogMiss("Draft an unprecedented settlement narrative in the style of a court filing", []string{"account"})
	if err != nil {
		t.Fatal(err)
	}
	if got == 0 {
		t.Fatal("a catalog MISS also made zero model calls. " +
			"That means the counter never rises on any path, so the zero in the test above proves nothing " +
			"and every figure derived from it is worthless. Fix the instrument before believing the suite.")
	}
}

func TestCompileMeasurementUsesTheSameInputShapeRegardlessOfArgumentOrder(t *testing.T) {
	a, err := CompileCallsOnCatalogHit("Reconcile the ledger", []string{"account", "period"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := CompileCallsOnCatalogHit("Reconcile the ledger", []string{"period", "account"})
	if err != nil || a != 0 || b != 0 {
		t.Fatalf("argument order changed measurement: %d %d %v", a, b, err)
	}
}
