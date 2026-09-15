package work

import (
	"strings"
	"testing"
)

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

// writesReg is a call graph whose mutations carry their writes: an update
// reached directly and through two logic layers, an insert reached through a
// logic that also calls the update, and a logic cycle around both.
func writesReg() Registry {
	return Registry{
		"advance": {ConstructKind: ConstructMutation, Concept: "v1:x:request", Write: &WriteSpec{
			Kind: "update", Fields: map[string]FieldValue{
				"status": {Arg: "status"},
				"stamp":  {Literal: "advance", HasLiteral: true},
			},
		}},
		"record": {ConstructKind: ConstructMutation, Concept: "v1:x:event", Write: &WriteSpec{Kind: "insert", NewRow: true}},
		"bare":   {ConstructKind: ConstructMutation, Concept: "v1:x:bare"},
		"orphan": {ConstructKind: ConstructMutation},
		"lookup": {ConstructKind: ConstructQuery},
		"send":   {ConstructKind: ConstructBuiltin, Effects: Footprint{External: true}},
		"route":  {ConstructKind: ConstructLogic, Calls: []string{"lookup", "advance", "send"}},
		"outer":  {ConstructKind: ConstructLogic, Calls: []string{"route", "record", "missing"}},
		"loopA":  {ConstructKind: ConstructLogic, Calls: []string{"loopB", "advance"}},
		"loopB":  {ConstructKind: ConstructLogic, Calls: []string{"loopA", "record"}},
	}
}

func writeKeys(ws []Write) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Concept + " " + w.Mutation + " via " + strings.Join(w.Path, " -> ")
	}
	return out
}

func TestUnionWrites_ADirectMutationIsItsOwnPath(t *testing.T) {
	got := UnionWrites([]string{"advance"}, writesReg())
	if len(got) != 1 {
		t.Fatalf("writes = %v, want the one update", writeKeys(got))
	}
	w := got[0]
	if w.Concept != "v1:x:request" || w.Mutation != "advance" || strings.Join(w.Path, ",") != "advance" {
		t.Fatalf("write = %+v, want v1:x:request by advance with the path [advance]", w)
	}
	if w.Spec.Kind != "update" || w.Spec.Fields["status"].Arg != "status" || w.Spec.Fields["stamp"].Literal != "advance" {
		t.Errorf("spec = %+v, want the mutation's own write", w.Spec)
	}
}

func TestUnionWrites_ThroughLogicCarriesThePath(t *testing.T) {
	got := writeKeys(UnionWrites([]string{"outer"}, writesReg()))
	want := []string{
		"v1:x:event record via outer -> record",
		"v1:x:request advance via outer -> route -> advance",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestUnionWrites_TerminatesOnALogicCycle(t *testing.T) {
	got := writeKeys(UnionWrites([]string{"loopA"}, writesReg()))
	want := []string{
		"v1:x:event record via loopA -> loopB -> record",
		"v1:x:request advance via loopA -> advance",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("a logic cycle must neither loop nor swallow a write:\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Each name reports the writes it reaches, so two names reaching one mutation
// give two paths, and the result is sorted whatever order the names came in:
// the graph built from it is printed in a refusal and must read the same on
// every boot.
func TestUnionWrites_SortedAndOrderIndependent(t *testing.T) {
	a := writeKeys(UnionWrites([]string{"route", "outer", "advance"}, writesReg()))
	b := writeKeys(UnionWrites([]string{"advance", "outer", "route"}, writesReg()))
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("the union depends on the order of the names:\n  %s\nvs\n  %s", strings.Join(a, "\n  "), strings.Join(b, "\n  "))
	}
	want := []string{
		"v1:x:event record via outer -> record",
		"v1:x:request advance via advance",
		"v1:x:request advance via outer -> route -> advance",
		"v1:x:request advance via route -> advance",
	}
	if strings.Join(a, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n  %s\nwant\n  %s", strings.Join(a, "\n  "), strings.Join(want, "\n  "))
	}
}

// A mutation that declares no write is still a write of its concept -- the
// graph reads an empty spec as a write it knows nothing about -- while one
// with no concept writes nothing the graph can name, as in UnionFootprint.
func TestUnionWrites_AMutationWithoutASpecOrAConcept(t *testing.T) {
	got := UnionWrites([]string{"bare", "orphan", "lookup", "send", "missing"}, writesReg())
	if len(got) != 1 || got[0].Concept != "v1:x:bare" || got[0].Spec.Kind != "" || got[0].Spec.Fields != nil {
		t.Fatalf("writes = %+v, want only v1:x:bare, with an empty spec", got)
	}
}

// The spec a Write carries is a copy: a caller that refines it (the graph
// resolves Arg fields against a call site) must not rewrite the registry.
func TestUnionWrites_SpecIsACopy(t *testing.T) {
	reg := writesReg()
	got := UnionWrites([]string{"advance"}, reg)
	got[0].Spec.Fields["status"] = FieldValue{Literal: "done", HasLiteral: true}
	if reg["advance"].Write.Fields["status"].HasLiteral {
		t.Fatal("editing a Write's spec edited the registry's")
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
