package sense

import "testing"

// TestCompleteTopLevelOffersLiveConstructs verifies the headline A2 win: the
// top-level completion now offers every author-facing construct keyword from
// the DSL spec, and no longer the stale `func` (internal rewriter target) or
// the retired `has` operator.
func TestCompleteTopLevelOffersLiveConstructs(t *testing.T) {
	s := New(nil)
	labels := map[string]bool{}
	for _, it := range s.completeTopLevel("") {
		labels[it.Label] = true
	}
	for _, kw := range []string{
		"concept", "query", "mutate", "logic", "automation", "action",
		"capability", "spec", "trait", "shape", "tool", "prompt", "provider",
		"builtin", "policy", "seed", "use",
	} {
		if !labels[kw] {
			t.Errorf("completeTopLevel missing live construct %q", kw)
		}
	}
	if labels["func"] {
		t.Error("completeTopLevel should not offer `func` (internal rewriter target, not an author word)")
	}
	if labels["has"] {
		t.Error("completeTopLevel should not offer the retired `has` operator")
	}
}

// TestCompleteTopLevelPrefixFilter confirms prefix filtering still applies --
// typing `mut` offers `mutate` (the declaration keyword) and nothing unrelated.
func TestCompleteTopLevelPrefixFilter(t *testing.T) {
	s := New(nil)
	got := map[string]bool{}
	for _, it := range s.completeTopLevel("mut") {
		if it.Kind == "keyword" {
			got[it.Label] = true
		}
	}
	if !got["mutate"] {
		t.Error("prefix `mut` should offer `mutate`")
	}
	if got["query"] || got["concept"] {
		t.Error("prefix `mut` should not offer non-matching constructs")
	}
}

// TestKeywordsProjectedFromSpec confirms the package-level Keywords list is the
// spec projection: it carries the constructs and drops the retired `has`.
func TestKeywordsProjectedFromSpec(t *testing.T) {
	kw := map[string]bool{}
	for _, k := range Keywords {
		kw[k] = true
	}
	for _, want := range []string{"mutate", "logic", "trait", "use", "in"} {
		if !kw[want] {
			t.Errorf("Keywords missing %q", want)
		}
	}
	if kw["has"] {
		t.Error("Keywords must not contain the retired `has` operator")
	}
}

// TestFieldTypesExcludeDeprecated confirms FieldTypes is spec-driven and omits
// the deprecated `array` spelling while keeping the live scalar/shape types.
func TestFieldTypesExcludeDeprecated(t *testing.T) {
	ft := map[string]bool{}
	for _, f := range FieldTypes {
		ft[f] = true
	}
	for _, want := range []string{"string", "int", "float", "bool", "datetime", "object", "enum"} {
		if !ft[want] {
			t.Errorf("FieldTypes missing %q", want)
		}
	}
	if ft["array"] {
		t.Error("FieldTypes must not offer the deprecated `array` spelling (use []T)")
	}
}
