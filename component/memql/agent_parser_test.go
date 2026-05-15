package memql

import (
	"strings"
	"testing"
)

// canonicalAgentSource is the strawman the brainstorm landed on, ported
// verbatim minus comments. Every parser test that round-trips a "full"
// declaration uses this so any drift between the docs/_reference and
// what the parser accepts surfaces here.
const canonicalAgentSource = `
@version("1.0.0")
@namespace("agents")
@scope("perUser")
@visibility("bff", "cognition", "agent")
@templateFile("templates/generalAssistant.tmpl")
@description("Per-user General Assistant.")
agent generalAssistant {
  role:        "general_assistant"
  roleSlug:    "general_assistant"
  name:        "General Assistant"
  description: "Designated fallback when no specialist fits."
  personality: "Friendly, capable, proactive."
  gender:      "female"

  providerConfig {
    llm {
      policyName:  "balancedChat"
      temperature: 0.7
      maxTokens:   4000
    }
  }

  capabilities {
    avatar:       true
    lipSync:      true
    vision:       true
    voiceToVoice: true
    claw:         false
    tools:    [tool("respondToUser"), tool("uiClick"), tool("uiNarrate"), tool("uiDescribe")]
    domains:  []
    keywords: []
  }

  knowledge: [knowledgeDomain("generalAssistantBaseline")]

  triggerBehavior {
    autoJoin:          true
    greetOnJoin:       true
    interruptionStyle: "polite"
    speakWhen:         "always"
  }

  audioControl: "mirror_user"
  videoControl: "mirror_user"
}
`

func TestParseAgentMemQL_Canonical(t *testing.T) {
	decl, err := parseAgentMemQL("unit-test", []byte(canonicalAgentSource))
	if err != nil {
		t.Fatalf("parseAgentMemQL: %v", err)
	}

	// Annotations
	if decl.version != "1.0.0" {
		t.Errorf("version: got %q want 1.0.0", decl.version)
	}
	if decl.namespace != "agents" {
		t.Errorf("namespace: got %q want agents", decl.namespace)
	}
	if decl.scope != "perUser" {
		t.Errorf("scope: got %q want perUser", decl.scope)
	}
	wantVis := []string{"bff", "cognition", "agent"}
	if len(decl.visibility) != 3 ||
		decl.visibility[0] != wantVis[0] ||
		decl.visibility[1] != wantVis[1] ||
		decl.visibility[2] != wantVis[2] {
		t.Errorf("visibility: got %v want %v", decl.visibility, wantVis)
	}
	if decl.templateFile != "templates/generalAssistant.tmpl" {
		t.Errorf("templateFile: got %q", decl.templateFile)
	}
	if decl.description != "Per-user General Assistant." {
		t.Errorf("description: got %q", decl.description)
	}

	// Header
	if decl.name != "generalAssistant" {
		t.Errorf("name: got %q want generalAssistant", decl.name)
	}

	// Top-level body fields
	if decl.role != "general_assistant" {
		t.Errorf("role: got %q", decl.role)
	}
	if decl.roleSlug != "general_assistant" {
		t.Errorf("roleSlug: got %q", decl.roleSlug)
	}
	if decl.displayName != "General Assistant" {
		t.Errorf("displayName: got %q", decl.displayName)
	}
	if decl.bodyDesc != "Designated fallback when no specialist fits." {
		t.Errorf("bodyDesc: got %q", decl.bodyDesc)
	}
	if decl.personality != "Friendly, capable, proactive." {
		t.Errorf("personality: got %q", decl.personality)
	}
	if decl.gender != "female" {
		t.Errorf("gender: got %q", decl.gender)
	}
	if decl.audioControl != "mirror_user" {
		t.Errorf("audioControl: got %q", decl.audioControl)
	}
	if decl.videoControl != "mirror_user" {
		t.Errorf("videoControl: got %q", decl.videoControl)
	}

	// providerConfig.llm
	if decl.llmPolicyName != "balancedChat" {
		t.Errorf("llm.policyName: got %q", decl.llmPolicyName)
	}
	if !decl.llmTempSet || decl.llmTemperature != 0.7 {
		t.Errorf("llm.temperature: got set=%v val=%v", decl.llmTempSet, decl.llmTemperature)
	}
	if !decl.llmMaxTokSet || decl.llmMaxTokens != 4000 {
		t.Errorf("llm.maxTokens: got set=%v val=%d", decl.llmMaxTokSet, decl.llmMaxTokens)
	}

	// capabilities
	if !decl.capAvatar || !decl.capLipSync || !decl.capVision || !decl.capVoiceToVoice {
		t.Errorf("cap booleans: avatar=%v lipSync=%v vision=%v v2v=%v",
			decl.capAvatar, decl.capLipSync, decl.capVision, decl.capVoiceToVoice)
	}
	if decl.capClaw {
		t.Errorf("cap.claw: got true, want false")
	}
	if len(decl.capDomains) != 0 {
		t.Errorf("cap.domains: got %v, want empty", decl.capDomains)
	}
	if len(decl.capKeywords) != 0 {
		t.Errorf("cap.keywords: got %v, want empty", decl.capKeywords)
	}
	if len(decl.capTools) != 4 {
		t.Fatalf("cap.tools: got %d, want 4", len(decl.capTools))
	}
	wantTools := []string{"respondToUser", "uiClick", "uiNarrate", "uiDescribe"}
	for i, want := range wantTools {
		if decl.capTools[i].Kind != "tool" {
			t.Errorf("tools[%d].Kind: got %q want tool", i, decl.capTools[i].Kind)
		}
		if decl.capTools[i].Name != want {
			t.Errorf("tools[%d].Name: got %q want %q", i, decl.capTools[i].Name, want)
		}
	}

	// knowledge
	if len(decl.knowledge) != 1 {
		t.Fatalf("knowledge: got %d, want 1", len(decl.knowledge))
	}
	if decl.knowledge[0].Kind != "knowledgeDomain" || decl.knowledge[0].Name != "generalAssistantBaseline" {
		t.Errorf("knowledge[0]: got %+v", decl.knowledge[0])
	}

	// triggerBehavior
	if !decl.tbAutoJoin || !decl.tbGreetOnJoin {
		t.Errorf("triggerBehavior bools: autoJoin=%v greetOnJoin=%v", decl.tbAutoJoin, decl.tbGreetOnJoin)
	}
	if decl.tbInterruptionStyle != "polite" {
		t.Errorf("triggerBehavior.interruptionStyle: got %q", decl.tbInterruptionStyle)
	}
	if decl.tbSpeakWhen != "always" {
		t.Errorf("triggerBehavior.speakWhen: got %q", decl.tbSpeakWhen)
	}
}

