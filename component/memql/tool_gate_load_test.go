package memql

// tool_gate_load_test.go -- the load-time half of the tool gates
// (memql#5438): @requiresAgentRole's values against the agent concept's own
// role enum, @requiresRank's slug against the ladder.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// TestAgentRoleVocabularyIsTheAgentConceptsRoleEnum: the vocabulary is READ
// from the v1:agents:agent declaration, not kept beside it.
func TestAgentRoleVocabularyIsTheAgentConceptsRoleEnum(t *testing.T) {
	roles, ok := agentRoleVocabulary(loadedConceptRegistry(t))
	if !ok {
		t.Fatalf("no agent-role vocabulary read from the embedded %s declaration", conceptAgentsAgent)
	}
	if want := []string{"assistant", "specialist"}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("agent-role vocabulary = %v, want %v -- the %s.%s enum in dsl/agents/concepts.memql",
			roles, want, conceptAgentsAgent, agentRoleField)
	}
}

// TestAgentRoleVocabularyFollowsTheDeclaration proves the derivation rather
// than the values: an agent concept declaring a third role makes it legal, and
// a registry with no agent concept has no vocabulary to admit anything with.
func TestAgentRoleVocabularyFollowsTheDeclaration(t *testing.T) {
	agent := parseFixtureConcept(t, "v1/agents/agent", `
/// An agent with one more role than the shipped tree declares.
concept agent {
  name  string
  role  enum("specialist", "assistant", "reviewer")
}
`)
	roles, ok := agentRoleVocabulary(concept.NewRegistry(map[string]*concept.Concept{agent.Name: agent}))
	if !ok || !containsExact(roles, "reviewer") {
		t.Fatalf("a role the declaration adds is not in the vocabulary: %v (ok=%v)", roles, ok)
	}
	if _, ok := agentRoleVocabulary(concept.NewRegistry(map[string]*concept.Concept{})); ok {
		t.Fatal("a registry with no agent concept produced a vocabulary")
	}
}

func TestToolGateDeclarationsAreCheckedAtLoad(t *testing.T) {
	eng := newQuietEngine(t)
	registry := loadedConceptRegistry(t)
	tools := newToolRegistry()
	for _, tool := range []*Tool{
		{Name: "goodAgentGate", RequiresAgentRole: []string{"assistant", "specialist"}, Origin: "unified:acme/tools.memql:goodAgentGate"},
		{Name: "goodRankGate", RequiresRank: "writer", Origin: "unified:acme/tools.memql:goodRankGate"},
		{Name: "plannerGate", RequiresAgentRole: []string{"assistant", "system-planner"}, Origin: "unified:acme/tools.memql:plannerGate"},
		{Name: "typoRankGate", RequiresRank: "superuser", Origin: "unified:acme/tools.memql:typoRankGate"},
		{Name: "ungated", Origin: "unified:acme/tools.memql:ungated"},
	} {
		if err := tools.Upsert(tool); err != nil {
			t.Fatalf("Upsert %s: %v", tool.Name, err)
		}
	}
	raw := []baseloader.RawFile{{Path: "acme/tools.memql", Content: `/// Plan a goal.
@handler(type="function", name="x")
@requiresAgentRole("assistant", "system-planner")
tool plannerGate {
}

/// A floor with a typo.
@requiresRank("superuser")
@handler(type="function", name="y")
tool typoRankGate {
}
`}}

	problems := eng.validateToolGateDeclarations(context.Background(), tools, registry, raw)
	if len(problems) != 2 {
		t.Fatalf("want exactly the two bad gates refused, got %d: %+v", len(problems), problems)
	}

	agent, rank := problems[0], problems[1]
	if agent.tool != "plannerGate" || baseloader.RuleCode(agent.err) != RuleRequiresAgentRoleUnknown {
		t.Fatalf("first problem = %s %v, want plannerGate [%s]", agent.tool, agent.err, RuleRequiresAgentRoleUnknown)
	}
	// Names the construct, the position, the value, what is legal, and the
	// way out when the gate was meant for a person.
	for _, want := range []string{`tool "plannerGate"`, "acme/tools.memql:3", `"system-planner" is not an agent role`,
		"assistant, specialist", `@requiresRank("<role>")`, "[" + RuleRequiresAgentRoleUnknown + "]"} {
		if !strings.Contains(agent.err.Error(), want) {
			t.Errorf("the agent-role refusal does not carry %q:\n%s", want, agent.err)
		}
	}
	if rank.tool != "typoRankGate" || baseloader.RuleCode(rank.err) != RuleRequiresRankUnknown {
		t.Fatalf("second problem = %s %v, want typoRankGate [%s]", rank.tool, rank.err, RuleRequiresRankUnknown)
	}
	for _, want := range []string{`tool "typoRankGate"`, "acme/tools.memql:8", `@requiresRank("superuser")`, "Known roles:", "[" + RuleRequiresRankUnknown + "]"} {
		if !strings.Contains(rank.err.Error(), want) {
			t.Errorf("the rank refusal does not carry %q:\n%s", want, rank.err)
		}
	}
	if agent.file != "acme/tools.memql" {
		t.Errorf("problem filed under %q, want the tool's file", agent.file)
	}

	// Recorded, the problems are coded strict-boot skips.
	report := newLoadReport()
	eng.recordToolGateProblems(context.Background(), report, tools, registry, raw)
	if !report.HasProblems() {
		t.Fatal("a refused tool gate left the load report clean, so strict boot would not refuse it")
	}
	codes := map[string]bool{}
	for _, s := range report.Skipped {
		codes[s.Code] = true
	}
	if !codes[RuleRequiresAgentRoleUnknown] || !codes[RuleRequiresRankUnknown] {
		t.Fatalf("the skips do not carry the rule codes: %+v", report.Skipped)
	}
}

// TestAnUncheckableAgentGateIsRefusedNotTrusted: with no agent concept loaded
// there is no vocabulary, and the gate is refused rather than admitted
// unchecked.
func TestAnUncheckableAgentGateIsRefusedNotTrusted(t *testing.T) {
	eng := newQuietEngine(t)
	tools := newToolRegistry()
	if err := tools.Upsert(&Tool{Name: "gated", RequiresAgentRole: []string{"assistant"}, Origin: "unified:acme/tools.memql:gated"}); err != nil {
		t.Fatal(err)
	}
	problems := eng.validateToolGateDeclarations(context.Background(), tools, concept.NewRegistry(map[string]*concept.Concept{}), nil)
	if len(problems) != 1 || !strings.Contains(problems[0].err.Error(), "is not loaded") {
		t.Fatalf("want the gate refused for want of a vocabulary, got %+v", problems)
	}
}

// TestAFunctionFloorRefusalCarriesTheSameCode: @requiresRank is one check on
// every construct that takes it, so a query's unknown floor is refused with
// the code a tool's is.
func TestAFunctionFloorRefusalCarriesTheSameCode(t *testing.T) {
	eng := newQuietEngine(t)
	fns := newFunctionRegistry()
	if err := fns.Upsert(&Function{Name: "typoFloorQuery", FunctionKind: "query", RequiresRank: "superuser"}); err != nil {
		t.Fatal(err)
	}
	problems := eng.validateRequiresRankSlugs(context.Background(), fns)
	if len(problems) != 1 || baseloader.RuleCode(problems[0]) != RuleRequiresRankUnknown ||
		!strings.HasSuffix(problems[0].Error(), "["+RuleRequiresRankUnknown+"]") {
		t.Fatalf("want one %s refusal, got %v", RuleRequiresRankUnknown, problems)
	}
}
