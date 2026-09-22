package procedure

import "testing"

// inst builds one instance: a sequence of actions with the given tools and
// argument trees, numbered in order.
func inst(pairs ...any) []Action {
	var out []Action
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Action{
			Tool: pairs[i].(string),
			Args: pairs[i+1].(*Node),
			Seq:  i / 2,
			Key:  "call" + string(rune('0'+i/2)),
		})
	}
	return out
}

func holeByPathKey(holes []Hole, step int, key string) (Hole, bool) {
	for _, h := range holes {
		if h.StepIndex == step && len(h.Path) == 1 && h.Path[0] == key {
			return h, true
		}
	}
	return Hole{}, false
}

func TestGeneralize_TwoInstancesDifferingInOneLiteralYieldOneHole(t *testing.T) {
	a := inst("exec", Obj(map[string]*Node{"target": Lit("a.txt")}))
	b := inst("exec", Obj(map[string]*Node{"target": Lit("b.txt")}))
	tmpl := Generalize([][]Action{a, b})
	if len(tmpl.Steps) != 1 {
		t.Fatalf("got %d steps, want 1", len(tmpl.Steps))
	}
	n, ok := tmpl.Steps[0].Args.At([]string{"target"})
	if !ok || n.Kind != KindHole {
		t.Fatalf("the differing literal must be a hole; got %v", n)
	}
}

// TestClassify_ALiteralEqualToAnEarlierResultYieldsADataFlowHole is issue
// #5405's first acceptance criterion.
func TestClassify_ALiteralEqualToAnEarlierResultYieldsADataFlowHole(t *testing.T) {
	mk := func(id string) []Action {
		return []Action{
			{Tool: "exec", Args: Obj(map[string]*Node{"cmd": Lit("mkid")}), Seq: 0,
				ResultValue: map[string]any{"id": id}},
			{Tool: "fs_write", Args: Obj(map[string]*Node{"name": Lit(id)}), Seq: 1},
		}
	}
	instances := [][]Action{mk("alpha"), mk("beta")}
	tmpl := Generalize(instances)
	holes := Classify(tmpl, instances)
	h, ok := holeByPathKey(holes, 1, "name")
	if !ok {
		t.Fatalf("expected a hole at step 1 key name; got %+v", holes)
	}
	if h.Class != HoleDataFlow {
		t.Fatalf("class = %q, want %q -- the value came from step 0's result in every instance",
			h.Class, HoleDataFlow)
	}
	if h.Ref == nil || h.Ref.StepIndex != 0 {
		t.Fatalf("the reference must name step 0; got %+v", h.Ref)
	}
}

func TestClassify_AValueConstantAcrossEveryInstanceIsKeptLiteral(t *testing.T) {
	mk := func(v string) []Action {
		return []Action{{Tool: "exec", Args: Obj(map[string]*Node{
			"env":    Lit("prod"),
			"target": Lit(v),
		}), Seq: 0}}
	}
	instances := [][]Action{mk("a"), mk("b")}
	tmpl := Generalize(instances)
	if _, ok := holeByPathKey(Classify(tmpl, instances), 0, "env"); ok {
		t.Fatal("a value identical in every instance is not a hole at all -- it stays literal")
	}
}

