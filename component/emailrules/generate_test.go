package emailrules

import (
	"strings"
	"testing"

	memqlengine "github.com/znasllc-io/memql/component/memql"
)

func baseRule() Rule {
	return Rule{
		ID:             "v1:campaigns:emailRule:ab12cd34",
		OwnerUserID:    "v1:identity:user:owner1",
		Name:           "Tell the owner about new admins",
		Description:    "A new admin is a security-relevant change somebody should know about.",
		TriggerConcept: "v1:identity:user",
		EventKind:      "created",
		TemplateID:     "v1:campaigns:template:t1",
		RecipientMode:  ModeClusterRoles,
		RecipientRoles: []string{"owner"},
	}
}

// The generated construct has to COMPILE, and the only honest way to know that
// is to hand it to the real parser -- the same Gate-1 entry point the
// activation path uses. A test that asserted on the generated string would
// prove the generator agrees with itself and nothing else, which is the
// fake-engine failure this tree has hit repeatedly.
func TestGeneratedAutomationCompiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule func() Rule
	}{
		{"operational lane, no condition", baseRule},
		{"operational lane with a condition", func() Rule {
			r := baseRule()
			r.Condition = `payload.role == "admin"`
			return r
		}},
		{"marketing lane over an audience", func() Rule {
			r := baseRule()
			r.RecipientMode = ModeAudience
			r.AudienceID = "v1:campaigns:audience:a1"
			r.EventKind = "updated"
			return r
		}},
		{"marketing lane off the triggering row", func() Rule {
			r := baseRule()
			r.RecipientMode = ModeRowAddress
			r.RecipientField = "primaryContactEmail"
			r.AudienceID = "v1:campaigns:audience:a1"
			r.TriggerConcept = "v1:accounts:account"
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, err := GenerateAutomation(tc.rule())
			if err != nil {
				t.Fatalf("GenerateAutomation: %v", err)
			}
			report := memqlengine.ValidateBundle(src, "campaigns/automations.memql")
			if !report.OK {
				t.Fatalf("the generated construct does not compile:\n%s\ndiagnostics: %+v", src, report.Diagnostics)
			}
		})
	}
}

// A regeneration must REPLACE, not accumulate. Two names for one rule would
// leave the old automation armed beside the new one, and an edited rule would
// send twice -- once with each version.
func TestConstructNameIsDeterministicAndScoped(t *testing.T) {
	a := ConstructNameFor("v1:campaigns:emailRule:ab12cd34")
	b := ConstructNameFor("v1:campaigns:emailRule:ab12cd34")
	if a != b {
		t.Fatalf("two names for one rule: %q and %q", a, b)
	}
	if !strings.HasPrefix(a, "emailRule") {
		t.Errorf("construct names are unique tree-wide; %q carries no scoping prefix", a)
	}
	if ConstructNameFor("v1:campaigns:emailRule:zz99") == a {
		t.Error("two rules share one construct name; the second would supersede the first")
	}
}

// The condition is emitted UNQUOTED inside @filter(...), so a character that
// closes the annotation does not produce a bad filter -- it produces a
// different construct. That has to be refused before the text is assembled.
func TestConditionCannotEscapeTheAnnotation(t *testing.T) {
	for _, bad := range []string{
		`payload.role == "admin") @trigger(event="node.created", concept="v1:identity:user"`,
		"payload.role == \"admin\"\n@disabled",
		`payload.role == "admin"; drop`,
		`payload.role == "admin" } automation evil {`,
		`payload.role == "admin"@x`,
	} {
		r := baseRule()
		r.Condition = bad
		if err := r.Validate(); err == nil {
			t.Errorf("condition %q was accepted; it can close the annotation and continue as source", bad)
		}
	}
	// A condition that does not test the triggering row is a rule that fires
	// on everything while looking selective.
	r := baseRule()
	r.Condition = `1 == 1`
	if err := r.Validate(); err == nil {
		t.Error("a condition naming no payload field was accepted")
	}
	// The ordinary case still passes.
	r.Condition = `payload.role == "admin" && payload.active == true`
	if err := r.Validate(); err != nil {
		t.Errorf("an ordinary condition was refused: %v", err)
	}
}

func TestValidateRefusesAnIncompleteForm(t *testing.T) {
	cases := map[string]func(Rule) Rule{
		"no owner":              func(r Rule) Rule { r.OwnerUserID = ""; return r },
		"no template":           func(r Rule) Rule { r.TemplateID = ""; return r },
		"a bare concept name":   func(r Rule) Rule { r.TriggerConcept = "user"; return r },
		"a deleted event":       func(r Rule) Rule { r.EventKind = "deleted"; return r },
		"an unknown mode":       func(r Rule) Rule { r.RecipientMode = "everyone"; return r },
		"an audience with none": func(r Rule) Rule { r.RecipientMode = ModeAudience; return r },
		"a row field with none": func(r Rule) Rule { r.RecipientMode = ModeRowAddress; return r },
		"a row address with no audience": func(r Rule) Rule {
			r.RecipientMode = ModeRowAddress
			r.RecipientField = "primaryContactEmail"
			return r
		},
	}
	for name, mutate := range cases {
		if err := mutate(baseRule()).Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The lane is a consequence of who receives, never a separate setting.
func TestLaneFollowsTheRecipient(t *testing.T) {
	if LaneFor(ModeClusterRoles) != "operational" {
		t.Error("cluster recipients must ride the operational lane; the marketing suppression list has no business deciding whether an operator hears about a security change")
	}
	for _, m := range []string{ModeAudience, ModeRowAddress} {
		if LaneFor(m) != "marketing" {
			t.Errorf("%s must ride the marketing lane: suppression checked, unsubscribe attached", m)
		}
	}
}

func TestGeneratedSourceNamesTheRuleAndTheLane(t *testing.T) {
	r := baseRule()
	src, err := GenerateAutomation(r)
	if err != nil {
		t.Fatalf("GenerateAutomation: %v", err)
	}
	for _, want := range []string{r.ID, "operational", "emailRuleFire", "event: event", "nodeId: id"} {
		if !strings.Contains(src, want) {
			t.Errorf("generated source does not mention %q:\n%s", want, src)
		}
	}
	// A name carrying a newline must not end the comment and leave the rest
	// as source.
	r.Name = "line one\nautomation evil {"
	src, err = GenerateAutomation(r)
	if err != nil {
		t.Fatalf("GenerateAutomation: %v", err)
	}
	if report := memqlengine.ValidateBundle(src, "campaigns/automations.memql"); !report.OK {
		t.Fatalf("a rule name containing a newline broke the construct:\n%s", src)
	}
}
