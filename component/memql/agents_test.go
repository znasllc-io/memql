package memql

import (
	"testing"
)

func TestCompileAgentDecl_Canonical(t *testing.T) {
	decl, err := parseAgentMemQL("unit-test", []byte(canonicalAgentSource))
	if err != nil {
		t.Fatalf("parseAgentMemQL: %v", err)
	}
	def, err := compileAgentDecl(decl)
	if err != nil {
		t.Fatalf("compileAgentDecl: %v", err)
	}

	if def.Name != "generalAssistant" {
		t.Errorf("Name: got %q", def.Name)
	}
	if def.Namespace != "agents" {
		t.Errorf("Namespace: got %q", def.Namespace)
	}
	if def.Scope != AgentScopePerUser {
		t.Errorf("Scope: got %q want perUser", def.Scope)
	}
	if def.TemplateFile != "templates/generalAssistant.tmpl" {
		t.Errorf("TemplateFile: got %q", def.TemplateFile)
	}
	if def.SystemPrompt != "" {
		t.Errorf("SystemPrompt should be empty until loader fills it; got %q", def.SystemPrompt)
	}

	if def.Role != "general_assistant" || def.RoleSlug != "general_assistant" {
		t.Errorf("Role/RoleSlug: %q/%q", def.Role, def.RoleSlug)
	}
	if def.DisplayName != "General Assistant" {
		t.Errorf("DisplayName: %q", def.DisplayName)
	}

	if def.LLMPolicyName != "balancedChat" || !def.LLMTempSet || def.LLMTemperature != 0.7 ||
		!def.LLMMaxTokSet || def.LLMMaxTokens != 4000 {
		t.Errorf("LLM config: %+v", def)
	}

	if !def.CapAvatar || !def.CapLipSync || !def.CapVision || !def.CapVoiceToVoice || def.CapClaw {
		t.Errorf("Capability flags wrong: %+v", def)
	}
	if len(def.CapTools) != 4 {
		t.Fatalf("CapTools: got %d, want 4", len(def.CapTools))
	}
	wantToolNames := []string{"respondToUser", "uiClick", "uiNarrate", "uiDescribe"}
	for i, want := range wantToolNames {
		if def.CapTools[i].Name != want {
			t.Errorf("CapTools[%d].Name: got %q want %q", i, def.CapTools[i].Name, want)
		}
	}

	if len(def.Knowledge) != 1 || def.Knowledge[0].Name != "generalAssistantBaseline" {
		t.Errorf("Knowledge: got %+v", def.Knowledge)
	}

	if !def.TBAutoJoin || !def.TBGreetOnJoin ||
		def.TBInterruptionStyle != "polite" || def.TBSpeakWhen != "always" {
		t.Errorf("TriggerBehavior: %+v", def)
	}

	if def.AudioControl != "mirror_user" || def.VideoControl != "mirror_user" {
		t.Errorf("AudioControl/VideoControl: %q/%q", def.AudioControl, def.VideoControl)
	}
}

func TestCompileAgentDecl_DefaultsScopeToPerUser(t *testing.T) {
	src := `
@version("1.0.0")
@namespace("agents")
agent foo { role: "specialist" }
`
	decl, err := parseAgentMemQL("unit-test", []byte(src))
	if err != nil {
		t.Fatalf("parseAgentMemQL: %v", err)
	}
	def, err := compileAgentDecl(decl)
	if err != nil {
		t.Fatalf("compileAgentDecl: %v", err)
	}
	if def.Scope != AgentScopePerUser {
		t.Errorf("Scope: got %q want perUser (default when @scope omitted)", def.Scope)
	}
}

func TestCompileAgentDecl_DefaultsRoleToSpecialist(t *testing.T) {
	src := `
@version("1.0.0")
@namespace("agents")
agent foo {}
`
	decl, err := parseAgentMemQL("unit-test", []byte(src))
	if err != nil {
		t.Fatalf("parseAgentMemQL: %v", err)
	}
	def, err := compileAgentDecl(decl)
	if err != nil {
		t.Fatalf("compileAgentDecl: %v", err)
	}
	if def.Role != "specialist" {
		t.Errorf("Role: got %q want specialist", def.Role)
	}
}

func TestCompileAgentDecl_GlobalScope(t *testing.T) {
	src := `
@version("1.0.0")
@namespace("agents")
@scope("global")
@templateFile("templates/x.tmpl")
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
	def, err := compileAgentDecl(decl)
	if err != nil {
		t.Fatalf("compileAgentDecl: %v", err)
	}
	if def.Scope != AgentScopeGlobal {
		t.Errorf("Scope: got %q want global", def.Scope)
	}
}

func TestAgentRegistry_UpsertGet(t *testing.T) {
	reg := NewAgentRegistry()

	// Get on empty registry returns ok=false.
	if got, ok := reg.Get("missing"); ok || got != nil {
		t.Errorf("Get on empty: got=%v ok=%v", got, ok)
	}

	a := &AgentDefinition{Name: "alpha", Role: "specialist"}
	if err := reg.Upsert(a); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, ok := reg.Get("alpha")
	if !ok || got == nil {
		t.Fatalf("Get after Upsert: ok=%v got=%v", ok, got)
	}
	if got.Name != "alpha" || got.Role != "specialist" {
		t.Errorf("retrieved def wrong: %+v", got)
	}

	// Replace via Upsert.
	a2 := &AgentDefinition{Name: "alpha", Role: "general_assistant"}
	if err := reg.Upsert(a2); err != nil {
		t.Fatalf("Upsert (replace): %v", err)
	}
	got, ok = reg.Get("alpha")
	if !ok || got.Role != "general_assistant" {
		t.Errorf("Upsert did not replace; got %+v", got)
	}

	// Names lists registered entries.
	names := reg.Names()
	if len(names) != 1 || names[0] != "alpha" {
		t.Errorf("Names: got %v", names)
	}

	// Nil receiver / nil definition / empty name -- no panic.
	var nilReg *AgentRegistry
	if _, ok := nilReg.Get("anything"); ok {
		t.Error("nil registry Get should return ok=false")
	}
	if err := reg.Upsert(nil); err != nil {
		t.Errorf("Upsert(nil) should be a no-op, got: %v", err)
	}
	if err := reg.Upsert(&AgentDefinition{Name: ""}); err != nil {
		t.Errorf("Upsert empty name should be a no-op, got: %v", err)
	}
}

func TestValidateAgentToolRefs_SkipsKnowledgeRefs(t *testing.T) {
	// validateAgentToolRefs only checks tool refs, not knowledge.
	// Confirm that knowledge refs in the def don't get reported as
	// unresolved (they're never inspected by this function).
	def := &AgentDefinition{
		Name:      "foo",
		CapTools:  []AgentToolRef{}, // empty -> no validation work
		Knowledge: []AgentKnowledgeRef{{Name: "someDomain"}},
	}
	got := validateAgentToolRefs(def, newToolRegistry())
	if len(got) != 0 {
		t.Errorf("expected no unresolved refs (empty CapTools), got %v", got)
	}
}

func TestValidateAgentToolRefs_NilSafe(t *testing.T) {
	// Nil registry / nil def -- no panic.
	if got := validateAgentToolRefs(nil, nil); got != nil {
		t.Errorf("nil inputs should return nil, got %v", got)
	}
	if got := validateAgentToolRefs(&AgentDefinition{}, nil); got != nil {
		t.Errorf("nil registry should return nil, got %v", got)
	}
}
