package airoute

import (
	"context"
	"reflect"
	"testing"
)

func TestContextReferencesFollowActualProviderAttempts(t *testing.T) {
	var events []CallObservation
	ctx := WithObserver(context.Background(), func(event CallObservation) { events = append(events, event) })
	refs := []string{"training-selection-receipt"}
	attempt := WithContextReferences(ctx, refs)
	refs[0] = "mutated"
	Observe(attempt, CallObservation{ID: "call1", Phase: "failed", Model: "requested-model"})
	Observe(attempt, CallObservation{ID: "call2", Phase: "completed", Model: "fallback-model"})
	Observe(ctx, CallObservation{ID: "unrelated", Phase: "completed"})
	for _, e := range events[:2] {
		if !reflect.DeepEqual(e.ContextReferences, []string{"training-selection-receipt"}) {
			t.Fatal(e)
		}
	}
	if len(events[2].ContextReferences) != 0 {
		t.Fatal("context leaked to another request")
	}
}
