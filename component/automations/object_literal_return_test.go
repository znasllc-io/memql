package automations

import (
	"reflect"
	"testing"
)

// object_literal_return_test.go -- DB-free guard for the #2274 fix: an
// object-literal logic return is evaluated locally (each value resolved against
// the evaluator, kept as a Go value) instead of being stringified back into a
// query. Covers literals, bare step refs, nested objects, and the empty object.

func TestTryEvaluateObjectLiteralLocally(t *testing.T) {
	ev := NewEvaluator()
	ev.SetStepResult("s", &StepResult{StepId: "s", Status: "success", Result: "hello"})

	t.Run("literals + step ref + nested", func(t *testing.T) {
		got, handled, err := tryEvaluateObjectLiteralLocally(
			`{ lit: "constant", flag: true, ref: s, nested: { k: "v" } }`, ev)
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v, want handled,nil", handled, err)
		}
		m, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("result is not a map: %T", got)
		}
		if m["lit"] != "constant" {
			t.Errorf("lit=%#v want \"constant\"", m["lit"])
		}
		if m["flag"] != true {
			t.Errorf("flag=%#v want true", m["flag"])
		}
		if m["ref"] != "hello" {
			t.Errorf("ref=%#v want \"hello\" (bare step ref did not resolve)", m["ref"])
		}
		if !reflect.DeepEqual(m["nested"], map[string]any{"k": "v"}) {
			t.Errorf("nested=%#v want map[k:v]", m["nested"])
		}
	})

	t.Run("empty object", func(t *testing.T) {
		got, handled, err := tryEvaluateObjectLiteralLocally(`{}`, ev)
		if err != nil || !handled {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if m, ok := got.(map[string]any); !ok || len(m) != 0 {
			t.Errorf("got %#v, want empty map", got)
		}
	})

	t.Run("non-object declines", func(t *testing.T) {
		for _, expr := range []string{`"a string"`, `42`, `s`, `coalesce(s, "x")`} {
			if _, handled, _ := tryEvaluateObjectLiteralLocally(expr, ev); handled {
				t.Errorf("%q: handled=true, want false (not an object literal)", expr)
			}
		}
	})
}
