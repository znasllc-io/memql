package parser

import (
	"strings"
	"testing"
)

// memql#1366 -- the struct-form automation rewriter supports the
// conditional step body `if <cond> { <call> }`, emitting
// `<step> := if <cond> { <translatedCall> }` which parseGoStyleStep
// already accepts (StepDef.Condition). This is what the phased-authoring
// headline synthesizer emits to gate layer k on layer k-1's success.

func TestNormaliseAutomationSource_ConditionalStep(t *testing.T) {
	src := `@description("Run the digest in two phases.")
automation digest {
  step digestPhase0 {
    automation digestPhase0 { }
  }
  step digestPhase1 {
    if steps.digestPhase0.status == "success" {
      automation digestPhase1 { }
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `digestPhase1 := if steps.digestPhase0.status == "success" { automationDigestPhase1(`) {
		t.Fatalf("conditional step did not rewrite to the `if cond { call }` assignment; got:\n%s", out)
	}
	if !strings.Contains(out, "digestPhase0 := automationDigestPhase0(") {
		t.Fatalf("ungated step must keep the plain assignment; got:\n%s", out)
	}
}

func TestNormaliseAutomationSource_ConditionalStep_CompoundCondition(t *testing.T) {
	src := `automation gather {
  step fetchA {
    automation fetchA { }
  }
  step merge {
    if steps.fetchA.status == "success" && steps.fetchB.status == "success" {
      automation merge { }
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `merge := if steps.fetchA.status == "success" && steps.fetchB.status == "success" { automationMerge(`) {
		t.Fatalf("compound condition must pass through verbatim; got:\n%s", out)
	}
}

func TestNormaliseAutomationSource_ConditionalStep_Errors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing brace after condition",
			body:    "if steps.a.status",
			wantErr: "expected `{` after the if condition",
		},
		{
			name:    "missing condition",
			body:    "if { automation x { } }",
			wantErr: "missing condition",
		},
		{
			name:    "empty body",
			body:    "if steps.a.status { }",
			wantErr: "if body is empty",
		},
		{
			name:    "trailing text after body",
			body:    "if steps.a.status { automation x { } } junk",
			wantErr: "unexpected trailing text",
		},
		// NOTE: a non-call if body (e.g. `if c { 42 }`) intentionally passes
		// the rewriter -- expression-shaped bodies are the procedural
		// parser's contract to reject ("step RHS must be a function call").
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "automation bad {\n  step s {\n    " + tc.body + "\n  }\n}"
			_, err := NormaliseAutomationSource(src)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
