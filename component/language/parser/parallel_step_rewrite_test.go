package parser

import (
	"strings"
	"testing"
)

// memql#1368 -- the struct-form automation rewriter supports the `parallel`
// step body:
//
//	step layer0 {
//	  parallel {
//	    wait: "all"        // all | any | none (default: all)
//	    failFast: true      // default: false
//	    branches: [
//	      step sales   { automation fetchSales { } },
//	      step support { automation fetchSupport { } }
//	    ]
//	  }
//	}
//
// emitting `layer0 := parallel { wait: ..., failFast: ..., branches: [ sales
// := automationFetchSales(...) support := ... ] }`, which parseGoStyleStep
// parses into StepTypeParallel. This is the grammar the phased-authoring
// headline synthesizer emits for a multi-phase topo layer (restoring the
// #1164 within-layer concurrency removed in PR #1367).

func TestNormaliseAutomationSource_ParallelStep(t *testing.T) {
	src := `@description("Gather two reports concurrently.")
automation gather {
  step layer0 {
    parallel {
      wait: "all"        // all | any | none (default: all)
      failFast: true      // default: false
      branches: [
        step sales   { automation fetchSales { } },
        step support { automation fetchSupport { } }
      ]
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `layer0 := parallel { wait: "all", failFast: true, branches: [ sales := automationFetchSales() support := automationFetchSupport() ] }`) {
		t.Fatalf("parallel step did not rewrite to the procedural parallel assignment; got:\n%s", out)
	}
}

func TestNormaliseAutomationSource_ParallelStep_Defaults(t *testing.T) {
	src := `automation gather {
  step layer0 {
    parallel {
      branches: [
        step a { automation fetchA { } },
        step b { automation fetchB { } }
      ]
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `parallel { wait: "all", failFast: false, branches: [`) {
		t.Fatalf("omitted wait/failFast must default to \"all\"/false; got:\n%s", out)
	}
}

// Keys may appear in any order; branch bodies support per-branch `if`
// gating (the executor honors branch Condition inside a parallel).
func TestNormaliseAutomationSource_ParallelStep_KeyOrderAndGatedBranch(t *testing.T) {
	src := `automation gather {
  step layer0 {
    parallel {
      branches: [
        step a { if steps.prep.status == "success" { automation fetchA { } } },
        step b { automation fetchB { } }
      ]
      failFast: true
      wait: "any"
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `parallel { wait: "any", failFast: true, branches: [`) {
		t.Fatalf("config keys must be accepted in any order; got:\n%s", out)
	}
	if !strings.Contains(out, `a := if steps.prep.status == "success" { automationFetchA() }`) {
		t.Fatalf("gated branch must keep its if wrapper; got:\n%s", out)
	}
}

// Nested parallel falls out of the recursive branch translation and the
// executor's recursive branch dispatch -- it is intentionally ALLOWED.
func TestNormaliseAutomationSource_ParallelStep_NestedParallel(t *testing.T) {
	src := `automation gather {
  step layer0 {
    parallel {
      branches: [
        step inner {
          parallel {
            branches: [
              step x { automation fetchX { } },
              step y { automation fetchY { } }
            ]
          }
        },
        step z { automation fetchZ { } }
      ]
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("nested parallel must rewrite: %v", err)
	}
	if !strings.Contains(out, `inner := parallel { wait: "all", failFast: false, branches: [ x := automationFetchX() y := automationFetchY() ] }`) {
		t.Fatalf("nested parallel branch must rewrite recursively; got:\n%s", out)
	}
}

// A gated parallel layer -- `if <cond> { parallel { ... } }` -- is what the
// headline synthesizer emits for a layer-k>0 fan-out.
func TestNormaliseAutomationSource_ParallelStep_GatedParallel(t *testing.T) {
	src := `automation gather {
  step prep {
    automation prep { }
  }
  step layer1 {
    if steps.prep.status == "success" {
      parallel {
        branches: [
          step a { automation fetchA { } },
          step b { automation fetchB { } }
        ]
      }
    }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `layer1 := if steps.prep.status == "success" { parallel { wait: "all", failFast: false, branches: [`) {
		t.Fatalf("gated parallel must rewrite to `if cond { parallel { ... } }`; got:\n%s", out)
	}
}

func TestNormaliseAutomationSource_ParallelStep_Errors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing branches",
			body:    `parallel { wait: "all" }`,
			wantErr: "a `branches: [ step <name> { ... }, ... ]` list is required",
		},
		{
			name:    "empty branches",
			body:    `parallel { branches: [ ] }`,
			wantErr: "branches must not be empty",
		},
		{
			name:    "bad wait value",
			body:    `parallel { wait: "sometimes", branches: [ step a { automation x { } } ] }`,
			wantErr: `invalid wait value "sometimes"`,
		},
		{
			name:    "unquoted wait value",
			body:    `parallel { wait: all, branches: [ step a { automation x { } } ] }`,
			wantErr: "expected a quoted string",
		},
		{
			name:    "bad failFast value",
			body:    `parallel { failFast: maybe, branches: [ step a { automation x { } } ] }`,
			wantErr: "failFast must be true or false",
		},
		{
			name:    "duplicate branch ids",
			body:    `parallel { branches: [ step a { automation x { } }, step a { automation y { } } ] }`,
			wantErr: `duplicate branch id "a"`,
		},
		{
			name:    "unknown key",
			body:    `parallel { retries: 3, branches: [ step a { automation x { } } ] }`,
			wantErr: `unknown key "retries"`,
		},
		{
			name:    "non-step branch entry",
			body:    `parallel { branches: [ automation x { } ] }`,
			wantErr: "each branches entry must be a `step <name> { ... }` block",
		},
		{
			name:    "empty branch body",
			body:    `parallel { branches: [ step a { } ] }`,
			wantErr: `branch "a": body is empty`,
		},
		{
			name:    "missing block",
			body:    `parallel`,
			wantErr: "expected `{` after `parallel`",
		},
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
