package procedure

import "testing"

func derivationInstances(names ...string) [][]Action {
	var out [][]Action
	for _, n := range names {
		out = append(out, []Action{
			{Tool: "exec", Args: Obj(map[string]*Node{"cmd": Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"path": "/srv/" + n + ".txt"}},
			{Tool: "fs_write", Args: Obj(map[string]*Node{"name": Lit(n + ".txt")}), Seq: 1},
		})
	}
	return out
}

func holeAt(step int, key string) Hole {
	return Hole{Id: "h", StepIndex: step, Path: []string{key}, Type: "string"}
}

func TestCheckDerivation_AProposalHoldingOnEveryInstanceIsKept(t *testing.T) {
	d, err := ParseDerivation(`basename(ref(0, "path"))`)
	if err != nil {
		t.Fatalf("ParseDerivation: %v", err)
	}
	ok, held := CheckDerivation(d, holeAt(1, "name"), derivationInstances("alpha", "beta", "gamma"))
	if !ok || held != 3 {
		t.Fatalf("ok=%v held=%d; want true, 3", ok, held)
	}
}

// TestCheckDerivation_AProposalHoldingOnSomeInstancesIsRejected is issue
// #5405's third acceptance criterion, verbatim from the record's failure-mode
// list: "the model fallback that proposes a derivation holding on some
// instances and not others is rejected, and the hole stays free".
func TestCheckDerivation_AProposalHoldingOnSomeInstancesIsRejected(t *testing.T) {
	instances := derivationInstances("alpha", "beta")
	// Break the second instance's correspondence.
	instances[1][1].Args = Obj(map[string]*Node{"name": Lit("something-else.txt")})

	d, err := ParseDerivation(`basename(ref(0, "path"))`)
	if err != nil {
		t.Fatalf("ParseDerivation: %v", err)
	}
	ok, held := CheckDerivation(d, holeAt(1, "name"), instances)
	if ok {
		t.Fatal("a derivation that explains one instance of two must be REJECTED")
	}
	if held != 1 {
		t.Fatalf("held = %d, want 1 -- the count is what lets a rejection be read", held)
	}
}

func TestCheckDerivation_AnUnparseableProposalIsARejectionAndNotAnError(t *testing.T) {
	// A model that answers with prose must cost the run nothing: the hole
	// stays free and the procedure is still learned.
	if _, err := ParseDerivation("I think it is the file name"); err == nil {
		t.Fatal("prose must not parse")
	}
	var zero Derivation
	ok, held := CheckDerivation(zero, holeAt(1, "name"), derivationInstances("a", "b"))
	if ok || held != 0 {
		t.Fatalf("an empty derivation must hold on nothing; ok=%v held=%d", ok, held)
	}
}

// TestParseDerivation_RefusesAnythingOutsideTheClosedGrammar is the NEGATIVE
// CONTROL. The closed grammar IS the safety property -- it is what lets a
// model's proposal be checked rather than trusted -- so the case that would
// falsify it is a function name nobody allowed.
func TestParseDerivation_RefusesAnythingOutsideTheClosedGrammar(t *testing.T) {
	for _, expr := range []string{
		`exec("rm -rf /")`,
		`eval(ref(0, "path"))`,
		`readFile("/etc/passwd")`,
		`ref(0, "path") + "x"`,
		`basename(ref(0), ref(1))`,
		`ref("zero", "path")`,
		`concat()`,
		`basename(`,
		``,
	} {
		if _, err := ParseDerivation(expr); err == nil {
			t.Errorf("ParseDerivation(%q) must refuse: an open grammar is code execution by another name", expr)
		}
	}
}

func TestParseDerivation_AcceptsTheWholeClosedGrammar(t *testing.T) {
	for _, expr := range []string{
		`ref(0, "path")`,
		`ref(0)`,
		`basename(ref(0, "path"))`,
		`dirname(ref(1, "a", "b"))`,
		`lower(ref(0))`,
		`upper(ref(0))`,
		`trim(ref(0))`,
		`concat("v", ref(0, "id"))`,
		`concat(lower(ref(0, "a")), "-", upper(ref(1, "b")))`,
		`"a literal"`,
	} {
		if _, err := ParseDerivation(expr); err != nil {
			t.Errorf("ParseDerivation(%q) = %v; want accepted", expr, err)
		}
	}
}

func TestCheckDerivation_ConcatAndCaseFunctionsEvaluate(t *testing.T) {
	instances := [][]Action{
		{
			{Tool: "exec", Args: Obj(map[string]*Node{"c": Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"id": "AB"}},
			{Tool: "fs_write", Args: Obj(map[string]*Node{"name": Lit("v-ab")}), Seq: 1},
		},
		{
			{Tool: "exec", Args: Obj(map[string]*Node{"c": Lit("x")}), Seq: 0,
				ResultValue: map[string]any{"id": "CD"}},
			{Tool: "fs_write", Args: Obj(map[string]*Node{"name": Lit("v-cd")}), Seq: 1},
		},
	}
	d, err := ParseDerivation(`concat("v-", lower(ref(0, "id")))`)
	if err != nil {
		t.Fatalf("ParseDerivation: %v", err)
	}
	if ok, held := CheckDerivation(d, holeAt(1, "name"), instances); !ok || held != 2 {
		t.Fatalf("ok=%v held=%d; want true, 2", ok, held)
	}
}

func TestCheckDerivation_ARefToAStepAtOrAfterTheHoleIsRejected(t *testing.T) {
	// A procedure cannot depend on a step that has not run. Accepting it
	// would lift an automation whose first statement reads its own later
	// output.
	d, err := ParseDerivation(`ref(1, "path")`)
	if err != nil {
		t.Fatalf("ParseDerivation: %v", err)
	}
	if ok, _ := CheckDerivation(d, holeAt(1, "name"), derivationInstances("a", "b")); ok {
		t.Fatal("a reference to the hole's own step must be rejected")
	}
}
