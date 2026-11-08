package routingrules

import (
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

func validForm() Form {
	return Form{
		Name:          "myOperatorRule",
		Description:   "Route my own agent replies at the reasoning level.",
		When:          map[string]string{"prompt": "agentReply", "role": "operator"},
		Policy:        "localOnly",
		Level:         "reasoning",
		Precedence:    500,
		OnUnavailable: "park",
		Excludes:      []string{"fleet:qwen3.5:7b"},
	}
}

// TestGeneratedRuleParses is the whole contract in one assertion: whatever this
// generator emits, the real parser accepts. A renderer checked only against
// its own expected string is a renderer that agrees with itself.
func TestGeneratedRuleParses(t *testing.T) {
	src, err := GenerateRule(validForm())
	if err != nil {
		t.Fatalf("GenerateRule: %v", err)
	}
	decl, err := langparser.ParseRuleDecl(src)
	if err != nil {
		t.Fatalf("the generated construct does not parse: %v\n\n%s", err, src)
	}
	if decl.Name != "myOperatorRule" {
		t.Fatalf("name = %q", decl.Name)
	}
	if decl.Policy != "localOnly" || decl.Level != "reasoning" || decl.Precedence != 500 {
		t.Fatalf("policy=%q level=%q precedence=%d", decl.Policy, decl.Level, decl.Precedence)
	}
	if decl.OnUnavailable != "park" {
		t.Fatalf("onUnavailable = %q", decl.OnUnavailable)
	}
	if !decl.When.Present["prompt"] || !decl.When.Present["role"] {
		t.Fatalf("the rendered conditions did not survive the round trip: %+v", decl.When)
	}
	if decl.When.Present["level"] {
		t.Fatal("a condition nobody wrote came back present; an absent key must stay absent")
	}
	if len(decl.Excludes) != 1 || decl.Excludes[0] != "fleet:qwen3.5:7b" {
		t.Fatalf("excludes = %v", decl.Excludes)
	}
	if decl.Locked {
		t.Fatal("the generated rule is @locked; runtime authoring never grants that")
	}
}

// TestGeneratedRuleNeverCarriesLocked. @locked places a rule ahead of every
// shipped one, an authority that belongs to the embedded tree alone. It is
// checked on the OUTPUT rather than trusted from the absence of a field,
// because a later Form gaining a Locked bool is exactly how it would arrive.
func TestGeneratedRuleNeverCarriesLocked(t *testing.T) {
	src, err := GenerateRule(validForm())
	if err != nil {
		t.Fatalf("GenerateRule: %v", err)
	}
	if strings.Contains(src, "@locked") {
		t.Fatalf("the generated construct carries @locked:\n%s", src)
	}
}

// TestRenderingIsStable. Regenerating an unchanged form must produce
// byte-identical source, or every regeneration looks like an edit to whatever
// hashes it -- which in this tree is the evidence review's approval hash: a
// re-render that moved one byte would orphan a decision somebody already made.
func TestRenderingIsStable(t *testing.T) {
	f := validForm()
	f.When = map[string]string{"tag": "background", "role": "operator", "prompt": "agentReply"}
	first, err := GenerateRule(f)
	if err != nil {
		t.Fatalf("GenerateRule: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := GenerateRule(f)
		if err != nil {
			t.Fatalf("GenerateRule: %v", err)
		}
		if again != first {
			t.Fatalf("rendering is not stable across map iteration:\n%s\n---\n%s", first, again)
		}
	}
}

// TestAnEmptyConditionSetRenders. A rule with no conditions matches every call,
// which is legal and is what an owner writes to put a policy in front of the
// shipped default. It must render as `@when()` rather than as nothing.
func TestAnEmptyConditionSetRenders(t *testing.T) {
	f := validForm()
	f.When = nil
	src, err := GenerateRule(f)
	if err != nil {
		t.Fatalf("GenerateRule: %v", err)
	}
	if !strings.Contains(src, "@when()") {
		t.Fatalf("an empty condition set did not render as @when():\n%s", src)
	}
	if _, err := langparser.ParseRuleDecl(src); err != nil {
		t.Fatalf("the conditionless rule does not parse: %v\n%s", err, src)
	}
}

// TestADescriptionBecomesADocComment. The description is what the rules list
// shows, and a rule whose reason lives only in a form row is one nobody reading
// the corpus can understand.
func TestADescriptionBecomesADocComment(t *testing.T) {
	f := validForm()
	f.Description = strings.Repeat("a reason a person wrote ", 8)
	src, err := GenerateRule(f)
	if err != nil {
		t.Fatalf("GenerateRule: %v", err)
	}
	if !strings.HasPrefix(src, "/// ") {
		t.Fatalf("the description did not become a doc comment:\n%s", src)
	}
	for _, line := range strings.Split(src, "\n") {
		if len(line) > 100 {
			t.Fatalf("a rendered line is %d characters; the doc wrap did not run:\n%s", len(line), line)
		}
	}
	if _, err := langparser.ParseRuleDecl(src); err != nil {
		t.Fatalf("the described rule does not parse: %v\n%s", err, src)
	}
}
