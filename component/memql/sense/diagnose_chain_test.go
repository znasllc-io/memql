package sense

// diagnose_chain_test.go -- Diagnose answers a diagnostic for an over-long
// expression chain instead of ending the process.
//
// Diagnose is the reachable end of the path the chain bound protects: the six
// Sense requests arrive on an authenticated stream, so any signed-in client
// can send source, and Diagnose parses it. Before the bound, the source in
// TestDiagnoseRefusesACrashDepthChain ended the process inside the parser's
// own walk over what it had just built -- a `fatal error: stack overflow`,
// which no recover() sees, so the pod died with every other client's work on
// it.

import (
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/language/parser"
)

// diagnoseChainSource is a logic whose body is one member chain of n links.
func diagnoseChainSource(links int) string {
	return "logic chainProbe {\n  args {\n    a string @required\n  }\n  return args" +
		strings.Repeat(".a", links) + "\n}\n"
}

// The measured crash payload: twelve million member reads in an automation
// body. It is now a diagnostic, in about the time it takes to lex it.
func TestDiagnoseRefusesACrashDepthChain(t *testing.T) {
	const links = 12_000_000
	source := "automation crashProbe { x := mutation m(v: args" + strings.Repeat(".a", links) + ") }"

	start := time.Now()
	diags := errorDiags(New(nil).Diagnose(source, "probe.memql"))
	elapsed := time.Since(start)
	t.Logf("diagnosed %d bytes in %v", len(source), elapsed.Round(time.Millisecond))

	if len(diags) == 0 {
		t.Fatal("an over-long chain must be diagnosed, not walked")
	}
	if !strings.Contains(diags[0].Message, parser.RuleExpressionChainTooLong) {
		t.Errorf("the diagnostic must carry the rule id %q: %s", parser.RuleExpressionChainTooLong, diags[0].Message)
	}
	if diags[0].Range.Start.Line < 1 {
		t.Errorf("the diagnostic has no position: %+v", diags[0].Range)
	}
	if elapsed > time.Minute {
		t.Errorf("the diagnosis took %v", elapsed)
	}
}

// Both sides of the bound through the same entry point: at the bound the
// source is diagnosed clean, one link past it the chain is the only problem.
func TestDiagnoseAtAndPastTheChainBound(t *testing.T) {
	svc := New(nil)

	if diags := errorDiags(svc.Diagnose(diagnoseChainSource(parser.MaxExpressionChain), "probe.memql")); len(diags) != 0 {
		t.Fatalf("a chain of exactly %d links must diagnose clean: %+v", parser.MaxExpressionChain, diags)
	}

	diags := errorDiags(svc.Diagnose(diagnoseChainSource(parser.MaxExpressionChain+1), "probe.memql"))
	if len(diags) == 0 {
		t.Fatalf("a chain of %d links must be diagnosed", parser.MaxExpressionChain+1)
	}
	if !strings.Contains(diags[0].Message, "the expression chain is too long (more than 4096 links)") ||
		!strings.Contains(diags[0].Message, parser.RuleExpressionChainTooLong) {
		t.Errorf("diagnostic wording: %s", diags[0].Message)
	}
}
