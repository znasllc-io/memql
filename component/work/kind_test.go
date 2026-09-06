package work

import (
	"errors"
	"testing"
)

// reg builds a registry from {name: (constructKind, calls...)} triples.
func reg() Registry {
	return Registry{
		"listRows":      {ConstructKind: ConstructQuery},
		"writeRow":      {ConstructKind: ConstructMutation},
		"isActive":      {ConstructKind: ConstructSpec},
		"summarize":     {ConstructKind: ConstructPrompt},
		"plainLogic":    {ConstructKind: ConstructLogic, Calls: []string{"listRows", "writeRow"}},
		"thinkingLogic": {ConstructKind: ConstructLogic, Calls: []string{"listRows", "summarize"}},
		"deepLogic":     {ConstructKind: ConstructLogic, Calls: []string{"plainLogic", "thinkingLogic"}},
		"loopyA":        {ConstructKind: ConstructLogic, Calls: []string{"loopyB"}},
		"loopyB":        {ConstructKind: ConstructLogic, Calls: []string{"loopyA"}},
		"embedBuiltin":  {ConstructKind: ConstructBuiltin},
		"askBuiltin":    {ConstructKind: ConstructBuiltin, Calls: []string{"summarize"}},
	}
}

func TestDeriveKind_DeterministicUnlessItReachesAPrompt(t *testing.T) {
	r := reg()
	for _, tc := range []struct {
		stepType string
		target   string
		want     Kind
	}{
		{"query", "listRows", KindDeterministic},
		{"mutation", "writeRow", KindDeterministic},
		{"function", "plainLogic", KindDeterministic},
		{"function", "thinkingLogic", KindReasoning},
		{"function", "deepLogic", KindReasoning},
		{"function", "embedBuiltin", KindDeterministic},
		{"function", "askBuiltin", KindReasoning},
		{"parallel", "", KindDeterministic},
		{"forEach", "", KindDeterministic},
		{"switch", "", KindDeterministic},
	} {
		got, err := DeriveKind(tc.stepType, tc.target, r)
		if err != nil {
			t.Errorf("%s/%s: %v", tc.stepType, tc.target, err)
			continue
		}
		if got != tc.want {
			t.Errorf("DeriveKind(%s, %s) = %q, want %q", tc.stepType, tc.target, got, tc.want)
		}
	}
}

func TestDeriveKind_SpecIsADecisionAndHumanFormsPark(t *testing.T) {
	r := reg()
	if k, _ := DeriveKind("spec", "isActive", r); k != KindDecision {
		t.Errorf("a spec call is a decision, got %q", k)
	}
	for _, st := range []string{"approval", "feedback"} {
		if k, _ := DeriveKind(st, "", r); k != KindHuman {
			t.Errorf("%s step kind = %q, want human", st, k)
		}
	}
	if k, _ := DeriveKind("wait", "", r); k != KindHuman {
		t.Errorf("a wait step parks like a human step, got %q", k)
	}
	if k, _ := DeriveKind("automation", "", r); k != KindSubrun {
		t.Errorf("a sub-automation call opens a child run, got %q", k)
	}
}

// A cycle in the call graph must terminate rather than hang, and it must
// not be mistaken for "reaches a prompt".
func TestReachesPrompt_TerminatesOnACycle(t *testing.T) {
	r := reg()
	reached, path := ReachesPrompt("loopyA", r)
	if reached {
		t.Fatalf("a cycle with no prompt in it does not reach a prompt; path=%v", path)
	}
}

func TestReachesPrompt_NamesThePath(t *testing.T) {
	_, path := ReachesPrompt("deepLogic", reg())
	if len(path) == 0 || path[len(path)-1] != "summarize" {
		t.Fatalf("path = %v, want it to end at the prompt so the loader error can name it", path)
	}
}

// The loader rule (spec section B): a step ANNOTATED deterministic that
// reaches a prompt is refused. The refusal is what stops a template
// claiming a replayable step that will in fact call a model every run.
func TestValidateDeclaredKind_RefusesDeterministicReachingAPrompt(t *testing.T) {
	err := ValidateDeclaredKind(KindDeterministic, KindReasoning, "thinkingLogic", []string{"thinkingLogic", "summarize"})
	if !errors.Is(err, ErrDeterministicReachesPrompt) {
		t.Fatalf("err = %v, want ErrDeterministicReachesPrompt", err)
	}
	if got := err.Error(); got == "" || !contains(got, "summarize") {
		t.Errorf("the refusal must name the prompt it reaches; got %q", got)
	}
	if err := ValidateDeclaredKind(KindDeterministic, KindDeterministic, "plainLogic", nil); err != nil {
		t.Errorf("an honest deterministic step is fine: %v", err)
	}
	if err := ValidateDeclaredKind(KindUnset, KindReasoning, "thinkingLogic", nil); err != nil {
		t.Errorf("an UNdeclared kind is derived, never refused: %v", err)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
