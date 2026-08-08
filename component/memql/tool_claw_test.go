package memql

import (
	"testing"
)

func TestLoadToolRegistryLoadsClawTools(t *testing.T) {
	t.Skip("legacy dsl/v1 tree retired; unified-tree coverage lives in component/memql/unified_*_test.go and test/dslconformance/embed_test.go.")
	reg, err := loadToolRegistry(nil)
	if err != nil {
		t.Fatalf("loadToolRegistry: %v", err)
	}

	clawTools := []string{
		"clawExecuteTask",
		"clawReadFile",
		"clawListFiles",
		"clawSearchCode",
	}

	for _, name := range clawTools {
		if !reg.Has(name) {
			t.Errorf("expected claw tool %q to be loaded", name)
			continue
		}

		tool, err := reg.Get(name)
		if err != nil {
			t.Errorf("get tool %q: %v", name, err)
			continue
		}

		if tool.Handler == nil {
			t.Errorf("tool %q has no handler", name)
			continue
		}
		if tool.Handler.Type != "webhook" {
			t.Errorf("tool %q handler type: expected 'webhook', got %q", name, tool.Handler.Type)
		}
		if tool.Handler.URL == "" {
			t.Errorf("tool %q handler URL is empty", name)
		}
		if tool.Handler.Method != "POST" {
			t.Errorf("tool %q handler method: expected 'POST', got %q", name, tool.Handler.Method)
		}
	}
}
