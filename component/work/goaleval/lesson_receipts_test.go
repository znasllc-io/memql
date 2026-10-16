package goaleval

import (
	"reflect"
	"testing"
)

func TestLessonBindingRequiresSuccessfulDeliveryAndKeepsExactRevision(t *testing.T) {
	selected := map[string]any{"id": "v1:work:observation:selection", "data": map[string]any{"trainingSelection": map[string]any{"selected": []any{map[string]any{"lessonId": "lesson-one", "lessonVersion": 2, "lessonDigest": "exact-digest"}}}}}
	call := func(phase, model string) map[string]any {
		return map[string]any{"data": map[string]any{"execution": map[string]any{"kind": "model", "call": map[string]any{"phase": phase, "model": model, "contextReferences": []any{"selection"}}}}}
	}
	if len(DeliveredLessonBindings([]map[string]any{selected})) != 0 {
		t.Fatal("selection was called delivery")
	}
	if len(DeliveredLessonBindings([]map[string]any{selected, call("failed", "requested-model")})) != 0 {
		t.Fatal("failure was called successful delivery")
	}
	got := DeliveredLessonBindings([]map[string]any{selected, call("failed", "requested-model"), call("completed", "fallback-model")})
	if !reflect.DeepEqual([]Binding{{ID: "lesson-one", Version: "2", Digest: "exact-digest", Source: "observation:selection"}}, got) {
		t.Fatalf("bindings: %#v", got)
	}
	if len(DeliveredLessonBindings([]map[string]any{selected, selected, call("completed", "fallback-model")})) != 1 {
		t.Fatal("duplicate binding")
	}
}
