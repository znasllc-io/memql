package memql

import (
	"strings"
	"testing"
)

// TestParsePolicyMemQL_GoldenPath locks the SI-router policy
// surface: @primary + @fallback + tuning knobs + empty `policy NAME { }`
// declaration.
//
// (Cross-cutting decision policies use `func (Policy)` and are
// parsed via the general function-parser path, not this routing
// parser.)
func TestParsePolicyMemQL_GoldenPath(t *testing.T) {
	src := []byte(`@description("Balanced LLM for most agent replies.")
@primary("chat54Mini")
@fallback("chat53")
@maxLatencyMs(8000)
@maxTimeToFirstTokenMs(500)
@preferredRole("assistant")
policy balancedChat { }`)

	cfg, err := parsePolicyMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil *PolicyConfig")
	}
	if cfg.Name != "balancedChat" {
		t.Errorf("Name = %q, want balancedChat", cfg.Name)
	}
	if cfg.Primary != "chat54Mini" {
		t.Errorf("Primary = %q, want chat54Mini", cfg.Primary)
	}
	if len(cfg.Fallbacks) != 1 || cfg.Fallbacks[0] != "chat53" {
		t.Errorf("Fallbacks = %v, want [chat53]", cfg.Fallbacks)
	}
	if cfg.MaxLatencyMs != 8000 {
		t.Errorf("MaxLatencyMs = %d, want 8000", cfg.MaxLatencyMs)
	}
	if cfg.MaxTimeToFirstTokenMs != 500 {
		t.Errorf("MaxTimeToFirstTokenMs = %d, want 500", cfg.MaxTimeToFirstTokenMs)
	}
	if len(cfg.PreferredRoles) != 1 || cfg.PreferredRoles[0] != "assistant" {
		t.Errorf("PreferredRoles = %v, want [assistant]", cfg.PreferredRoles)
	}
}

// TestParsePolicyMemQL_ProviderChainOrder locks the ordering
// semantics: primary first, then fallbacks in declaration order.
func TestParsePolicyMemQL_ProviderChainOrder(t *testing.T) {
	src := []byte(`@primary("primaryProvider")
@fallback("fallbackA")
@fallback("fallbackB")
policy chain { }`)

	cfg, err := parsePolicyMemQL("test.memql", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	chain := cfg.ProviderChain()
	if len(chain) != 3 {
		t.Fatalf("ProviderChain len = %d, want 3", len(chain))
	}
	want := []string{"primaryProvider", "fallbackA", "fallbackB"}
	for i, w := range want {
		if chain[i] != w {
			t.Errorf("chain[%d] = %q, want %q", i, chain[i], w)
		}
	}
}

// TestParsePolicyMemQL_RequiresPrimary locks the rule: a routing
// policy without @primary is an error.
func TestParsePolicyMemQL_RequiresPrimary(t *testing.T) {
	src := []byte(`@description("missing primary")
@fallback("a")
policy orphan { }`)

	_, err := parsePolicyMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "@primary") {
		t.Errorf("error should mention @primary, got %v", err)
	}
}

// TestParsePolicyMemQL_RequiresPolicyDeclaration locks the rule:
// a file without `policy NAME { }` is an error.
func TestParsePolicyMemQL_RequiresPolicyDeclaration(t *testing.T) {
	src := []byte(`@primary("foo")`)

	_, err := parsePolicyMemQL("test.memql", src)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "policy") {
		t.Errorf("error should mention missing policy declaration, got %v", err)
	}
}
