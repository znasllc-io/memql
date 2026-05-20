package compiler

import (
	"strings"
	"testing"
)

// TestUnaryOperatorsCompile verifies that == nil and != nil operators
// compile correctly without appending "<nil>" to the query string.
// This is a regression test for a bug where unary operators with nil values
// were formatted as "payload.field!=nil<nil>" instead of "payload.field!=nil".
func TestUnaryOperatorsCompile(t *testing.T) {
	tests := []struct {
		name             string
		source           string
		expectedQuery    string
		shouldContain    string
		shouldNotContain string
	}{
		{
			name: "== nil operator in function query",
			source: `
func (Query) testMissing() {
	concept==v1:test;payload.field==nil
}`,
			expectedQuery:    `concept=="v1:test";payload.field==nil`,
			shouldNotContain: "<nil>",
		},
		{
			name: "!= nil operator in function query",
			source: `
func (Query) testNotMissing() {
	concept==v1:test;payload.field!=nil
}`,
			expectedQuery:    `concept=="v1:test";payload.field!=nil`,
			shouldNotContain: "<nil>",
		},
		{
			name: "multiple fields with != nil",
			source: `
func (Query) testMultiple() {
	concept==v1:test;payload.field1!=nil;payload.field2!=nil
}`,
			shouldContain:    "payload.field1!=nil",
			shouldNotContain: "<nil>",
		},
		{
			name: "automation with != nil in query step",
			source: `
@enabled
@schedule(cron="0 */15 * * * *")
func (Automation) testAutomation(_ any) {
  step1 := query {
    concept==v1:hr:flag;
    payload.status=="open";
    payload.slaDeadline!=nil
  }

  return step1
}`,
			shouldContain:    `payload.slaDeadline!=nil`,
			shouldNotContain: "<nil>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := CompileSource(tt.source)
			if err != nil {
				t.Fatalf("compile error: %v", err)
			}

			// For queries, check the query string
			if len(result.Functions) > 0 {
				query := result.Functions[0].Query
				if tt.expectedQuery != "" && query != tt.expectedQuery {
					t.Errorf("expected query:\n%s\ngot:\n%s", tt.expectedQuery, query)
				}
				if tt.shouldContain != "" && !strings.Contains(query, tt.shouldContain) {
					t.Errorf("query should contain %q, got: %s", tt.shouldContain, query)
				}
				if tt.shouldNotContain != "" && strings.Contains(query, tt.shouldNotContain) {
					t.Errorf("query should NOT contain %q, got: %s", tt.shouldNotContain, query)
				}
			}

			// For automations, check the step queries
			if len(result.Automations) > 0 {
				automation := result.Automations[0]

				// Get steps and check queries
				if steps, ok := automation.JSON["steps"].([]any); ok {
					for _, step := range steps {
						stepMap, _ := step.(map[string]any)
						if queryConfig, ok := stepMap["query"].(map[string]any); ok {
							if query, ok := queryConfig["query"].(string); ok {
								if tt.shouldContain != "" && !strings.Contains(query, tt.shouldContain) {
									t.Errorf("step query should contain %q, got: %s", tt.shouldContain, query)
								}
								if tt.shouldNotContain != "" && strings.Contains(query, tt.shouldNotContain) {
									t.Errorf("step query should NOT contain %q, got: %s", tt.shouldNotContain, query)
								}
							}
						}
					}
				}
			}
		})
	}
}

// TestUnaryOperatorWithOtherConditions tests that unary operators work correctly
// when combined with other comparison operators in the same query.
func TestUnaryOperatorWithOtherConditions(t *testing.T) {
	source := `
func (Query) testCombined() {
	concept==v1:test;payload.status=="active";payload.optionalField!=nil;payload.count>5
}
`
	result, err := CompileSource(source)
	if err != nil {
		t.Fatalf("compile error: %v", err)
	}

	if len(result.Functions) == 0 {
		t.Fatal("expected function to compile")
	}

	query := result.Functions[0].Query

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
