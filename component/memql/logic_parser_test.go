package memql

import (
	"strings"
	"testing"
)

// TestParseLogicMemQL_GoldenPath locks the canonical struct-form
// logic syntax: args block + body block ending in `return <expr>`.
func TestParseLogicMemQL_GoldenPath(t *testing.T) {
	src := `@enabled
@useBuiltin(ensureDailySpaceForUser)
@description("On user creation, ensure today's daily space exists.")
logic logicProvisionDailySpaceOnUserCreate {
  args {
    event object @required
  }
  body {
    return ensureDailySpaceForUser({ userId: args.event.payload.id })
  }
}`

	fn, err := parseLogicMemQL("logicProvisionDailySpaceOnUserCreate", "test.memql", src, nil)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if fn == nil {
		t.Fatal("expected non-nil *Function")
	}
	if fn.Name != "logicProvisionDailySpaceOnUserCreate" {
		t.Errorf("Name = %q, want logicProvisionDailySpaceOnUserCreate", fn.Name)
	}
}

// TestParseLogicMemQL_RejectsUnknownAnnotation locks the
// per-construct annotation allow-list. Logic blocks have a narrow
// surface (no @internal, no @timeout, etc.) so the rejection set
// is wide.
func TestParseLogicMemQL_RejectsUnknownAnnotation(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "internal-not-allowed-on-logic",
			src: `@internal
logic logicFoo { body { return 1 } }`,
		},
		{
			name: "rateLimit-not-allowed-on-logic",
			src: `@rateLimit(requests=10, per="1h")
logic logicFoo { body { return 1 } }`,
		},
		{
			name: "completely-unknown",
			src: `@bogusKnob
logic logicFoo { body { return 1 } }`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLogicMemQL("logicFoo", "test.memql", tc.src, nil)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "unknown") && !strings.Contains(err.Error(), "annotation") {
				t.Errorf("error should describe an unknown annotation, got %v", err)
			}
		})
	}
}

// TestParseLogicMemQL_AcceptsAllAllowedAnnotations sweeps the
// allow-list.
func TestParseLogicMemQL_AcceptsAllAllowedAnnotations(t *testing.T) {
	cases := []string{
		`@description("d")`,
		`@enabled`,
		`@disabled`,
		`@deprecated("hint")`,
		`@useConcept(foo)`,
		`@useShape(s)`,
		`@useSpec(x)`,
		`@useTrait(t)`,
		`@useQuery(q)`,
		`@useMutation(m)`,
		`@useLogic(l)`,
		`@useBuiltin(b)`,
		`@usePrompt(p)`,
		`@useTool(tn)`,
	}
	for _, ann := range cases {
		t.Run(ann, func(t *testing.T) {
			src := ann + `
logic logicFoo { body { return 1 } }`
			_, err := parseLogicMemQL("logicFoo", "test.memql", src, nil)
			if err != nil && strings.Contains(err.Error(), "unknown annotation") {
				t.Errorf("allow-listed annotation %q reported as unknown: %v", ann, err)
			}
		})
	}
}
