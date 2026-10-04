package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/znasllc-io/memql/component/outbound"
)

// TestOutboundSecretNamePatternMatchesTheDSL holds one rule written twice
// (memql#5480). The name of a secret target is interpolated into the engine's
// resolver query, so stageOutboundRequestToSecret's targetSecret arg refuses a
// name outside its @pattern before a row can exist, and the outbound worker
// refuses the same names again before it resolves anything, because a row can
// reach the worker without ever meeting that arg. Two copies of a rule drift
// unless something holds them together: this reads the DSL's copy off the file
// and compares it with the worker's.
func TestOutboundSecretNamePatternMatchesTheDSL(t *testing.T) {
	const file = "dsl/platform/mutations.memql"
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	block := regexp.MustCompile(`(?s)\nmutation outboundRequest stageOutboundRequestToSecret \{.*?\n\}`).Find(src)
	if block == nil {
		t.Fatalf("%s declares no stageOutboundRequestToSecret: this gate is reading the wrong file, "+
			"or the mutation moved and the gate must move with it", file)
	}
	m := regexp.MustCompile(`(?m)^\s*targetSecret\s+string!\s+@pattern\("([^"]*)"\)`).FindSubmatch(block)
	if m == nil {
		t.Fatalf("stageOutboundRequestToSecret's targetSecret arg carries no @pattern. The resolver "+
			"interpolates the name into its lookup query, so the mutation must hold it to %s "+
			"(outbound.SecretNamePattern) before a row exists", outbound.SecretNamePattern)
	}
	if got := string(m[1]); got != outbound.SecretNamePattern {
		t.Fatalf("the DSL holds a secret target's name to %q and the worker to %q. They are one rule: "+
			"a name the mutation admits and the worker refuses fails every delivery naming it, and "+
			"one the worker admits and the mutation refuses is a check nothing reaches", got, outbound.SecretNamePattern)
	}
}
