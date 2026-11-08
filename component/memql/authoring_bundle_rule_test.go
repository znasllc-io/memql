package memql_test

// authoring_bundle_rule_test.go -- a `rule` is an authorable construct kind:
// the bundle splitter slices it, and Gate 1 compiles it the way the embedded
// tree's rule loader does (memql#6022 follow-up).
//
// The sandbox, the core-first resolver and the activation have all known the
// kind since epic memql#5127, but the splitter in front of them classified no
// `rule` region: every generated routing rule reached the sandbox as an
// "unrecognized construct" and was refused, so none was ever armed. The
// DB-backed path through the builtin is app/routing_rule_arming_db_test.go.

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// generatedRuleSource is a rule as component/routingrules renders one: a doc
// comment, one annotation per form field, and an empty body.
const generatedRuleSource = `/// Route my own agent replies.
@when(prompt="agentReply", role="operator")
@level("reasoning")
@policy("localOnly")
@precedence(500)
@onUnavailable("park")
rule myAgentReplies { }
`

// TestTheBundleSplitterSlicesARule: a rule region is one construct of kind
// rule, carrying the annotations that make it one -- a slice that dropped them
// would hand Gate 1 a rule with no policy.
func TestTheBundleSplitterSlicesARule(t *testing.T) {
	got := memql.SplitBundleSource(generatedRuleSource)
	if len(got) != 1 || got[0].Kind != "rule" || got[0].Name != "myAgentReplies" {
		t.Fatalf("SplitBundleSource = %+v, want exactly one rule named myAgentReplies", got)
	}
	if !strings.Contains(got[0].Source, `@policy("localOnly")`) {
		t.Fatalf("the rule's slice lost its annotations:\n%s", got[0].Source)
	}
}

// TestGateOneCompilesARule: the generated rule passes Gate 1.
func TestGateOneCompilesARule(t *testing.T) {
	if rep := memql.ValidateBundle(generatedRuleSource, "rules/rules.memql"); !rep.OK {
		t.Fatalf("Gate 1 refused a generated rule: %+v", rep.Diagnostics)
	}
}

// TestGateOneRefusesALockedRuleForBeingLocked: @locked is the embedded tree's
// alone (it evaluates a rule before every shipped one), and Gate 1 refuses it
// in an authored bundle. That refusal was unreachable while the splitter
// classified no rule -- a locked rule was refused as unrecognized, for the
// wrong reason -- so this holds the refusal to its own reason now that a rule
// reaches it.
func TestGateOneRefusesALockedRuleForBeingLocked(t *testing.T) {
	locked := strings.Replace(generatedRuleSource, "rule myAgentReplies", "@locked\nrule myAgentReplies", 1)
	rep := memql.ValidateBundle(locked, "rules/rules.memql")
	if rep.OK {
		t.Fatal("Gate 1 accepted a rule carrying @locked")
	}
	for _, d := range rep.Diagnostics {
		if !d.OK && strings.Contains(d.Error, "carries @locked") {
			return
		}
	}
	t.Fatalf("Gate 1 refused the locked rule, but not for being locked: %+v", rep.Diagnostics)
}
