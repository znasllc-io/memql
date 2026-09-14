package memql

import (
	"reflect"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// TestPolicyDeclToPolicyConfig_RequiresPrimary: a policy without @primary is
// refused on the path every policy file loads through -- the shared parser,
// then policyDeclToPolicyConfig. (The hand-rolled parser that carried this
// test was reached by nothing else and is deleted, memql#5359.)
func TestPolicyDeclToPolicyConfig_RequiresPrimary(t *testing.T) {
	decl, err := languageParser.ParsePolicyDecl("@description(\"missing primary\")\n@fallback(\"a\")\npolicy orphan { }")
	if err != nil {
		t.Fatalf("ParsePolicyDecl: %v", err)
	}
	if _, err := policyDeclToPolicyConfig(decl); err == nil || !strings.Contains(err.Error(), "@primary") {
		t.Fatalf("want a refusal naming @primary, got: %v", err)
	}
}

// TestPolicyConfigProviderChainOrder: the chain the Router walks is the
// primary, then the fallbacks in declaration order, blank entries dropped.
func TestPolicyConfigProviderChainOrder(t *testing.T) {
	decl, err := languageParser.ParsePolicyDecl("@primary(\"primaryProvider\")\n@fallback(\"fallbackA\")\n@fallback(\"fallbackB\")\npolicy chain { }")
	if err != nil {
		t.Fatalf("ParsePolicyDecl: %v", err)
	}
	cfg, err := policyDeclToPolicyConfig(decl)
	if err != nil {
		t.Fatalf("policyDeclToPolicyConfig: %v", err)
	}
	if got, want := cfg.ProviderChain(), []string{"primaryProvider", "fallbackA", "fallbackB"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProviderChain = %v, want %v", got, want)
	}
	blank := PolicyConfig{Primary: "p", Fallbacks: []string{" ", "f"}}
	if got, want := blank.ProviderChain(), []string{"p", "f"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProviderChain with a blank fallback = %v, want %v", got, want)
	}
}
