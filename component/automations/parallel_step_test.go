package automations

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// memql#1368 -- the `parallel` step in the struct-form automation grammar.
//
// These tests lock the full chain: authored struct form -> rewriter ->
// procedural parser -> compiler -> Automation.Steps[].Parallel IR
// (ParallelStepConfig{Branches, Wait, FailFast}), which the executor's
// ParallelExecutor already runs (branch ids surface as <parent>.<branch>).

const parallelAuthoredSrc = `@description("Gather two reports concurrently, then merge.")
automation gather {
  step layer0 {
    parallel {
      wait: "all"
      failFast: true
      branches: [
        step sales   { automation fetchSales { } },
        step support { automation fetchSupport { } }
      ]
    }
  }
  step merge {
    if steps.layer0.status == "success" {
      automation mergeReports { }
    }
  }
}`

func newTestLoader() *Loader {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return NewLoader(LoaderOptions{Logger: logger})
}

func TestCompileSource_ParallelStep(t *testing.T) {
	auto, err := newTestLoader().CompileSource(parallelAuthoredSrc, "test:parallel-step")
	if err != nil {
		t.Fatalf("parallel automation must compile: %v", err)
	}
	if len(auto.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(auto.Steps))
	}

	layer := auto.Steps[0]
	if layer.ID != "layer0" || layer.Type != StepTypeParallel {
		t.Fatalf("step 0 must be the parallel layer, got %s/%s", layer.ID, layer.Type)
	}
	if layer.Parallel == nil {
		t.Fatal("parallel step must carry the Parallel config")
	}
	if layer.Parallel.Wait != "all" || !layer.Parallel.FailFast {
		t.Errorf("want wait=all failFast=true, got wait=%q failFast=%v",
			layer.Parallel.Wait, layer.Parallel.FailFast)
	}
	if len(layer.Parallel.Branches) != 2 {
		t.Fatalf("want 2 branches, got %d", len(layer.Parallel.Branches))
	}
	// `automation <name> { }` branch bodies compile to sub-automation
	// dispatch steps (StepTypeAutomation), exactly like top-level steps.
	wantBranches := []struct{ id, sub string }{
		{"sales", "fetchSales"},
		{"support", "fetchSupport"},
	}
	for i, want := range wantBranches {
		br := layer.Parallel.Branches[i]
		if br == nil || br.ID != want.id {
			t.Fatalf("branch %d: want id %q, got %+v", i, want.id, br)
		}
		if br.Type != StepTypeAutomation || br.Automation == nil || br.Automation.Name != want.sub {
			t.Errorf("branch %q must dispatch sub-automation %s, got type=%s automation=%+v",
				want.id, want.sub, br.Type, br.Automation)
		}
	}

	// The downstream merge step is gated on the parallel layer's status.
	merge := auto.Steps[1]
	if merge.ID != "merge" || merge.Condition == "" {
		t.Fatalf("merge step must be gated; got %+v", merge)
	}
	if !strings.Contains(merge.Condition, "steps.layer0.status") {
		t.Errorf("merge gate must reference the parallel layer, got %q", merge.Condition)
	}
}

func TestCompileSource_ParallelStep_Defaults(t *testing.T) {
	src := `@description("Defaults probe.")
automation gather {
  step layer0 {
    parallel {
      branches: [
        step a { automation fetchA { } },
        step b { automation fetchB { } }
      ]
    }
  }
}`
	auto, err := newTestLoader().CompileSource(src, "test:parallel-defaults")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	cfg := auto.Steps[0].Parallel
	if cfg == nil {
		t.Fatal("parallel config missing")
	}
	if cfg.Wait != "all" || cfg.FailFast {
		t.Errorf("defaults must be wait=all failFast=false, got wait=%q failFast=%v", cfg.Wait, cfg.FailFast)
	}
}

// A gated parallel layer (`if cond { parallel { ... } }`) keeps the
// condition on the parallel step itself -- the synthesizer's layer-k>0 shape.
func TestCompileSource_ParallelStep_GatedLayer(t *testing.T) {
	src := `@description("Gated fan-out probe.")
automation gather {
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
	auto, err := newTestLoader().CompileSource(src, "test:parallel-gated")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(auto.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(auto.Steps))
	}
	layer := auto.Steps[1]
	if layer.Type != StepTypeParallel || layer.Parallel == nil {
		t.Fatalf("step 1 must be the gated parallel layer, got %+v", layer)
	}
	if !strings.Contains(layer.Condition, "steps.prep.status") {
		t.Errorf("gate condition must ride onto the parallel step, got %q", layer.Condition)
	}
}

// Per-branch gating inside the parallel: the executor's ParallelExecutor
// evaluates branch Condition before dispatch.
func TestCompileSource_ParallelStep_GatedBranch(t *testing.T) {
	src := `@description("Branch gate probe.")
automation gather {
  step layer0 {
    parallel {
      branches: [
        step a { if steps.prep.status == "success" { automation fetchA { } } },
        step b { automation fetchB { } }
      ]
    }
  }
}`
	auto, err := newTestLoader().CompileSource(src, "test:parallel-branch-gate")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	branches := auto.Steps[0].Parallel.Branches
	if len(branches) != 2 {
		t.Fatalf("want 2 branches, got %d", len(branches))
	}
	if branches[0].Condition == "" {
		t.Errorf("gated branch must keep its condition")
	}
	if branches[1].Condition != "" {
		t.Errorf("ungated branch must have no condition, got %q", branches[1].Condition)
	}
}

func TestCompileSource_ParallelStep_Diagnostics(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing branches",
			body:    `parallel { wait: "all" }`,
			wantErr: "branches",
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
			name:    "duplicate branch ids",
			body:    `parallel { branches: [ step a { automation x { } }, step a { automation y { } } ] }`,
			wantErr: `duplicate branch id "a"`,
		},
		{
			name:    "unknown key",
			body:    `parallel { retries: 3, branches: [ step a { automation x { } } ] }`,
			wantErr: `unknown key "retries"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "@description(\"bad\")\nautomation bad {\n  step s {\n    " + tc.body + "\n  }\n}"
			_, err := newTestLoader().CompileSource(src, "test:parallel-bad")
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
