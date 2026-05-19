package memql

import (
	"strings"
	"testing"
)

// parseAutomationMemQL is the dedicated parser entry point that
// validates the per-construct annotation allow-list before
// delegating structural parsing to the shared function-conversion
// path. The structural parse for automations today flows through
// component/automations/loader.go, so the value tested here is the
// allow-list itself -- typos / wrong-construct annotations must
// surface as hard parse-time errors rather than silently sticking
// to FunctionDef.Attributes.

// TestParseAutomationMemQL_RejectsUnknownAnnotation locks the
// per-construct allow-list -- the value this parser adds.
func TestParseAutomationMemQL_RejectsUnknownAnnotation(t *testing.T) {
	src := `@trigger(event="system.startup")
@bogusKnob
automation autoFoo {
  step run { logic logicFoo { } }
}`

	_, err := parseAutomationMemQL("autoFoo", "test.memql", src, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bogusKnob") {
		t.Errorf("error should mention bogusKnob, got %v", err)
	}
}

// TestParseAutomationMemQL_AcceptsTriggerSurface verifies the
// trigger / schedule / async / filter annotations all pass the
// allow-list pass. Structural-parse errors downstream are
// acceptable -- the allow-list pass is what this test locks.
func TestParseAutomationMemQL_AcceptsTriggerSurface(t *testing.T) {
	cases := []string{
		`@trigger(event="system.startup")`,
		`@schedule("0 0 * * *")`,
		`@async`,
		`@filter("payload.foo == \"bar\"")`,
		`@useAutomation(other)`,
	}
	for _, ann := range cases {
		t.Run(ann, func(t *testing.T) {
			src := ann + `
automation autoFoo {
  step run { logic autoFoo { } }
}`
			_, err := parseAutomationMemQL("autoFoo", "test.memql", src, nil)
			if err != nil && strings.Contains(err.Error(), "unknown annotation") {
				t.Errorf("allow-listed annotation %q reported as unknown: %v", ann, err)
			}
		})
	}
}

// TestParseAutomationMemQL_RejectsQueryOnlyAnnotation locks the
// cross-construct separation: annotations that belong to other
// constructs must be rejected on automations.
func TestParseAutomationMemQL_RejectsQueryOnlyAnnotation(t *testing.T) {
	src := `@trigger(event="system.startup")
@cacheTTL("5m")
automation autoFoo {
  step run { logic autoFoo { } }
}`

	_, err := parseAutomationMemQL("autoFoo", "test.memql", src, nil)
	if err == nil {
		t.Fatal("expected error (cacheTTL is a query-only annotation), got nil")
	}
	if !strings.Contains(err.Error(), "cacheTTL") {
		t.Errorf("error should mention cacheTTL, got %v", err)
	}
}
