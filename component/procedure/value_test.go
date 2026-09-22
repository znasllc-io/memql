package procedure

import "testing"

func TestNodeEqual_IsStructuralAndOrderIndependentForObjects(t *testing.T) {
	a := Obj(map[string]*Node{"b": Lit("2"), "a": Lit("1")})
	b := Obj(map[string]*Node{"a": Lit("1"), "b": Lit("2")})
	if !a.Equal(b) {
		t.Fatal("two objects with the same keys and values must be equal regardless of insertion order")
	}
}

func TestNodeEqual_ArraysAreOrderSensitive(t *testing.T) {
	a := Arr(Lit("1"), Lit("2"))
	b := Arr(Lit("2"), Lit("1"))
	if a.Equal(b) {
		t.Fatal("an array is a sequence: [1,2] must not equal [2,1]")
	}
}

func TestNodeSize_CountsEveryNodeSoCompressionHasAUnit(t *testing.T) {
	// {a: 1, b: [2, 3]} -- object + a's value + array + 2 elements = 5,
	// plus the object itself already counted: 1+1+1+1+1 = 5.
	n := Obj(map[string]*Node{"a": Lit("1"), "b": Arr(Lit("2"), Lit("3"))})
	if got := n.Size(); got != 5 {
		t.Fatalf("Size() = %d, want 5 -- the score's unit must count the whole tree", got)
	}
}

func TestNodeAt_WalksAPathAndReportsAMiss(t *testing.T) {
	n := Obj(map[string]*Node{"a": Obj(map[string]*Node{"b": Lit("x")})})
	got, ok := n.At([]string{"a", "b"})
	if !ok || got.Lit != "x" {
		t.Fatalf("At(a.b) = %v, %v; want the literal x", got, ok)
	}
	if _, ok := n.At([]string{"a", "zzz"}); ok {
		t.Fatal("At must report a miss rather than an empty node -- absent and empty are different answers")
	}
}

func TestNodeAt_IndexesIntoAnArray(t *testing.T) {
	n := Obj(map[string]*Node{"argv": Arr(Lit("grep"), Lit("-n"))})
	got, ok := n.At([]string{"argv", "1"})
	if !ok || got.Lit != "-n" {
		t.Fatalf("At(argv.1) = %v, %v; want -n", got, ok)
	}
	if _, ok := n.At([]string{"argv", "9"}); ok {
		t.Fatal("an out-of-range index is a miss")
	}
}

func TestNodeEqual_AHoleEqualsOnlyTheSameHole(t *testing.T) {
	if !Hole("h1", "string").Equal(Hole("h1", "string")) {
		t.Fatal("the same hole must equal itself")
	}
	if Hole("h1", "string").Equal(Hole("h2", "string")) {
		t.Fatal("two templates open at different positions are different templates")
	}
	if Hole("h1", "string").Equal(Lit("x")) {
		t.Fatal("a hole is not a literal")
	}
}
