package automations

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// memql#1368 -- the `parallel` statement.
//
// These tests lock the full chain: an authored `parallel` statement -> the
// statement parser -> the statement compiler -> Automation.Steps[].Parallel IR
// (ParallelStepConfig{Branches, Wait, FailFast}, each branch a block step),
// which the ParallelExecutor runs.

const parallelAuthoredSrc = `@description("Gather two reports concurrently, then merge.")
@trigger(event="system.startup")
automation gather {
  parallel {
    branch sales {
      automation fetchSales()
    }
    branch support {
      automation fetchSupport()
    }
  }
  automation mergeReports()
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
	if layer.Type != StepTypeParallel || layer.Parallel == nil {
		t.Fatalf("step 0 must be the parallel, got %s/%s", layer.ID, layer.Type)
	}
	if layer.Parallel.Wait != "all" || !layer.Parallel.FailFast {
		t.Errorf("want wait=all failFast=true, got wait=%q failFast=%v",
			layer.Parallel.Wait, layer.Parallel.FailFast)
	}
	if len(layer.Parallel.Branches) != 2 {
		t.Fatalf("want 2 branches, got %d", len(layer.Parallel.Branches))
	}
	// Each branch is a block step named by its label, holding its
	// statements: here one sub-automation call.
	wantBranches := []struct{ id, sub string }{
		{"sales", "fetchSales"},
		{"support", "fetchSupport"},
	}
	for i, want := range wantBranches {
		br := layer.Parallel.Branches[i]
		if br == nil || br.ID != want.id || br.Type != StepTypeBlock || br.Block == nil {
			t.Fatalf("branch %d: want block %q, got %+v", i, want.id, br)
		}
		if len(br.Block.Steps) != 1 {
			t.Fatalf("branch %q: want one statement, got %d", want.id, len(br.Block.Steps))
		}
		if call := br.Block.Steps[0]; call.Type != StepTypeAutomation || call.Automation == nil || call.Automation.Name != want.sub {
			t.Errorf("branch %q must dispatch sub-automation %s, got type=%s automation=%+v",
				want.id, want.sub, call.Type, call.Automation)
		}
	}

	// The merge runs after the parallel, ungated: a failed branch stops the
	// other branches and fails the parallel, which ends the run before it.
	merge := auto.Steps[1]
	if merge.Automation == nil || merge.Automation.Name != "mergeReports" || merge.Condition != "" {
		t.Fatalf("the merge must follow the parallel, ungated; got %+v", merge)
	}
}

// A statement parallel waits for every branch and stops the others when one
// fails: `wait any` and `on error continue` are the two ways to say otherwise.
func TestCompileSource_ParallelStep_Defaults(t *testing.T) {
	src := `@description("Defaults probe.")
@trigger(event="system.startup")
automation gather {
  parallel {
    branch a {
      automation fetchA()
    }
    branch b {
      automation fetchB()
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
	if cfg.Wait != "all" || !cfg.FailFast {
		t.Errorf("defaults must be wait=all failFast=true, got wait=%q failFast=%v", cfg.Wait, cfg.FailFast)
	}
}

// A gated parallel (`if cond { parallel { ... } }`) keeps the condition on
// the parallel step itself.
func TestCompileSource_ParallelStep_GatedLayer(t *testing.T) {
	src := `@description("Gated fan-out probe.")
@trigger(event="system.startup")
automation gather {
  prep := automation prep()
  if prep.ready == true {
    parallel {
      branch a {
        automation fetchA()
      }
      branch b {
        automation fetchB()
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
		t.Fatalf("step 1 must be the gated parallel, got %+v", layer)
	}
	if !strings.Contains(layer.Condition, "prep.ready") {
		t.Errorf("the gate's condition must ride onto the parallel step, got %q", layer.Condition)
	}
}

// Per-branch gating inside the parallel: an `if` in a branch gates the
// statements of that branch alone.
func TestCompileSource_ParallelStep_GatedBranch(t *testing.T) {
	src := `@description("Branch gate probe.")
@trigger(event="system.startup")
automation gather {
  args {
    go any
  }
  parallel {
    branch a {
      if args.go == true {
        automation fetchA()
      }
    }
    branch b {
      automation fetchB()
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
	if steps := branches[0].Block.Steps; len(steps) != 1 || steps[0].Condition == "" {
		t.Errorf("the gated branch's statement must keep its condition, got %+v", steps)
	}
	if steps := branches[1].Block.Steps; len(steps) != 1 || steps[0].Condition != "" {
		t.Errorf("the ungated branch's statement must have no condition, got %+v", steps)
	}
}

func TestCompileSource_ParallelStep_Diagnostics(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "no branches",
			body:    "parallel {\n  }",
			wantErr: "a parallel has at least one branch",
		},
		{
			name:    "a branch without its keyword",
			body:    "parallel {\n    a {\n      automation x()\n    }\n  }",
			wantErr: "expected `branch <label> { }` in the parallel",
		},
		{
			name:    "duplicate branch labels",
			body:    "parallel {\n    branch a {\n      automation x()\n    }\n    branch a {\n      automation y()\n    }\n  }",
			wantErr: "the branch label `a` is used twice in this parallel",
		},
		{
			name:    "bad wait value",
			body:    "parallel {\n    branch a {\n      automation x()\n    }\n  } wait sometimes",
			wantErr: "`wait` takes `any`",
		},
		{
			name:    "the default wait written",
			body:    "parallel {\n    branch a {\n      automation x()\n    }\n  } wait all",
			wantErr: "`wait all` is the default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "@description(\"bad\")\n@trigger(event=\"system.startup\")\nautomation bad {\n  " + tc.body + "\n}"
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
