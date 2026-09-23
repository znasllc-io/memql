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

// TestAntiUnifyKeepsTheLeftForm: the generalization of two command lines is a
// command line. Without the Form a template's argv would materialize as a
// JSON array, and the replay would hand a shell a list.
func TestAntiUnifyKeepsTheLeftForm(t *testing.T) {
	a := Arr(Lit("go"), Lit("test"), Lit("./a"))
	a.Form = FormArgv
	b := Arr(Lit("go"), Lit("test"), Lit("./b"))
	b.Form = FormPath
	g, _ := AntiUnify(a, b, newHoleNamer())
	if g.Form != FormArgv {
		t.Fatalf("array: Form = %q, want the left operand's %q", g.Form, FormArgv)
	}

	oa := Obj(map[string]*Node{"x": Lit("1")})
	oa.Form = FormJSON
	ob := Obj(map[string]*Node{"x": Lit("2")})
	og, _ := AntiUnify(oa, ob, newHoleNamer())
	if og.Form != FormJSON {
		t.Fatalf("object: Form = %q, want the left operand's %q", og.Form, FormJSON)
	}

	// Through Generalize, which is where it matters: the recorded command is
	// still a command line after its instances were anti-unified.
	tmpl := Generalize([][]Action{
		Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{"command": "echo hi > a.txt"}}}),
		Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{"command": "echo hi > b.txt"}}}),
	})
	cmd, _ := tmpl.Steps[0].Args.At([]string{"command"})
	if cmd.Form != FormArgv {
		t.Fatalf("a generalized command must still be a command line; Form = %q", cmd.Form)
	}
}

// TestAntiUnifyKeepsTheTypeOfALiteralBothSidesAgreeOn: a number both
// instances agree on is still a number in the template. A template that
// forgot it would render and materialize it as a string, and a tool whose
// schema wants an integer refuses the replay's call.
func TestAntiUnifyKeepsTheTypeOfALiteralBothSidesAgreeOn(t *testing.T) {
	a := Obj(map[string]*Node{"retries": LitOf("3", "number"), "name": Lit("a")})
	b := Obj(map[string]*Node{"retries": LitOf("3", "number"), "name": Lit("b")})
	g, _ := AntiUnify(a, b, newHoleNamer())
	n, _ := g.At([]string{"retries"})
	if n.Kind != KindLit || n.LitType != "number" {
		t.Fatalf("an agreed number must stay a number; got %+v", n)
	}
}

// TestAHoleIsTypedByWhatBothSidesObserved is HoleType's own contract -- "the
// widest type observed at this position" -- which the renderer and
// Materialize both read. Two numbers open a number hole; a number against a
// string is only a string; and a third instance keeps the type rather than
// collapsing it to "mixed", because a hole meeting one more literal of its own
// type has observed nothing wider.
func TestAHoleIsTypedByWhatBothSidesObserved(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b *Node
		want string
	}{
		{"two numbers", LitOf("1", "number"), LitOf("2", "number"), "number"},
		{"two bools", LitOf("true", "bool"), LitOf("false", "bool"), "bool"},
		{"a number and a string", LitOf("1", "number"), Lit("x"), "string"},
		{"two strings", Lit("a"), Lit("b"), "string"},
		{"a string hole meeting a string", HoleNode("h9", "string"), Lit("c"), "string"},
		{"a number hole meeting a number", HoleNode("h9", "number"), LitOf("7", "number"), "number"},
		{"a scalar and an object", Lit("a"), Obj(map[string]*Node{"k": Lit("v")}), "mixed"},
	} {
		g, _ := AntiUnify(c.a, c.b, newHoleNamer())
		if g.Kind != KindHole || g.HoleType != c.want {
			t.Errorf("%s: got %+v, want a %q hole", c.name, g, c.want)
		}
	}

	three := [][]Action{
		{{Tool: "mcp", Args: Obj(map[string]*Node{"limit": LitOf("10", "number")})}},
		{{Tool: "mcp", Args: Obj(map[string]*Node{"limit": LitOf("20", "number")})}},
		{{Tool: "mcp", Args: Obj(map[string]*Node{"limit": LitOf("30", "number")})}},
	}
	tmpl := Generalize(three)
	if len(tmpl.Holes) != 1 || tmpl.Holes[0].Type != "number" {
		t.Fatalf("three numbers must open one number hole; got %+v", tmpl.Holes)
	}
}
