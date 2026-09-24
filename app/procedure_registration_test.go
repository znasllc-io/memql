package app

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// Test the binary's registration set, not the integration package's own init.
// The shipped automation runs on every node that receives a work-run event.
func TestProcedureExecutorsReachTheApplication(t *testing.T) {
	for _, registration := range memql.RegisteredPlugins() {
		if registration.Name != "procedure" {
			continue
		}
		provider, err := registration.Factory(memql.PluginContext{Engine: &memql.MemQLEngine{}})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"learnFromRun": "runId", "mineCorpus": "ownerUserId"}
		for _, capability := range provider.Capabilities() {
			field, expected := want[capability.Name]
			if !expected {
				continue
			}
			// Missing arguments reach the real handler's validation without
			// a database or a model call.
			_, err := capability.Handler(context.Background(), map[string]any{}, 0)
			if err == nil || !strings.Contains(err.Error(), field) {
				t.Fatalf("%s did not reach argument validation: %v", capability.Name, err)
			}
			delete(want, capability.Name)
		}
		if len(want) != 0 {
			t.Fatalf("shipped procedure executors missing: %v", want)
		}
		return
	}
	t.Fatal("procedure DSL ships in this binary, but its integration is not registered")
}
