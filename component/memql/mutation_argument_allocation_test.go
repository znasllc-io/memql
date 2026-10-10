package memql

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Journaling a long-running workflow makes many mutation calls. Argument
// validation must depend on the called contract, not copy every loaded one.
func TestMutationArgumentValidationDoesNotScaleWithUnrelatedFunctions(t *testing.T) {
	measure := func(count int) float64 {
		registry := newFunctionRegistry()
		for n := 0; n < count; n++ {
			if err := registry.add(&Function{
				Name: fmt.Sprintf("mutation%d", n), FunctionKind: "mutation", Enabled: true,
				MutationTemplate: &FunctionMutationTemplate{},
				ArgsSchema:       &ArgsSchemaConfig{Fields: []*FunctionArgsField{{Name: "value", Type: "string"}}},
			}); err != nil {
				t.Fatal(err)
			}
		}
		engine := &MemQLEngine{}
		call := &FunctionCallExpression{Name: "mutation0", Args: map[string]any{}}
		return testing.AllocsPerRun(20, func() {
			_, err := engine.executeMutationFunctionCall(context.Background(), call, registry)
			if err == nil || !strings.Contains(err.Error(), "value") || !strings.Contains(err.Error(), "required") {
				t.Fatalf("required argument validation was bypassed: %v", err)
			}
		})
	}
	small, large := measure(1), measure(256)
	t.Logf("allocations per validation: one function %.0f, 256 functions %.0f", small, large)
	if large > small+8 {
		t.Fatalf("unrelated functions amplified argument-validation allocations: %.0f -> %.0f", small, large)
	}
}
