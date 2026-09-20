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
		{"function", "listRows", KindDeterministic},
		{"function", "writeRow", KindDeterministic},
		{"function", "plainLogic", KindDeterministic},
		{"function", "thinkingLogic", KindReasoning},
		{"function", "deepLogic", KindReasoning},
		{"function", "embedBuiltin", KindDeterministic},
		{"function", "askBuiltin", KindReasoning},
		{"parallel", "", KindDeterministic},
		{"forEach", "", KindDeterministic},
		{"block", "", KindDeterministic},
		{"expression", "", KindDeterministic},
		{"return", "", KindDeterministic},
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

// A step type no statement compiles to is refused rather than guessed at:
// the step blocks' types went with the step blocks (epic memql#5370).
func TestDeriveKind_RetiredStepTypesAreUnknown(t *testing.T) {
	for _, st := range []string{"query", "mutation", "shape", "webhook", "switch", "emitConceptCard"} {
		if _, err := DeriveKind(st, "", reg()); err == nil {
			t.Errorf("step type %q is retired; DeriveKind must refuse it", st)
		}
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

// TestDeriveKind_AppSessionActions: the six step types an app session's
// recording writes (epic memql#5396, design D2).
//
// THE SPLIT IS THE POINT, and it is what epic C rests on. A shell command, a
// file read or write, a fetch and an MCP call back into MemQL all reach NO
// prompt -- they are deterministic, which is exactly why a recurring sequence
// of them can later replay without a model. `app_answer` is the one that
// cannot: it is the app's own structured answer, so the intelligence IS the
// step, and recording it as deterministic would tell a later lift that the
// cheapest part of the session was the expensive one.
//
// It also catches the near-miss that would otherwise be silent. DeriveKind
// errors on a type it does not know, and it is the ONLY thing in the tree that
// validates a stepType at all -- nothing on the write path does -- so a
// recorder spelling `fsWrite` or `app-answer` would write rows nobody rejects
// and this function would start failing on them later, in a caller that has
// not been wired yet.
func TestDeriveKind_AppSessionActions(t *testing.T) {
	r := reg()
	for _, tc := range []struct {
		stepType string
		want     Kind
	}{
		{"exec", KindDeterministic},
		{"fs_write", KindDeterministic},
		{"fs_read", KindDeterministic},
		{"fetch", KindDeterministic},
		{"mcp", KindDeterministic},
		{"app_answer", KindReasoning},
	} {
		got, err := DeriveKind(tc.stepType, "", r)
		if err != nil {
			t.Errorf("%s: %v -- an app session records this type, and DeriveKind is the one "+
				"place a stepType is checked at all", tc.stepType, err)
			continue
		}
		if got != tc.want {
			t.Errorf("DeriveKind(%q) = %q, want %q", tc.stepType, got, tc.want)
		}
	}
}
