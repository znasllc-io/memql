package authoring

// authoring_sandbox_automation_test.go -- integration coverage for the
// automation-compile seam (issue #956, follow-up 2/3).
//
// This file is in the EXTERNAL test package so it can import
// component/automations (whose init() registers the compile hook) alongside
// component/memql. The internal package tests run without automations linked
// and exercise the hook-unregistered (skipped) path; this file exercises the
// real compile path. The blank import is what wires the hook.

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"

	// Side-effect import: registers the automation-compile hook the sandbox
	// uses for the `automation` construct kind.
	_ "github.com/znasllc-io/memql/component/automations"
)

// loadConceptsForSandbox mirrors the engine-init smoke test: it loads the
// full embedded DSL concept tree so automations whose triggers / steps name
// real concepts can resolve. Safe to call repeatedly (idempotent merge).
func loadConceptsForSandbox(t *testing.T) {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
}

// TestSandboxAutomation_ValidScheduledCompiles: a well-formed scheduled
// automation compiles cleanly once the automations package is linked.
func TestSandboxAutomation_ValidScheduledCompiles(t *testing.T) {
	loadConceptsForSandbox(t)

	rep := memql.SandboxCompileBundle([]memql.SandboxConstruct{
		{
			Kind: "automation",
			Name: "sandboxScheduledSweep",
			Source: `@enabled
@trigger(schedule="0 0 4 * * *")
@description("Daily sandbox sweep")
automation sandboxScheduledSweep {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`,
		},
	})

	if !rep.OK {
		t.Fatalf("expected valid automation to compile, got: %+v", rep.Diagnostics)
	}
	d := rep.Diagnostics[0]
	if !d.OK || d.Skipped || d.Error != "" {
		t.Errorf("expected clean automation compile, got %+v", d)
	}
}

// TestSandboxAutomation_StructuredTriggerCompiles: the structured @trigger
// form (event= + concept=) compiles, exercising trigger normalization.
func TestSandboxAutomation_StructuredTriggerCompiles(t *testing.T) {
	loadConceptsForSandbox(t)

	rep := memql.SandboxCompileBundle([]memql.SandboxConstruct{
		{
			Kind: "automation",
			Name: "sandboxOnUserCreate",
			Source: `@enabled
@trigger(event="node.created", concept="v1:identity:user")
@description("Sandbox: react to user creation")
automation sandboxOnUserCreate {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`,
		},
	})

	if !rep.OK {
		t.Fatalf("expected structured-trigger automation to compile, got: %+v", rep.Diagnostics)
	}
}

// TestSandboxAutomation_SyntaxErrorFails: a malformed automation fails the
// bundle with a precise diagnostic carrying the origin.
func TestSandboxAutomation_SyntaxErrorFails(t *testing.T) {
	loadConceptsForSandbox(t)

	rep := memql.SandboxCompileBundle([]memql.SandboxConstruct{
		{
			Kind: "automation",
			Name: "sandboxBroken",
			// Missing closing brace on the step block.
			Source: `@trigger(schedule="0 0 4 * * *")
automation sandboxBroken {
  step run {
    logic sandboxNoopLogic { event: event }
}`,
		},
	})

	if rep.OK {
		t.Fatalf("expected bundle FAIL on broken automation, got OK: %+v", rep.Diagnostics)
	}
	d := rep.Diagnostics[0]
	if d.OK || d.Skipped || d.Error == "" {
		t.Errorf("expected a hard failure with a diagnostic, got %+v", d)
	}
	if !strings.Contains(d.Error, "sandbox:automation:sandboxBroken") {
		t.Errorf("diagnostic should carry the sandbox origin, got %q", d.Error)
	}
}

// TestSandboxAutomation_StructuredTriggerMissingConceptFails: the structured
// trigger form without a concept= is rejected with a precise diagnostic --
// the same bind-time check the bootstrap loader enforces.
func TestSandboxAutomation_StructuredTriggerMissingConceptFails(t *testing.T) {
	loadConceptsForSandbox(t)

	rep := memql.SandboxCompileBundle([]memql.SandboxConstruct{
		{
			Kind: "automation",
			Name: "sandboxBadTrigger",
			Source: `@trigger(event="node.created")
automation sandboxBadTrigger {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`,
		},
	})

	if rep.OK {
		t.Fatalf("expected bundle FAIL on structured trigger missing concept=, got OK: %+v", rep.Diagnostics)
	}
	d := rep.Diagnostics[0]
	if d.OK || d.Skipped || !strings.Contains(d.Error, "concept") {
		t.Errorf("expected a missing-concept trigger diagnostic, got %+v", d)
	}
}

// TestSandboxAutomation_NameMismatchFails: the declared construct.Name must
// equal the automation name in the source.
func TestSandboxAutomation_NameMismatchFails(t *testing.T) {
	loadConceptsForSandbox(t)

	rep := memql.SandboxCompileBundle([]memql.SandboxConstruct{
		{
			Kind: "automation",
			Name: "claimedName",
			Source: `@trigger(schedule="0 0 4 * * *")
automation actualAutomationName {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`,
		},
	})

	if rep.OK {
		t.Fatalf("expected bundle FAIL on automation name mismatch, got OK: %+v", rep.Diagnostics)
	}
	d := rep.Diagnostics[0]
	if d.OK || d.Skipped || !strings.Contains(d.Error, "does not match") {
		t.Errorf("expected a name-mismatch failure, got %+v", d)
	}
}

// TestSandboxAutomation_NoMutationOfConceptRegistry: compiling an automation
// in the sandbox must not add/remove anything from the live concept registry.
func TestSandboxAutomation_NoMutationOfConceptRegistry(t *testing.T) {
	loadConceptsForSandbox(t)

	before := len(memoryNodes.List())
	_ = memql.SandboxCompileBundle([]memql.SandboxConstruct{
		{
			Kind: "automation",
			Name: "sandboxScheduledSweep",
			Source: `@trigger(schedule="0 0 4 * * *")
automation sandboxScheduledSweep {
  step run {
    logic sandboxNoopLogic { event: event }
  }
}`,
		},
	})
	if after := len(memoryNodes.List()); after != before {
		t.Fatalf("sandbox automation compile mutated the concept registry: before=%d after=%d", before, after)
	}
}
