package compiler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// TestUnaryOperatorsCompile verifies that == nil and != nil compile without
// "<nil>" appended. This is a regression test for a bug where unary operators
// with nil values were formatted as "payload.field!=nil<nil>" instead of
// "payload.field!=nil".
//
// Two grammars carry the comparison since the edition-2026 flip. The internal
// query form -- the string an SDK sends to Execute, which keeps its grammar --
// is serialised by expressionToString, so its cases parse that form directly;
// an authored filter or condition is edition 2026, compiled through
// CompileSource.
func TestUnaryOperatorsCompile(t *testing.T) {
	c := NewDefault()
	for _, tc := range []struct {
		name, internal, want string
	}{
		{"== nil in the internal query form", `concept==v1:test;payload.field==nil`, `concept=="v1:test";payload.field==nil`},
		{"!= nil in the internal query form", `concept==v1:test;payload.field!=nil`, `concept=="v1:test";payload.field!=nil`},
		{"several != nil in the internal query form", `concept==v1:test;payload.field1!=nil;payload.field2!=nil`, `concept=="v1:test";payload.field1!=nil;payload.field2!=nil`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := parser.ParseExpression(tc.internal)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.internal, err)
			}
			if got := c.expressionToString(expr); got != tc.want {
				t.Errorf("expressionToString(%q) = %q, want %q", tc.internal, got, tc.want)
			}
		})
	}

	t.Run("== nil in an authored filter", func(t *testing.T) {
		result, err := CompileSource(`
query test testMissing {
  filter row => row.field == nil
}`)
		if err != nil {
			t.Fatalf("compile error: %v", err)
		}
		if len(result.Functions) != 1 {
			t.Fatalf("expected one compiled query, got %d", len(result.Functions))
		}
		query := result.Functions[0].Query
		if !strings.Contains(query, "row.field == nil") || strings.Contains(query, "<nil>") {
			t.Errorf("compiled query = %q, want it to carry `row.field == nil` and no <nil>", query)
		}
	})

	t.Run("!= nil in an automation condition", func(t *testing.T) {
		result, err := CompileSource(`
@enabled
@schedule(cron="0 */15 * * * *")
automation testAutomation {
  step flagged {
    query openFlags()
  }
  step escalate {
    if flagged.first().slaDeadline != nil {
      logic escalateFlag(flag: flagged.first())
    }
  }
}`)
		if err != nil {
			t.Fatalf("compile error: %v", err)
		}
		if len(result.Automations) != 1 {
			t.Fatalf("expected one compiled automation, got %d", len(result.Automations))
		}
		b, err := json.Marshal(result.Automations[0].JSON)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "<nil>") {
			t.Errorf("compiled automation carries <nil>: %s", b)
		}
		found := false
		for _, step := range result.Automations[0].JSON["steps"].([]map[string]any) {
			if step["id"] == "escalate" {
				found = true
				if step["condition"] != "flagged.first().slaDeadline != nil" {
					t.Errorf("escalate condition = %#v, want the canonical v1 source", step["condition"])
				}
			}
		}
		if !found {
			t.Errorf("no escalate step in %s", b)
		}
	})
}

// TestUnaryOperatorWithOtherConditions tests that unary operators serialise
// correctly beside other comparison operators in one internal-form query.
func TestUnaryOperatorWithOtherConditions(t *testing.T) {
	expr, err := parser.ParseExpression(`concept==v1:test;payload.status=="active";payload.optionalField!=nil;payload.count>5`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	query := NewDefault().expressionToString(expr)

	// Should contain all conditions
	expectedParts := []string{
		`concept=="v1:test"`,
		`payload.status=="active"`,
		`payload.optionalField!=nil`,
		`payload.count>5`,
	}

	for _, part := range expectedParts {
		if !strings.Contains(query, part) {
			t.Errorf("query missing expected part %q\nfull query: %s", part, query)
		}
	}

	// Should NOT contain <nil>
	if strings.Contains(query, "<nil>") {
		t.Errorf("query should not contain '<nil>', got: %s", query)
	}
}
