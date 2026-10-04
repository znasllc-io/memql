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
	choice := RouteChoice{Source: "app:claude-code", Level: "strong", By: "v1:identity:user:alice"}

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
