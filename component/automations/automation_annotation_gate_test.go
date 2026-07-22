package automations

import (
	"strings"
	"testing"
)

// TestCompileMemQL_AnnotationGate proves the #2712 wiring: compileMemQL runs
// the annotation allow-list + retired-name gate, so an unknown / dead /
// retired annotation on an automation is load-rejected (before compilation)
// instead of silently dropped. The gate fires ahead of body compilation, so
// the reject cases need no resolvable body.
func TestCompileMemQL_AnnotationGate(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	reject := []struct {
		name   string
		src    string
		expect string
	}{
		{"unknown", "@triggr(event=\"x\")\nautomation p {\n  step s { logic l {} }\n}\n", "triggr"},
		{"dead-retry", "@retry(count=3)\nautomation p {\n  step s { logic l {} }\n}\n", "retry"},
		{"dead-async", "@async\nautomation p {\n  step s { logic l {} }\n}\n", "async"},
		{"retired-role", "@role(\"admin\")\nautomation p {\n  step s { logic l {} }\n}\n", "#2709"},
		{"retired-internal", "@internal\nautomation p {\n  step s { logic l {} }\n}\n", "#2708"},
		{"retired-permission", "@permission(\"x\")\nautomation p {\n  step s { logic l {} }\n}\n", "#2713"},
	}
	for _, tc := range reject {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loader.compileMemQL(tc.src, "test:"+tc.name)
			if err == nil {
				t.Fatalf("expected @%s automation to be rejected by the gate", tc.name)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("rejection missing %q, got: %v", tc.expect, err)
			}
		})
	}
}
