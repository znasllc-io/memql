package work

import "testing"

func fpReg() Registry {
	return Registry{
		"readRows":   {ConstructKind: ConstructQuery},
		"writeThing": {ConstructKind: ConstructMutation, Concept: "v1:x:thing"},
		"writeOther": {ConstructKind: ConstructMutation, Concept: "v1:x:other"},
		"sendMail":   {ConstructKind: ConstructBuiltin, Effects: Footprint{External: true, Spend: true}},
		"writeFile":  {ConstructKind: ConstructBuiltin, Effects: Footprint{Files: true}},
		"onMachine":  {ConstructKind: ConstructBuiltin, Effects: Footprint{Machine: true, Files: true}},
		"composite":  {ConstructKind: ConstructLogic, Calls: []string{"writeThing", "sendMail", "readRows"}},
		"deep":       {ConstructKind: ConstructLogic, Calls: []string{"composite", "writeOther"}},
		"cycleA":     {ConstructKind: ConstructLogic, Calls: []string{"cycleB", "writeFile"}},
		"cycleB":     {ConstructKind: ConstructLogic, Calls: []string{"cycleA"}},
	}
}

func TestUnionFootprint_QueryHasNone(t *testing.T) {
	got := UnionFootprint([]string{"readRows"}, fpReg())
	if got.IsSideEffect() {
		t.Fatalf("a query has no effects; got %+v", got)
	}
	if len(got.Concepts) != 0 {
		t.Errorf("concepts = %v, want none", got.Concepts)
	}
}

func TestUnionFootprint_MutationEffectIsItsConcept(t *testing.T) {
	got := UnionFootprint([]string{"writeThing"}, fpReg())
	if len(got.Concepts) != 1 || got.Concepts[0] != "v1:x:thing" {
		t.Fatalf("concepts = %v, want [v1:x:thing]", got.Concepts)
	}
	if !got.IsSideEffect() {
		t.Error("writing a row is a side effect the safety gate must see")
	}
}

func TestUnionFootprint_UnionsTransitivelyAndDeduplicates(t *testing.T) {
	got := UnionFootprint([]string{"deep", "writeThing"}, fpReg())
	if !got.External || !got.Spend {
		t.Errorf("the union must carry sendMail's external+spend; got %+v", got)
	}
	want := map[string]bool{"v1:x:thing": true, "v1:x:other": true}
	if len(got.Concepts) != 2 {
		t.Fatalf("concepts = %v, want exactly the two, deduplicated", got.Concepts)
	}
	for _, c := range got.Concepts {
		if !want[c] {
			t.Errorf("unexpected concept %q", c)
		}
	}
}

func TestUnionFootprint_SortedSoTheRowIsStable(t *testing.T) {
	a := UnionFootprint([]string{"deep"}, fpReg())
	b := UnionFootprint([]string{"writeOther", "composite"}, fpReg())
	if len(a.Concepts) != len(b.Concepts) {
		t.Fatalf("%v vs %v", a.Concepts, b.Concepts)
	}
	for i := range a.Concepts {
		if a.Concepts[i] != b.Concepts[i] {
			t.Fatalf("the union must be order-independent and sorted, or the same step writes a different expectedFootprint each run: %v vs %v", a.Concepts, b.Concepts)
		}
	}
}

func TestUnionFootprint_TerminatesOnACycle(t *testing.T) {
	got := UnionFootprint([]string{"cycleA"}, fpReg())
	if !got.Files {
		t.Errorf("the cycle must not swallow writeFile's effect; got %+v", got)
	}
}

func TestFootprint_IsSideEffect(t *testing.T) {
	if (Footprint{}).IsSideEffect() {
		t.Error("an empty footprint touches nothing")
	}
	for _, f := range []Footprint{
		{Concepts: []string{"v1:x:y"}}, {Files: true}, {Machine: true}, {External: true}, {Spend: true},
	} {
		if !f.IsSideEffect() {
			t.Errorf("%+v is a side effect", f)
		}
	}
}
