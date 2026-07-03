package parser

import (
	"reflect"
	"strings"
	"testing"
)

// TestParsePolicyDecl_GoldenPath locks the canonical AI-router
// policy shape: @primary + @fallback + tuning knobs + empty
// `policy NAME { }` declaration. Mirrors
// dsl/v1/policies/v1/balancedChat-style files.
func TestParsePolicyDecl_GoldenPath(t *testing.T) {
	source := `@description("Balanced LLM for most agent replies.")
@primary("chat54Mini")
@fallback("chat53")
@maxLatencyMs(8000)
@maxTimeToFirstTokenMs(500)
@preferredRole("assistant")
policy balancedChat { }`

	got, err := ParsePolicyDecl(source)
	if err != nil {
		t.Fatalf("ParsePolicyDecl: %v", err)
	}
	if got.Name != "balancedChat" {
		t.Errorf("Name = %q, want balancedChat", got.Name)
	}
	if got.Description != "Balanced LLM for most agent replies." {
		t.Errorf("Description = %q", got.Description)
	}
	if got.Primary != "chat54Mini" {
		t.Errorf("Primary = %q, want chat54Mini", got.Primary)
	}
	if !reflect.DeepEqual(got.Fallbacks, []string{"chat53"}) {
		t.Errorf("Fallbacks = %v, want [chat53]", got.Fallbacks)
	}
	if got.MaxLatencyMs != 8000 {
		t.Errorf("MaxLatencyMs = %d, want 8000", got.MaxLatencyMs)
	}
	if got.MaxTimeToFirstTokenMs != 500 {
		t.Errorf("MaxTimeToFirstTokenMs = %d, want 500", got.MaxTimeToFirstTokenMs)
	}
	if !reflect.DeepEqual(got.PreferredRoles, []string{"assistant"}) {
		t.Errorf("PreferredRoles = %v, want [assistant]", got.PreferredRoles)
	}
}

// TestParsePolicyDecl_MultipleFallbacksAndRoles locks the
// repeatable-annotation behaviour: multiple @fallback /
// @preferredRole annotations accumulate in declaration order.
func TestParsePolicyDecl_MultipleFallbacksAndRoles(t *testing.T) {
	source := `@primary("primaryProvider")
@fallback("fallbackA")
@fallback("fallbackB")
@fallback("fallbackC")
@preferredRole("assistant")
@preferredRole("specialist")
policy multiChain { }`

	got, err := ParsePolicyDecl(source)
	if err != nil {
		t.Fatalf("ParsePolicyDecl: %v", err)
	}
	if !reflect.DeepEqual(got.Fallbacks, []string{"fallbackA", "fallbackB", "fallbackC"}) {
		t.Errorf("Fallbacks = %v, want [fallbackA fallbackB fallbackC]", got.Fallbacks)
	}
	if !reflect.DeepEqual(got.PreferredRoles, []string{"assistant", "specialist"}) {
		t.Errorf("PreferredRoles = %v, want [assistant specialist]", got.PreferredRoles)
	}
}

// TestParsePolicyDecl_MinimalShape locks a policy with only
// @primary (no fallbacks, no tuning knobs, no roles). The
// converter on the memql side enforces @primary being present;
// the parser itself is permissive.
func TestParsePolicyDecl_MinimalShape(t *testing.T) {
	source := `@primary("onlyProvider")
policy minimal { }`

	got, err := ParsePolicyDecl(source)
	if err != nil {
		t.Fatalf("ParsePolicyDecl: %v", err)
	}
	if got.Primary != "onlyProvider" {
		t.Errorf("Primary = %q, want onlyProvider", got.Primary)
	}
	if len(got.Fallbacks) != 0 {
		t.Errorf("Fallbacks = %v, want empty", got.Fallbacks)
	}
	if got.MaxLatencyMs != 0 {
		t.Errorf("MaxLatencyMs = %d, want 0 (unset)", got.MaxLatencyMs)
	}
}

// TestParsePolicyDecl_RejectsUnknownAttribute locks the memql#2395
// HOLE-1 fix: an unknown @-annotation is REJECTED against the canonical
// annotations.ByReceiver registry (the previous drain-and-skip tolerance
// silently swallowed typos like @primry; a future tuning knob ships by
// adding it to the registry first, which is the single source of truth).
func TestParsePolicyDecl_RejectsUnknownAttribute(t *testing.T) {
	source := `@primary("p")
@futureKnob("ignored")
policy tolerant { }`

	_, err := ParsePolicyDecl(source)
	if err == nil {
		t.Fatal("ParsePolicyDecl accepted an unknown annotation @futureKnob (registry gate missing)")
	}
	if !strings.Contains(err.Error(), "unknown annotation @futureKnob") {
		t.Errorf("error should name the unknown annotation, got: %v", err)
	}
}

// TestParsePolicyDecl_RejectsMissingName errors when the policy
// keyword isn't followed by an identifier.
func TestParsePolicyDecl_RejectsMissingName(t *testing.T) {
	source := `@primary("p")
policy { }`

	_, err := ParsePolicyDecl(source)
	if err == nil {
		t.Fatal("expected error for missing policy name, got nil")
	}
	if !strings.Contains(err.Error(), "policy") {
		t.Errorf("error should mention policy, got %v", err)
	}
}
