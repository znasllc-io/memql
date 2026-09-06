package work

import "testing"

func TestDerivePostcondition_MutationAndQueryGetOneFree(t *testing.T) {
	r := fpReg()
	pc, ok := DerivePostcondition("mutation", "writeThing", r)
	if !ok {
		t.Fatal("a mutation's postcondition is derivable: the row exists with the fields it wrote")
	}
	if pc.Kind != PostconditionCheck || pc.Ref != "rowWritten:v1:x:thing" {
		t.Errorf("mutation postcondition = %+v", pc)
	}
	pc, ok = DerivePostcondition("query", "readRows", r)
	if !ok {
		t.Fatal("a query's postcondition is its shape")
	}
	if pc.Kind != PostconditionSchema {
		t.Errorf("query postcondition kind = %q, want schema", pc.Kind)
	}
}

func TestDerivePostcondition_ReasoningStepsGetNoneForFree(t *testing.T) {
	if _, ok := DerivePostcondition("function", "sendMail", fpReg()); ok {
		// sendMail is a builtin with external effects: nothing about it
		// says what its result should look like.
		t.Fatal("a builtin's postcondition cannot be derived; the template must declare one")
	}
}

// The spec's rule, stated as a gate: a step with no postcondition cannot be
// CALLED deterministic. This is what stops "deterministic" meaning "we did
// not check".
func TestRequirePostcondition_DeterministicWithoutOneIsRefused(t *testing.T) {
	err := RequirePostcondition(KindDeterministic, Postcondition{}, "someStep")
	if err == nil {
		t.Fatal("a deterministic step with no postcondition must be refused")
	}
	if err := RequirePostcondition(KindDeterministic, Postcondition{Kind: PostconditionSpec, Ref: "isActive"}, "someStep"); err != nil {
		t.Errorf("a declared postcondition satisfies it: %v", err)
	}
	if err := RequirePostcondition(KindReasoning, Postcondition{}, "someStep"); err != nil {
		t.Errorf("only DETERMINISTIC steps owe a postcondition: %v", err)
	}
}

func TestPostconditionFailure_IsAContractSymptom(t *testing.T) {
	sym, ev, ok := ClassifyByRules(Signal{PostconditionFailed: true})
	if !ok || sym != SymptomContract {
		t.Fatalf("a postcondition failure is a contract symptom, got %q ok=%v", sym, ok)
	}
	if ActFor(sym, 1, 3) != ActRepair {
		t.Fatal("a contract symptom repairs from the failed step, keeping the prefix")
	}
	_ = ev
}
