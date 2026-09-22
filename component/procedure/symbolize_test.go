package procedure

import "testing"

func argsOf(cmd string) *Node { return Obj(map[string]*Node{"command": Arr(litsOf(cmd)...)}) }

func litsOf(cmd string) []*Node { return splitArgv(cmd) }

// TestSymbolize_TwoUnrelatedToolsNeverShareASymbol is issue #5403's first
// acceptance criterion, and the record's own failure mode: a budget too loose
// collapses everything into one cluster. Grouping by TOOL first is what makes
// this true by construction rather than by tuning, so the test asserts it at
// a budget of zero and at a budget larger than either tree.
func TestSymbolize_TwoUnrelatedToolsNeverShareASymbol(t *testing.T) {
	actions := []Action{
		{Tool: "exec", Args: Obj(map[string]*Node{"x": Lit("1")}), Seq: 0},
		{Tool: "fs_write", Args: Obj(map[string]*Node{"x": Lit("1")}), Seq: 1},
	}
	for _, budget := range []int{0, 1, 2, 1000} {
		p := DefaultParams()
		p.SymbolBudget = budget
		syms := Symbolize(actions, p)
		if len(syms) != 2 {
			t.Fatalf("budget %d: got %d symbols, want 2 -- identical arguments under different tools "+
				"must never share a symbol at ANY budget", budget, len(syms))
		}
	}
}

func TestSymbolize_TwoTracesDifferingOnlyInALiteralAreOneCluster(t *testing.T) {
	actions := []Action{
		{Tool: "exec", Args: argsOf("grep -n foo a.txt"), Seq: 0},
		{Tool: "exec", Args: argsOf("grep -n foo b.txt"), Seq: 1},
	}
	syms := Symbolize(actions, DefaultParams())
	if len(syms) != 1 {
		t.Fatalf("got %d symbols, want 1 -- one differing literal is inside the budget", len(syms))
	}
	if len(syms[0].Members) != 2 {
		t.Fatalf("the cluster holds %d members, want 2", len(syms[0].Members))
	}
}

// TestSymbolize_ABudgetOfZeroClustersOnlyIdenticalActions is the NEGATIVE
// CONTROL for the budget: without it, a Symbolize that ignored distance
// entirely would still pass the test above.
func TestSymbolize_ABudgetOfZeroClustersOnlyIdenticalActions(t *testing.T) {
	p := DefaultParams()
	p.SymbolBudget = 0
	actions := []Action{
		{Tool: "exec", Args: argsOf("grep -n foo a.txt"), Seq: 0},
		{Tool: "exec", Args: argsOf("grep -n foo b.txt"), Seq: 1},
		{Tool: "exec", Args: argsOf("grep -n foo a.txt"), Seq: 2},
	}
	syms := Symbolize(actions, p)
	if len(syms) != 2 {
		t.Fatalf("got %d symbols, want 2 -- at budget 0 only identical actions cluster", len(syms))
	}
}

func TestAntiUnify_ArraysPairByLongestCommonSubsequence(t *testing.T) {
	a := Arr(Lit("a"), Lit("b"), Lit("c"))
	b := Arr(Lit("a"), Lit("x"), Lit("b"), Lit("c"))
	_, dist := AntiUnify(a, b, newHoleNamer())
	if dist != 1 {
		t.Fatalf("distance = %d, want 1 -- one INSERTED element is one difference. "+
			"Positional pairing would shift every later element and report 3", dist)
	}
}

func TestAntiUnify_ObjectsPairByKeyNotByPosition(t *testing.T) {
	a := Obj(map[string]*Node{"alpha": Lit("1"), "zeta": Lit("2")})
	b := Obj(map[string]*Node{"zeta": Lit("2"), "alpha": Lit("1")})
	_, dist := AntiUnify(a, b, newHoleNamer())
	if dist != 0 {
		t.Fatalf("distance = %d, want 0 -- key order is a spelling, not a difference", dist)
	}
}

func TestAntiUnify_AKeyPresentOnOneSideOnlyIsAHole(t *testing.T) {
	a := Obj(map[string]*Node{"x": Lit("1"), "y": Lit("2")})
	b := Obj(map[string]*Node{"x": Lit("1")})
	g, dist := AntiUnify(a, b, newHoleNamer())
	if dist == 0 {
		t.Fatal("a key one side lacks is a difference")
	}
	n, ok := g.At([]string{"y"})
	if !ok || n.Kind != KindHole {
		t.Fatalf("the one-sided key must generalize to a hole; got %v, %v", n, ok)
	}
}

func TestAntiUnify_IsCommutativeInDistance(t *testing.T) {
	// A clustering that depended on read order would give two replicas two
	// different symbol sets from one corpus.
	a := Obj(map[string]*Node{"p": Arr(Lit("1"), Lit("2")), "q": Lit("x")})
	b := Obj(map[string]*Node{"p": Arr(Lit("1")), "r": Lit("y")})
	_, ab := AntiUnify(a, b, newHoleNamer())
	_, ba := AntiUnify(b, a, newHoleNamer())
	if ab != ba {
		t.Fatalf("Distance(a,b) = %d but Distance(b,a) = %d", ab, ba)
	}
}

func TestAntiUnify_GeneralizingAWholeSubtreeCostsMoreThanOneLiteral(t *testing.T) {
	small := Obj(map[string]*Node{"k": Lit("a")})
	smallB := Obj(map[string]*Node{"k": Lit("b")})
	_, cheap := AntiUnify(small, smallB, newHoleNamer())

	big := Obj(map[string]*Node{"k": Obj(map[string]*Node{"a": Lit("1"), "b": Lit("2"), "c": Lit("3")})})
	bigB := Obj(map[string]*Node{"k": Lit("scalar")})
	_, dear := AntiUnify(big, bigB, newHoleNamer())

	if !(dear > cheap) {
		t.Fatalf("generalizing a subtree (%d) must cost more than one literal (%d)", dear, cheap)
	}
}

func TestSymbolize_ClusterTemplateIsTheGeneralizationOfEveryMember(t *testing.T) {
	// Re-anti-unifying on each join is what keeps the template true of the
	// whole cluster rather than of its first two members.
	actions := []Action{
		{Tool: "exec", Args: argsOf("go test ./a"), Seq: 0},
		{Tool: "exec", Args: argsOf("go test ./b"), Seq: 1},
		{Tool: "exec", Args: argsOf("go test ./c"), Seq: 2},
	}
	syms := Symbolize(actions, DefaultParams())
	if len(syms) != 1 {
		t.Fatalf("got %d symbols, want 1", len(syms))
	}
	n, ok := syms[0].Template.At([]string{"command", "2"})
	if !ok || n.Kind != KindHole {
		t.Fatalf("the varying argv position must be a hole in the cluster template; got %v", n)
	}
}

func TestSymbolize_IdsAreStableAndDeterministic(t *testing.T) {
	actions := []Action{
		{Tool: "exec", Args: argsOf("ls"), Seq: 0},
		{Tool: "fs_write", Args: Obj(map[string]*Node{"path": Lit("x")}), Seq: 1},
		{Tool: "exec", Args: argsOf("ls"), Seq: 2},
	}
	first := Symbolize(actions, DefaultParams())
	second := Symbolize(actions, DefaultParams())
	if len(first) != len(second) {
		t.Fatalf("%d symbols then %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Id != second[i].Id || first[i].Tool != second[i].Tool {
			t.Fatalf("symbol %d differs between runs: %+v vs %+v", i, first[i], second[i])
		}
	}
}
