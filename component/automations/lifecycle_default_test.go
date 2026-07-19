package automations

import "testing"

// Lifecycle ruling (#2604): absent = enabled, @enabled = explicit no-op,
// @disabled = the only off-switch -- uniformly across construct kinds.
// Automations were the one kind that defaulted to disabled (the #360
// default-true flip was scoped to FunctionDef because every automation then
// carried @enabled explicitly). This test pins the ruling at the loader
// level, through the real pipeline (rewriter -> parser -> compiler -> JSON
// round-trip), because the compiler always stamps output["enabled"] from
// AutomationDef.Enabled -- the *bool nil->true fallback in IsEnabled never
// applies to DSL-authored automations, only to hand-written JSON.
func lifecycleAutomationSource(annotation string) string {
	prefix := ""
	if annotation != "" {
		prefix = annotation + "\n"
	}
	return prefix + `@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")
@description("lifecycle default probe")
automation lifecycleProbe {
  step gate {
    logic requireForwardDeploy { environment: "staging" }
  }
}`
}

func TestCompileMemQL_LifecycleDefaults(t *testing.T) {
	cases := []struct {
		name        string
		annotation  string
		wantEnabled bool
	}{
		{"absent", "", true}, // the ruling; compiled to disabled pre-#2604
		{"explicit-enabled", "@enabled", true},
		{"explicit-disabled", "@disabled", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loader := NewLoader(LoaderOptions{})
			auto, err := loader.compileMemQL(lifecycleAutomationSource(tc.annotation), "test:lifecycleProbe")
			if err != nil {
				t.Fatalf("compileMemQL: %v", err)
			}
			if auto.Enabled == nil {
				t.Fatal("Automation.Enabled is nil; the compiler must stamp an explicit value for DSL automations")
			}
			if got := auto.IsEnabled(); got != tc.wantEnabled {
				t.Errorf("annotation %q: IsEnabled() = %v, want %v", tc.annotation, got, tc.wantEnabled)
			}
		})
	}
}
