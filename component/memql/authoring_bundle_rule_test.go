package memql_test

// authoring_bundle_rule_test.go -- a `rule` is NOT an authorable construct
// kind: the bundle splitter classifies it so Gate 1 can refuse it by name and
// say where a routing rule goes (2026-11-08).
//
// The router reads the shipped rules plus the routing configuration document
// and never the authored runtime, so a rule armed from a bundle read active
// everywhere activation writes and decided no call. routingRuleActivate, the
// one door that armed them, was retired for that reason; a custom rule is
// saved with routingRuleSave (routing_configuration_rules.go).

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// ruleSource is a rule as it would arrive in a bundle: a doc comment, one
// annotation per field, and an empty body.
const ruleSource = `/// Route my own agent replies.
@when(prompt="agentReply", role="operator")
@level("reasoning")
@policy("localOnly")
@precedence(500)
@onUnavailable("park")
rule myAgentReplies { }
`

// TestTheBundleSplitterSlicesARule: a rule region is one construct of kind
// rule, named for the rule. Without the slice, Gate 1 could only call it an
// unrecognized region, which names neither the rule nor where it belongs.
func TestTheBundleSplitterSlicesARule(t *testing.T) {
	got := memql.SplitBundleSource(ruleSource)
	if len(got) != 1 || got[0].Kind != "rule" || got[0].Name != "myAgentReplies" {
		t.Fatalf("SplitBundleSource = %+v, want exactly one rule named myAgentReplies", got)
	}
}

// TestGateOneRefusesARuleAndNamesRoutingRuleSave: Gate 1 refuses a rule in a
// bundle, locked or not, for being a rule, and the refusal names the path that
// routes. Accepting it would arm a construct the router never consults.
func TestGateOneRefusesARuleAndNamesRoutingRuleSave(t *testing.T) {
	for name, src := range map[string]string{
		"an ordinary rule": ruleSource,
		"a locked rule":    strings.Replace(ruleSource, "rule myAgentReplies", "@locked\nrule myAgentReplies", 1),
	} {
		t.Run(name, func(t *testing.T) {
			rep := memql.ValidateBundle(src, "rules/rules.memql")
			if rep.OK {
				t.Fatalf("Gate 1 accepted %s; the router would never read it", name)
			}
			for _, d := range rep.Diagnostics {
				if d.OK || d.Kind != "rule" {
					continue
				}
				if d.Skipped {
					t.Fatalf("Gate 1 skipped %s rather than refusing it; a skipped construct does not fail the bundle: %+v", name, d)
				}
				if !strings.Contains(d.Error, `rule "myAgentReplies"`) ||
					!strings.Contains(d.Error, "authoring is not supported in a bundle") ||
					!strings.Contains(d.Error, "routingRuleSave") {
					t.Fatalf("Gate 1 refused %s without naming the rule and routingRuleSave: %q", name, d.Error)
				}
				return
			}
			t.Fatalf("Gate 1 refused %s, but no diagnostic is the rule's: %+v", name, rep.Diagnostics)
		})
	}
}

// TestARuleBesideAValidConstructFailsTheWholeBundle: a rule cannot ride along
// in an otherwise valid bundle. Gate 1's verdict is per bundle, so one refused
// construct keeps every construct in it from being armed.
func TestARuleBesideAValidConstructFailsTheWholeBundle(t *testing.T) {
	bundle := ruleSource + `
/// A concept that compiles on its own.
concept ruleNeighbour {
  label  string
}
`
	rep := memql.ValidateBundle(bundle, "rules/rules.memql")
	if rep.OK {
		t.Fatalf("a bundle carrying a rule validated: %+v", rep.Diagnostics)
	}
	sawNeighbour := false
	for _, d := range rep.Diagnostics {
		if d.Kind == "concept" && d.Name == "ruleNeighbour" {
			sawNeighbour = true
			if !d.OK {
				t.Fatalf("the concept beside the rule was refused too, so this test proves nothing about the rule: %+v", d)
			}
		}
	}
	if !sawNeighbour {
		t.Fatalf("the concept beside the rule was not compiled at all: %+v", rep.Diagnostics)
	}
}
