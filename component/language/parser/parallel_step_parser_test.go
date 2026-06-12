package parser

import (
	"strings"
	"testing"
)

// memql#1368 -- the procedural parser accepts the parallel step assignment
// the struct-form rewriter emits:
//
//	layer0 := parallel { wait: "all", failFast: true, branches: [ a := callA() b := callB() ] }
//
// producing a StepDef{Type: StepTypeParallel, Config: *ParallelStepConfig}.

func parseAutomationSteps_t(t *testing.T, src string) []StepDef {
	t.Helper()
	lexer := NewLexer(src)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("lexer error: %v", err)
	}
	ast, err := NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("parser error: %v", err)
	}
	file, ok := ast.(*File)
	if !ok || len(file.Definitions) != 1 {
		t.Fatalf("expected one definition, got %T", ast)
	}
	fn, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("expected *FunctionDef, got %T", file.Definitions[0])
	}
	auto, ok := fn.Body.(*AutomationDef)
	if !ok {
		t.Fatalf("expected *AutomationDef body, got %T", fn.Body)
	}
	return auto.Steps
}

func parseAutomationErr_t(t *testing.T, src string) error {
	t.Helper()
	lexer := NewLexer(src)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("lexer error: %v", err)
	}
	_, err = NewParser(tokens).Parse()
	return err
}

func TestParser_ParallelStep(t *testing.T) {
	src := `
func (Automation) gather(_ any) {
  layer0 := parallel { wait: "any", failFast: true, branches: [ a := automationFetchA({ }) b := automationFetchB({ }) ] }
}`
	steps := parseAutomationSteps_t(t, src)
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
	step := steps[0]
	if step.ID != "layer0" || step.Type != StepTypeParallel {
		t.Fatalf("expected layer0/parallel, got %s/%s", step.ID, step.Type)
	}
	cfg, ok := step.Config.(*ParallelStepConfig)
	if !ok {
		t.Fatalf("expected *ParallelStepConfig, got %T", step.Config)
	}
	if cfg.Wait != "any" || !cfg.FailFast {
		t.Errorf("expected wait=any failFast=true, got wait=%q failFast=%v", cfg.Wait, cfg.FailFast)
	}
	if len(cfg.Branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(cfg.Branches))
	}
	for i, want := range []string{"a", "b"} {
		br := cfg.Branches[i]
		if br.ID != want || br.Type != StepTypeFunction {
			t.Errorf("branch %d: expected %s/function, got %s/%s", i, want, br.ID, br.Type)
		}
	}
	if fn, ok := cfg.Branches[0].Config.(*FunctionStepConfig); !ok || fn.Name != "automationFetchA" {
		t.Errorf("branch a must invoke automationFetchA, got %+v", cfg.Branches[0].Config)
	}
}

func TestParser_ParallelStep_Defaults(t *testing.T) {
	src := `
func (Automation) gather(_ any) {
  layer0 := parallel { branches: [ a := automationFetchA({ }) ] }
}`
	steps := parseAutomationSteps_t(t, src)
	cfg := steps[0].Config.(*ParallelStepConfig)
	if cfg.Wait != "all" || cfg.FailFast {
		t.Errorf("defaults must be wait=all failFast=false, got wait=%q failFast=%v", cfg.Wait, cfg.FailFast)
	}
}

// `step := if cond { parallel { ... } }` -- the gated fan-out layer shape the
// headline synthesizer emits. The condition lands on StepDef.Condition.
func TestParser_ParallelStep_Gated(t *testing.T) {
	src := `
func (Automation) gather(_ any) {
  layer1 := if steps.prep.status == "success" { parallel { branches: [ a := automationFetchA({ }) b := automationFetchB({ }) ] } }
}`
	steps := parseAutomationSteps_t(t, src)
	step := steps[0]
	if step.Type != StepTypeParallel {
		t.Fatalf("expected parallel step, got %s", step.Type)
	}
	if !strings.Contains(step.Condition, "steps.prep.status") {
		t.Errorf("the if condition must be stamped on the StepDef, got %q", step.Condition)
	}
	cfg := step.Config.(*ParallelStepConfig)
	if len(cfg.Branches) != 2 {
		t.Errorf("expected 2 branches, got %d", len(cfg.Branches))
	}
}

// Per-branch gating (`a := if cond { call }`) inside the branches list.
func TestParser_ParallelStep_GatedBranch(t *testing.T) {
	src := `
func (Automation) gather(_ any) {
  layer0 := parallel { branches: [ a := if steps.prep.status == "success" { automationFetchA({ }) } b := automationFetchB({ }) ] }
}`
	steps := parseAutomationSteps_t(t, src)
	cfg := steps[0].Config.(*ParallelStepConfig)
	if len(cfg.Branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(cfg.Branches))
	}
	if cfg.Branches[0].Condition == "" {
		t.Errorf("gated branch must carry its condition")
	}
	if cfg.Branches[1].Condition != "" {
		t.Errorf("ungated branch must carry no condition, got %q", cfg.Branches[1].Condition)
	}
}

// Nested parallel branches parse recursively (allowed by design; the
// executor's ParallelExecutor dispatches branch types through the registry,
// which includes "parallel" itself).
func TestParser_ParallelStep_Nested(t *testing.T) {
	src := `
func (Automation) gather(_ any) {
  layer0 := parallel { branches: [ inner := parallel { branches: [ x := automationFetchX({ }) ] } z := automationFetchZ({ }) ] }
}`
	steps := parseAutomationSteps_t(t, src)
	cfg := steps[0].Config.(*ParallelStepConfig)
	if len(cfg.Branches) != 2 {
		t.Fatalf("expected 2 branches, got %d", len(cfg.Branches))
	}
	if cfg.Branches[0].Type != StepTypeParallel {
		t.Fatalf("inner branch must be a parallel step, got %s", cfg.Branches[0].Type)
	}
	innerCfg := cfg.Branches[0].Config.(*ParallelStepConfig)
	if len(innerCfg.Branches) != 1 || innerCfg.Branches[0].ID != "x" {
		t.Errorf("nested parallel must keep its branch, got %+v", innerCfg.Branches)
	}
}

func TestParser_ParallelStep_Diagnostics(t *testing.T) {
	cases := []struct {
		name    string
		stmt    string
		wantErr string
	}{
		{
			name:    "empty branches",
			stmt:    `layer0 := parallel { branches: [ ] }`,
			wantErr: "non-empty branches list",
		},
		{
			name:    "missing branches",
			stmt:    `layer0 := parallel { wait: "all" }`,
			wantErr: "non-empty branches list",
		},
		{
			name:    "bad wait value",
			stmt:    `layer0 := parallel { wait: "sometimes", branches: [ a := automationFetchA({ }) ] }`,
			wantErr: `invalid wait value "sometimes"`,
		},
		{
			name:    "duplicate branch ids",
			stmt:    `layer0 := parallel { branches: [ a := automationFetchA({ }) a := automationFetchB({ }) ] }`,
			wantErr: `duplicate branch id "a"`,
		},
		{
			name:    "unknown key",
			stmt:    `layer0 := parallel { retries: 3, branches: [ a := automationFetchA({ }) ] }`,
			wantErr: `unknown key "retries"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "func (Automation) gather(_ any) {\n  " + tc.stmt + "\n}"
			err := parseAutomationErr_t(t, src)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
