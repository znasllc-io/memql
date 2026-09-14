package events

import (
	"context"
	"testing"
)

func TestCauseZeroIsARoot(t *testing.T) {
	if !(Cause{}).IsZero() {
		t.Fatal("the zero cause must read as a root event")
	}
	for _, c := range []Cause{
		{CausationId: "run-1"},
		{CorrelationId: "evt-1"},
		{Depth: 1},
		{Chain: []Link{{Automation: "a", RunId: "run-1"}}},
	} {
		if c.IsZero() {
			t.Errorf("%+v is not a root: any field set means a run caused it", c)
		}
	}
}

func TestCauseNextExtendsTheChainOneDeeper(t *testing.T) {
	parent := Cause{
		CausationId:   "run-1",
		CorrelationId: "evt-root",
		Depth:         1,
		Chain:         []Link{{Automation: "a", RunId: "run-1"}},
	}
	child := parent.Next("b", "run-2", parent.CorrelationId)

	if child.Depth != 2 {
		t.Errorf("depth = %d, want 2: a run is one deeper than the event that triggered it", child.Depth)
	}
	if child.CausationId != "run-2" {
		t.Errorf("causation = %q, want run-2: the run is the cause of what it publishes", child.CausationId)
	}
	if child.CorrelationId != "evt-root" {
		t.Errorf("correlation = %q, want the chain's own", child.CorrelationId)
	}
	want := []Link{{Automation: "a", RunId: "run-1"}, {Automation: "b", RunId: "run-2"}}
	if len(child.Chain) != len(want) {
		t.Fatalf("chain = %+v, want %+v", child.Chain, want)
	}
	for i := range want {
		if child.Chain[i] != want[i] {
			t.Fatalf("chain = %+v, want %+v (oldest first)", child.Chain, want)
		}
	}
}

// The child must not share the parent's backing array: two runs triggered by
// the same event each extend the chain, and an append into a shared array
// would let one run's link overwrite the other's.
func TestCauseNextDoesNotAliasTheParent(t *testing.T) {
	parent := Cause{Depth: 1, Chain: make([]Link, 1, 8)}
	parent.Chain[0] = Link{Automation: "a", RunId: "run-1"}

	left := parent.Next("b", "run-2", "c")
	right := parent.Next("c", "run-3", "c")
	if left.Chain[1].Automation != "b" || right.Chain[1].Automation != "c" {
		t.Fatalf("siblings share a chain: left %+v, right %+v", left.Chain, right.Chain)
	}

	parent.Chain[0].Automation = "mutated"
	if left.Chain[0].Automation != "a" {
		t.Fatal("a change to the parent's chain reached the child's")
	}
}

func TestCauseCloneDoesNotAlias(t *testing.T) {
	c := Cause{Depth: 2, Chain: []Link{{Automation: "a", RunId: "1"}, {Automation: "b", RunId: "2"}}}
	cl := c.Clone()
	cl.Chain[0].Automation = "x"
	if c.Chain[0].Automation != "a" {
		t.Fatal("Clone shares the chain's backing array")
	}
}

func TestCauseContextRoundTrip(t *testing.T) {
	if _, ok := CauseFromContext(context.Background()); ok {
		t.Fatal("an empty context carries no cause")
	}
	if _, ok := CauseFromContext(ContextWithCause(context.Background(), Cause{})); ok {
		t.Fatal("a zero cause is reported as none: stamping it would read as a root anyway")
	}

	want := Cause{CausationId: "run-9", CorrelationId: "evt-1", Depth: 3, Chain: []Link{{Automation: "a", RunId: "run-9"}}}
	ctx := ContextWithCause(context.Background(), want)
	want.Chain[0].Automation = "changed after stamping"

	got, ok := CauseFromContext(ctx)
	if !ok {
		t.Fatal("the cause was lost")
	}
	if got.Depth != 3 || got.CausationId != "run-9" || got.CorrelationId != "evt-1" {
		t.Fatalf("got %+v", got)
	}
	if got.Chain[0].Automation != "a" {
		t.Fatal("the context holds the caller's array: a change after stamping reached it")
	}
}