// TestClassify_DataFlowBeatsConstantWhenBothFit is D13's ORDER, stated as the
// case that would falsify it.
func TestClassify_DataFlowBeatsConstantWhenBothFit(t *testing.T) {
	mk := func() []Action {
		return []Action{
			{Tool: "exec", Args: Obj(map[string]*Node{"cmd": Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"out": "same"}},
			{Tool: "fs_write", Args: Obj(map[string]*Node{"name": Lit("same"), "other": Lit("q")}), Seq: 1},
		}
	}
	// The two instances differ elsewhere so a template with holes exists,
	// but `name` is both constant AND equal to step 0's result.
	i1, i2 := mk(), mk()
	i2[1].Args = Obj(map[string]*Node{"name": Lit("same"), "other": Lit("r")})
	instances := [][]Action{i1, i2}
	tmpl := Generalize(instances)
	holes := ClassifyAll(tmpl, instances)
	h, ok := holeByPathKey(holes, 1, "name")
	if !ok {
		t.Fatalf("expected `name` to be considered; got %+v", holes)
	}
	if h.Class != HoleDataFlow {
		t.Fatalf("class = %q, want %q: the equality is the explanation and the constancy is a "+
			"coincidence of a thin corpus", h.Class, HoleDataFlow)
	}
}

func TestClassify_AVaryingValueWithNoDerivationIsFree(t *testing.T) {
	mk := func(v string) []Action {
		return []Action{{Tool: "exec", Args: Obj(map[string]*Node{"target": Lit(v)}), Seq: 0}}
	}
	instances := [][]Action{mk("alpha"), mk("beta")}
	tmpl := Generalize(instances)
	h, ok := holeByPathKey(Classify(tmpl, instances), 0, "target")
	if !ok {
		t.Fatalf("expected a hole; got %+v", Classify(tmpl, instances))
	}
	if h.Class != HoleFree {
		t.Fatalf("class = %q, want %q", h.Class, HoleFree)
	}
}

// TestClassify_AClassificationHoldingOnSomeInstancesIsNotRecordedAsExplained
// is the NEGATIVE CONTROL for Evidence, and the over-generalization failure
// mode the record names first.
func TestClassify_AClassificationHoldingOnSomeInstancesIsNotRecordedAsExplained(t *testing.T) {
	derived := []Action{
		{Tool: "exec", Args: Obj(map[string]*Node{"cmd": Lit("x")}), Seq: 0,
			ResultValue: map[string]any{"id": "alpha"}},
		{Tool: "fs_write", Args: Obj(map[string]*Node{"name": Lit("alpha")}), Seq: 1},
	}
	notDerived := []Action{
		{Tool: "exec", Args: Obj(map[string]*Node{"cmd": Lit("x")}), Seq: 0,
			ResultValue: map[string]any{"id": "beta"}},
		{Tool: "fs_write", Args: Obj(map[string]*Node{"name": Lit("unrelated")}), Seq: 1},
	}
	instances := [][]Action{derived, notDerived}
	tmpl := Generalize(instances)
	h, ok := holeByPathKey(Classify(tmpl, instances), 1, "name")
	if !ok {
		t.Fatalf("expected a hole at step 1 name")
	}
	if h.Class == HoleDataFlow {
		t.Fatal("a derivation that holds on ONE of two instances must not be recorded as data flow")
	}
	if h.Class != HoleUnexplained {
		t.Fatalf("class = %q, want %q -- partial evidence is what earns the one bounded model call",
			h.Class, HoleUnexplained)
	}
	if h.Evidence != 1 {
		t.Fatalf("Evidence = %d, want 1", h.Evidence)
	}
}

// TestClassify_AHoleWithNoDerivationEvidenceIsFreeAndNotUnexplained is the
// gate on the model call. Without it every free parameter costs a call, and
// D6's "induction spends no model" stops being true in practice.
func TestClassify_AHoleWithNoDerivationEvidenceIsFreeAndNotUnexplained(t *testing.T) {
	mk := func(v string) []Action {
		return []Action{
			{Tool: "exec", Args: Obj(map[string]*Node{"cmd": Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"id": "never-referenced"}},
			{Tool: "fs_write", Args: Obj(map[string]*Node{"name": Lit(v)}), Seq: 1},
		}
	}
	instances := [][]Action{mk("p"), mk("q")}
	tmpl := Generalize(instances)
	h, _ := holeByPathKey(Classify(tmpl, instances), 1, "name")
	if h.Class != HoleFree {
		t.Fatalf("class = %q, want %q -- no instance derives it, so there is nothing to ask about",
			h.Class, HoleFree)
	}
}

func TestClassify_IsDeterministicInHoleOrder(t *testing.T) {
	mk := func(v, w string) []Action {
		return []Action{{Tool: "exec", Args: Obj(map[string]*Node{"a": Lit(v), "b": Lit(w)}), Seq: 0}}
	}
	instances := [][]Action{mk("1", "2"), mk("3", "4")}
	tmpl := Generalize(instances)
	first, second := Classify(tmpl, instances), Classify(tmpl, instances)
	if len(first) != len(second) {
		t.Fatalf("%d holes then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Id != second[i].Id || first[i].Class != second[i].Class {
			t.Fatalf("hole %d differs between runs", i)
		}
	}
}
