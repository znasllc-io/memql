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
	if !HoleNode("h1", "string").Equal(HoleNode("h1", "string")) {
		t.Fatal("the same hole must equal itself")
	}
	if HoleNode("h1", "string").Equal(HoleNode("h2", "string")) {
		t.Fatal("two templates open at different positions are different templates")
	}
	if HoleNode("h1", "string").Equal(Lit("x")) {
		t.Fatal("a hole is not a literal")
	}
}

// TestFormIsIgnoredByEqual pins what Form IS: a note on how canonicalization
// read a string (a command line, a path, a JSON document), kept so that
// Materialize can write the value back the way it arrived. It is never part of
// identity. Two recordings whose trees agree are the same action however their
// strings were spelled, and a Form inside Equal would stop them generalizing.
func TestFormIsIgnoredByEqual(t *testing.T) {
	argv := Arr(Lit("ls"), Lit("-la"))
	argv.Form = FormArgv
	path := Arr(Lit("ls"), Lit("-la"))
	path.Form = FormPath
	plain := Arr(Lit("ls"), Lit("-la"))
	if !argv.Equal(path) || !argv.Equal(plain) || !plain.Equal(argv) {
		t.Fatal("two trees that differ only in Form must be Equal: Form is a rendering hint")
	}
	obj := Obj(map[string]*Node{"a": Lit("1")})
	obj.Form = FormJSON
	if !obj.Equal(Obj(map[string]*Node{"a": Lit("1")})) {
		t.Fatal("an object's Form must not take part in equality either")
	}
}

// TestCloneCopiesTheForm: a template is built from clones of recorded trees,
// and a clone that dropped the Form would materialize a command line as an
// array.
func TestCloneCopiesTheForm(t *testing.T) {
	n := Obj(map[string]*Node{"command": Arr(Lit("git"), Lit("status"))})
	n.Form = FormJSON
	n.Kids[0].Form = FormArgv
	c := n.Clone()
	if c.Form != FormJSON || c.Kids[0].Form != FormArgv {
		t.Fatalf("Clone lost a Form: root %q, child %q", c.Form, c.Kids[0].Form)
	}
}
