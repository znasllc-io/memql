package events

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"reflect"
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

// TestEventCloneCopiesCauseWithoutAliasing guards the fact Clone() is built
// on: component/bus.Bus.Publish clones the event once per subscriber, so a
// field Clone forgets is lost for every subscriber, not just one. Cause is
// exactly that kind of field -- an automation run's whole causal lineage,
// silently reset to the zero (root) cause for every fan-out.
func TestEventCloneCopiesCauseWithoutAliasing(t *testing.T) {
	original := NewEvent("test", KindNodeCreated, map[string]any{"a": 1})
	original.Cause = Cause{
		CausationId:   "run-1",
		CorrelationId: "evt-1",
		Depth:         2,
		Chain:         []Link{{Automation: "a", RunId: "run-1"}, {Automation: "b", RunId: "run-2"}},
	}

	cloned := original.Clone()

	if cloned.Cause.CausationId != original.Cause.CausationId ||
		cloned.Cause.CorrelationId != original.Cause.CorrelationId ||
		cloned.Cause.Depth != original.Cause.Depth {
		t.Fatalf("Clone did not copy Cause: got %+v, want %+v", cloned.Cause, original.Cause)
	}
	if len(cloned.Cause.Chain) != len(original.Cause.Chain) {
		t.Fatalf("Clone dropped chain entries: got %+v, want %+v", cloned.Cause.Chain, original.Cause.Chain)
	}

	// The clone must not share the original's backing array: one
	// subscriber's copy must not be mutable by way of another's.
	cloned.Cause.Chain[0].Automation = "mutated"
	if original.Cause.Chain[0].Automation != "a" {
		t.Fatal("Clone shares the Cause chain's backing array with the original")
	}
}

// TestEventWithCauseReturnsACopy guards WithCause's value-receiver contract:
// every publish path calls it as event = event.WithCause(c), so a version
// that mutated in place would still work there, but would also let a shared
// event value leak a cause across two callers that both hold a copy.
func TestEventWithCauseReturnsACopy(t *testing.T) {
	original := NewEvent("test", KindNodeCreated, nil)
	cause := Cause{CausationId: "run-1", CorrelationId: "evt-1", Depth: 1, Chain: []Link{{Automation: "a", RunId: "run-1"}}}

	withCause := original.WithCause(cause)

	if !original.Cause.IsZero() {
		t.Fatal("WithCause mutated the receiver: the original must stay a root event")
	}
	if withCause.Cause.CausationId != "run-1" || withCause.Cause.Depth != 1 {
		t.Fatalf("WithCause did not set the cause: got %+v", withCause.Cause)
	}
}

// TestCauseFromMapReadsTheJSONShape: a run's triggerEvent.cause is written as
// Cause's JSON and read back as whatever the decoder made of it -- a float64
// depth, a []any chain of maps. What comes out must be the cause that went in,
// or a resumed run leaves its chain.
func TestCauseFromMapReadsTheJSONShape(t *testing.T) {
	want := Cause{
		CausationId:   "run-2",
		CorrelationId: "evt-1",
		Depth:         2,
		Chain:         []Link{{Automation: "a", RunId: "run-1"}, {Automation: "b", RunId: "run-2"}},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, isFloat := decoded["depth"].(float64); !isFloat {
		t.Fatalf("the fixture must carry the depth as encoding/json decodes it (float64); got %T", decoded["depth"])
	}
	if got := CauseFromMap(decoded); !reflect.DeepEqual(got, want) {
		t.Fatalf("CauseFromMap(float64 depth) = %+v, want %+v", got, want)
	}

	// A decoder told to keep numbers as json.Number reads the same cause.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var numbered map[string]any
	if err := dec.Decode(&numbered); err != nil {
		t.Fatal(err)
	}
	if got := CauseFromMap(numbered); !reflect.DeepEqual(got, want) {
		t.Fatalf("CauseFromMap(json.Number depth) = %+v, want %+v", got, want)
	}

	// A Go caller's int depth, and a chain built as []map[string]any.
	built := map[string]any{
		"causationId": "run-2", "correlationId": "evt-1", "depth": 2,
		"chain": []map[string]any{{"automation": "a", "runId": "run-1"}, {"automation": "b", "runId": "run-2"}},
	}
	if got := CauseFromMap(built); !reflect.DeepEqual(got, want) {
		t.Fatalf("CauseFromMap(int depth) = %+v, want %+v", got, want)
	}
}

func TestCauseFromMapOfNothingIsARoot(t *testing.T) {
	for _, m := range []map[string]any{nil, {}, {"unrelated": true}} {
		if got := CauseFromMap(m); !got.IsZero() {
			t.Errorf("CauseFromMap(%v) = %+v, want the zero cause: a run with no recorded cause was a root", m, got)
		}
	}
}

// A depth is an ordering against the cap, so an out-of-range one saturates:
// a corrupt row's depth is refused, never wrapped into a small one that is
// admitted. A negative depth is no depth.
func TestCauseFromMapNarrowsTheDepthTowardTheCap(t *testing.T) {
	if got := CauseFromMap(map[string]any{"depth": 1e30}).Depth; got != math.MaxInt {
		t.Errorf("depth 1e30 read as %d, want it saturated at MaxInt", got)
	}
	if got := CauseFromMap(map[string]any{"depth": float64(-4)}).Depth; got != 0 {
		t.Errorf("depth -4 read as %d, want 0", got)
	}
	// A chain entry that is not an object names no run and is skipped.
	got := CauseFromMap(map[string]any{"depth": float64(1), "chain": []any{"junk", map[string]any{"automation": "a", "runId": "run-1"}}})
	if len(got.Chain) != 1 || got.Chain[0] != (Link{Automation: "a", RunId: "run-1"}) {
		t.Errorf("chain = %+v, want the one real link", got.Chain)
	}
}
