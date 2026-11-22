package common

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A route choice rides the run ROW between nodes, so the only thing that
// crosses the hop is its map form: what one node writes, the next must read
// back unchanged, and a row without one must read as Auto.
func TestRouteChoiceSurvivesItsRowForm(t *testing.T) {
	choice := RouteChoice{Source: "app:claude-code", Level: "strong", Destination: "computer", By: "v1:identity:user:alice", NoPaidProviders: true}

	// Through JSON, because that is how the row comes back: a decoded payload,
	// never the Go value that was written.
	raw, err := json.Marshal(choice.Map())
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := RouteChoiceFrom(decoded); got != choice {
		t.Fatalf("round trip = %+v, want %+v", got, choice)
	}

	// Auto writes NOTHING: a map with empty strings in it would be a choice
	// that says "the rules decide" in three fields instead of zero.
	if m := (RouteChoice{}).Map(); m != nil {
		t.Fatalf("an Auto choice rendered %v, want nil", m)
	}
	for _, absent := range []any{nil, map[string]any{}, "app:claude-code", map[string]any{"source": 7}} {
		if got := RouteChoiceFrom(absent); !got.IsZero() {
			t.Fatalf("RouteChoiceFrom(%#v) = %+v, want Auto", absent, got)
		}
	}
}

// A goal a visitor's request opened is marked on its rows (design record
// 2026-11-21-vendor-accounts-and-api-keys, K8 line 2), and the mark is a choice
// of nobody's: it must survive the row form alone, with no source and no level.
func TestRouteChoiceCarriesTheVisitorMarkAlone(t *testing.T) {
	choice := RouteChoice{By: "u-merchant", ForVisitor: true}
	if choice.IsZero() {
		t.Fatal("a visitor's mark alone reads as no choice, so it would never be written to the row")
	}
	raw, err := json.Marshal(choice.Map())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"by":"u-merchant","forVisitor":true}` {
		t.Fatalf("row form = %s", raw)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := RouteChoiceFrom(decoded); got != choice {
		t.Fatalf("round trip = %+v, want %+v", got, choice)
	}
	// Only the literal true is the mark.
	for _, not := range []any{map[string]any{"forVisitor": "true"}, map[string]any{"forVisitor": 1}} {
		if got := RouteChoiceFrom(not); got.ForVisitor {
			t.Fatalf("RouteChoiceFrom(%#v) read a visitor's mark", not)
		}
	}
}

// A choice made through a credential that acts as a person without being them
// (an app session, delegated access) says so on the row, because the run that
// honours it executes later on a context that no longer can.
func TestARouteChoiceRemembersItWasNotMadeInPerson(t *testing.T) {
	choice := RouteChoice{Source: "federation:strongest", By: "u-ana", ByMachine: true}
	raw, err := json.Marshal(choice.Map())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"by":"u-ana","byMachine":true,"source":"federation:strongest"}` {
		t.Fatalf("row form = %s", raw)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := RouteChoiceFrom(decoded); got != choice {
		t.Fatalf("round trip = %+v, want %+v", got, choice)
	}
	// A person's own choice writes nothing extra, so every row written before
	// the mark existed still reads as a person's.
	own, _ := json.Marshal(RouteChoice{Source: "federation:strongest", By: "u-ana"}.Map())
	if string(own) != `{"by":"u-ana","source":"federation:strongest"}` {
		t.Fatalf("a person's own choice is stored as %s", own)
	}
	// Only the literal true is the mark.
	for _, not := range []any{map[string]any{"source": "route:local", "byMachine": "true"}, map[string]any{"source": "route:local", "byMachine": 1}} {
		if got := RouteChoiceFrom(not); got.ByMachine {
			t.Fatalf("RouteChoiceFrom(%#v) read the mark", not)
		}
	}
}

// The same mark on a re-run's override, read back from the recorded form.
func TestAStepOverrideRemembersItWasNotMadeInPerson(t *testing.T) {
	got := StepOverrideFromRecord(map[string]any{"model": "federation:claudeSonnet", "requestedBy": "u-ana", "byMachine": true})
	if got == nil || !got.ByMachine || got.RequestedBy != "u-ana" {
		t.Fatalf("the recorded override lost its mark: %+v", got)
	}
	if own := StepOverrideFromRecord(map[string]any{"model": "federation:claudeSonnet", "requestedBy": "u-ana"}); own == nil || own.ByMachine {
		t.Fatalf("a person's own re-run reads as a machine's: %+v", own)
	}
	if odd := StepOverrideFromRecord(map[string]any{"model": "m", "byMachine": "true"}); odd == nil || odd.ByMachine {
		t.Fatalf("a mark that is not the literal true was read: %+v", odd)
	}
}

// The choice is part of the run a context names, so the one value a step's
// context carries is the one every model call it makes reads.
func TestTheRunContextCarriesTheRouteChoice(t *testing.T) {
	choice := RouteChoice{Source: "policy:localFirst", Level: "fast", By: "alice"}
	ctx := ContextWithRun(nil, RunContext{RunId: "v1:work:run:r1", Routing: choice})
	rc, ok := RunFromContext(ctx)
	if !ok || !reflect.DeepEqual(rc.Routing, choice) {
		t.Fatalf("run context routing = %+v (ok=%v), want %+v", rc.Routing, ok, choice)
	}
}
