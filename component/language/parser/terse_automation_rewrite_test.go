package parser

import (
	"strings"
	"testing"
)

// The terse single-step automation form (memql#2215, ADR §2.4 / §7)
//
//	automation NAME @trigger(event="...") => logic logicName
//
// must lower to the canonical longhand block and then -- via the
// existing NormaliseAutomationSource stage -- compile to a procedural
// function byte-for-byte identical to the hand-written longhand. This
// is what guarantees there is no separate terse execution path and so
// no dry-run/live divergence.
func TestTerseAutomation_LowersIdenticalToLonghand(t *testing.T) {
	terse := `@enabled
@description("Register this node on startup.")
automation registerNode @trigger(event="system.startup") => logic registerNode`

	longhand := `@enabled
@description("Register this node on startup.")
@trigger(event="system.startup")
automation registerNode {
  step run {
    logic registerNode { event: event }
  }
}`

	terseOut, err := NormaliseAll(terse)
	if err != nil {
		t.Fatalf("terse NormaliseAll error: %v", err)
	}
	longhandOut, err := NormaliseAll(longhand)
	if err != nil {
		t.Fatalf("longhand NormaliseAll error: %v", err)
	}
	if terseOut != longhandOut {
		t.Fatalf("terse and longhand must lower identically.\nterse:\n%s\nlonghand:\n%s", terseOut, longhandOut)
	}
	if !strings.Contains(terseOut, "func (Automation) registerNode") {
		t.Fatalf("expected procedural automation rewrite; got %q", terseOut)
	}
	if !strings.Contains(terseOut, "logic registerNode { event: event }") &&
		!strings.Contains(terseOut, "registerNode({ event: event })") {
		t.Fatalf("expected the single logic step to forward event; got %q", terseOut)
	}
}

// The schedule trigger variant lowers the same way.
func TestTerseAutomation_ScheduleTrigger(t *testing.T) {
	terse := `automation pruneStaleClusterNodes @trigger(schedule="0 */10 * * * *") => logic pruneStaleClusterNodes`
	out, err := NormaliseAll(terse)
	if err != nil {
		t.Fatalf("NormaliseAll error: %v", err)
	}
	if !strings.Contains(out, `@trigger(schedule="0 */10 * * * *")`) {
		t.Fatalf("expected the schedule trigger to be hoisted; got %q", out)
	}
	if !strings.Contains(out, "func (Automation) pruneStaleClusterNodes") {
		t.Fatalf("expected procedural rewrite; got %q", out)
	}
}

// A structured event trigger (event + concept) survives the lowering.
func TestTerseAutomation_StructuredTrigger(t *testing.T) {
	terse := `automation onDeploy @trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*") => logic handleDeploy`
	if !LooksLikeTerseAutomation(terse) {
		t.Fatalf("structured-trigger terse form should be detected")
	}
	out, err := NormaliseTerseAutomationSource(terse)
	if err != nil {
		t.Fatalf("lower error: %v", err)
	}
	if !strings.Contains(out, `@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")`) {
		t.Fatalf("expected structured trigger preserved; got %q", out)
	}
	if !strings.Contains(out, "logic handleDeploy { event: event }") {
		t.Fatalf("expected single logic step; got %q", out)
	}
}

// Longhand automations and non-terse lines are left untouched by the
// terse detector/rewriter (no false positives).
func TestTerseAutomation_DoesNotMatchLonghand(t *testing.T) {
	longhand := `@trigger(event="system.startup")
automation registerNode {
  step run {
    logic registerNode { event: event }
  }
}`
	if LooksLikeTerseAutomation(longhand) {
		t.Fatalf("longhand automation must not be detected as terse")
	}
	out, err := NormaliseTerseAutomationSource(longhand)
	if err != nil {
		t.Fatalf("lower error: %v", err)
	}
	if out != longhand {
		t.Fatalf("longhand must pass through unchanged; got %q", out)
	}
}

// Indentation on the terse line is preserved into the expansion (the
// rewriter keys every emitted line off the captured leading whitespace).
func TestTerseAutomation_PreservesIndent(t *testing.T) {
	terse := "  automation registerNode @trigger(event=\"system.startup\") => logic registerNode"
	out, err := NormaliseTerseAutomationSource(terse)
	if err != nil {
		t.Fatalf("lower error: %v", err)
	}
	if !strings.Contains(out, "  automation registerNode {") {
		t.Fatalf("expected preserved indent on automation line; got %q", out)
	}
	if !strings.Contains(out, "    step run {") {
		t.Fatalf("expected indented step; got %q", out)
	}
}
