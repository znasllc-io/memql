package memql

import (
	"strings"
	"testing"
)

// TestSandboxCompilesAPolicy. A policy used to fall through the sandbox's
// default arm and be reported "not compiled by the sandbox" -- a graceful skip,
// which meant an authored construct reached activation without its grammar ever
// having been checked.
func TestSandboxCompilesAPolicy(t *testing.T) {
	report := SandboxCompileBundle([]SandboxConstruct{{
		Name: "myPolicy",
		Kind: "policy",
		Source: `@primary("fleet:strongest")
@fallback("federation:cheapest")
policy myPolicy { }`,
	}})
	if !report.OK {
		t.Fatalf("a valid policy did not compile: %+v", report.Diagnostics)
	}
	for _, d := range report.Diagnostics {
		if d.Skipped {
			t.Fatalf("kind %q was SKIPPED rather than compiled: %s.\n"+
				"A skip here is what let an authored construct reach activation with its grammar "+
				"unchecked.", d.Kind, d.Error)
		}
	}
}

// TestSandboxRefusesEveryRule. A routing rule is cluster routing configuration
// (routingRuleSave), and the router never reads one armed from a bundle, so
// Gate 1 refuses a rule whatever it says -- valid, locked or malformed --
// rather than compiling it and letting it arm as a construct that decides no
// call (2026-11-08). It is REFUSED, never skipped: a skipped construct does not
// fail the bundle.
func TestSandboxRefusesEveryRule(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"a valid rule", "@when(prompt=\"agentReply\")\n@policy(\"localFirst\")\n@precedence(500)\nrule myRule { }"},
		{"a locked rule", "@when()\n@policy(\"localFirst\")\n@precedence(9999)\n@locked\nrule myRule { }"},
		{"a malformed rule", "@when(model=\"gpt-5\")\n@precedence(5)\nrule myRule { }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := SandboxCompileBundle([]SandboxConstruct{{Name: "myRule", Kind: "rule", Source: tc.source}})
			if report.OK {
				t.Fatalf("the sandbox accepted %s; armed, it would decide no call", tc.name)
			}
			if len(report.Diagnostics) != 1 {
				t.Fatalf("want one diagnostic for the one rule, got %+v", report.Diagnostics)
			}
			d := report.Diagnostics[0]
			if d.Skipped {
				t.Fatalf("%s was skipped rather than refused, so it would not fail its bundle: %+v", tc.name, d)
			}
			for _, want := range []string{`rule "myRule"`, "authoring is not supported in a bundle", "routingRuleSave"} {
				if !strings.Contains(d.Error, want) {
					t.Fatalf("the refusal of %s does not say %q, so it does not tell the author where a rule goes:\n%s",
						tc.name, want, d.Error)
				}
			}
		})
	}
}

// TestEngineCoreHasCoversRuleAndPolicy: the authoring pipeline's core-first
// resolver sees the loaded rules and policies. A kind absent from its switch
// reports false, which reads as "core does not own this name" and would let an
// authored construct take it. Gate 1 refuses an authored rule outright now;
// the resolver still answers truthfully for the kind, so an active bundle
// armed before that refusal cannot claim a shipped name either.
func TestEngineCoreHasCoversRuleAndPolicy(t *testing.T) {
	e := &MemQLEngine{}
	e.rules = NewRuleRegistry()
	if err := e.rules.Register(&RuleConfig{
		Name: "shippedThing", Policy: "localFirst", SourceFile: "rules/rules.memql",
		When: RuleWhen{Present: map[string]bool{}},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	e.policies = &PolicyRegistry{byName: map[string]*PolicyConfig{
		"shippedPolicy": {Name: "shippedPolicy", Primary: "fleet:strongest"},
	}}

	has := EngineCoreHas(e)
	if !has("rule", "shippedThing") {
		t.Error("core-first does not see a loaded rule, so an authored one could take its name")
	}
	if !has("policy", "shippedPolicy") {
		t.Error("core-first does not see a loaded policy, so an authored one could take its name")
	}
	if has("rule", "somethingElse") || has("policy", "somethingElse") {
		t.Error("core-first claims a name nothing loaded, which would refuse a legitimate authored construct")
	}
}