func TestParseAgentMemQL_GlobalScope(t *testing.T) {
	src := `
@version("1.0.0")
@namespace("agents")
@scope("global")
@templateFile("templates/prReviewer.tmpl")
agent prReviewer {
  role: "specialist"
  roleSlug: "pr_reviewer"
  name: "PR Reviewer"
}
`
	decl, err := parseAgentMemQL("unit-test", []byte(src))
	if err != nil {
		t.Fatalf("parseAgentMemQL: %v", err)
	}
	if decl.scope != "global" {
		t.Errorf("scope: got %q want global", decl.scope)
	}
	if decl.name != "prReviewer" {
		t.Errorf("name: got %q", decl.name)
	}
	if decl.roleSlug != "pr_reviewer" {
		t.Errorf("roleSlug: got %q", decl.roleSlug)
	}
}

func TestParseAgentMemQL_RejectsUnknownAnnotation(t *testing.T) {
	src := `
@version("1.0.0")
@namespace("agents")
@bogusAnnotation("x")
agent foo { role: "specialist" }
`
	_, err := parseAgentMemQL("unit-test", []byte(src))
	if err == nil {
		t.Fatal("expected error for unknown annotation, got nil")
	}
	if !strings.Contains(err.Error(), "unknown agent annotation") {
		t.Errorf("error message should mention unknown agent annotation, got: %v", err)
	}
}

func TestParseAgentMemQL_RejectsInvalidScope(t *testing.T) {
	src := `
@version("1.0.0")
@namespace("agents")
@scope("nonsense")
agent foo { role: "specialist" }
`
	_, err := parseAgentMemQL("unit-test", []byte(src))
	if err == nil {
		t.Fatal("expected error for invalid @scope value, got nil")
	}
	if !strings.Contains(err.Error(), "@scope must be") {
		t.Errorf("error message should mention @scope constraint, got: %v", err)
	}
}

func TestParseAgentMemQL_RejectsUnknownTopLevelField(t *testing.T) {
	src := `
@version("1.0.0")
agent foo {
  bogus: "value"
}
`
	_, err := parseAgentMemQL("unit-test", []byte(src))
	if err == nil {
		t.Fatal("expected error for unknown top-level field, got nil")
	}
	if !strings.Contains(err.Error(), "unknown top-level field") {
		t.Errorf("error message should mention unknown field, got: %v", err)
	}
}

func TestParseAgentMemQL_RejectsWrongRefKind(t *testing.T) {
	src := `
@version("1.0.0")
agent foo {
  role: "specialist"
  capabilities {
    tools: [knowledgeDomain("x")]
  }
}
`
	_, err := parseAgentMemQL("unit-test", []byte(src))
	if err == nil {
		t.Fatal("expected error for wrong ref kind, got nil")
	}
	if !strings.Contains(err.Error(), "expected tool") {
		t.Errorf("error message should mention expected ref kind, got: %v", err)
	}
}

func TestParseAgentMemQL_NoAgentDeclFails(t *testing.T) {
	src := `
@version("1.0.0")
// no agent block here
`
	_, err := parseAgentMemQL("unit-test", []byte(src))
	if err == nil {
		t.Fatal("expected error when no agent declaration present, got nil")
	}
}

func TestParseAgentMemQL_LegacyFuncFormRejected(t *testing.T) {
	src := `
func (Agent) foo() {}
`
	_, err := parseAgentMemQL("unit-test", []byte(src))
	if err == nil {
		t.Fatal("expected error for legacy func form, got nil")
	}
	if !strings.Contains(err.Error(), "canonical struct form") {
		t.Errorf("error should mention canonical form, got: %v", err)
	}
}
