package parser

import "testing"

// #1758 -- the action-library replay step parses to a
// StepDef{Type: StepTypeAction, Config: *ActionStepConfig}.
func TestParser_ActionStep(t *testing.T) {
	src := `
func (Automation) scaffold(_ any) {
  writeCfg := action { ref: "act_writeFile@3", args: { path: "/work/config.yaml", content: "hi" }, surface: "workbench" }
}`
	steps := parseAutomationSteps_t(t, src)
	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}
	step := steps[0]
	if step.ID != "writeCfg" || step.Type != StepTypeAction {
		t.Fatalf("expected writeCfg/action, got %s/%s", step.ID, step.Type)
	}
	cfg, ok := step.Config.(*ActionStepConfig)
	if !ok {
		t.Fatalf("expected *ActionStepConfig, got %T", step.Config)
	}
	if cfg.Ref != "act_writeFile@3" {
		t.Fatalf("expected ref act_writeFile@3, got %q", cfg.Ref)
	}
	if cfg.Surface != "workbench" {
		t.Fatalf("expected surface workbench, got %q", cfg.Surface)
	}
	if _, ok := cfg.Args["path"]; !ok {
		t.Fatalf("expected path arg present, got %+v", cfg.Args)
	}
}

func TestParser_ActionStep_FloatingRef(t *testing.T) {
	src := `
func (Automation) fmt(_ any) {
  go := action { ref: "act_fmt" }
}`
	steps := parseAutomationSteps_t(t, src)
	cfg, ok := steps[0].Config.(*ActionStepConfig)
	if !ok || cfg.Ref != "act_fmt" {
		t.Fatalf("expected floating ref act_fmt, got %+v", steps[0].Config)
	}
}

func TestParser_ActionStep_RequiresRef(t *testing.T) {
	src := `
func (Automation) bad(_ any) {
  oops := action { args: { x: 1 } }
}`
	if err := parseAutomationErr_t(t, src); err == nil {
		t.Fatalf("expected error for an action step with no ref")
	}
}
