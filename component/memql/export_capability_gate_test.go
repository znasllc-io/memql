package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// export_capability_gate_test.go -- the capability on the account export's seven
// calls (account export, design record 2026-11-12, section H). The handlers are
// integrations/accounts' and are tested against a database there, person gate
// included; this file holds the wall in front of them, which is the engine's,
// and reads the annotation from the embedded tree rather than from the test.
// The shape follows account_inventory_gate_test.go.

func TestAccountExportCapabilityGateRequiresTheExportPart(t *testing.T) {
	installSeededCatalog(t)
	e := treeBuiltinEngine(t)
	if err := e.initBuiltinExecutorHandlers(); err != nil {
		t.Fatalf("initBuiltinExecutorHandlers: %v", err)
	}
	reached := map[string]int{}
	builtins := map[string]string{
		"accountExportReadiness": "integration.accounts.exportReadiness",
		"accountExportPrepare":   "integration.accounts.exportPrepare",
		"accountExportStart":     "integration.accounts.exportStart",
		"accountExports":         "integration.accounts.exports",
		"accountExportStatus":    "integration.accounts.exportStatus",
		"accountExportCancel":    "integration.accounts.exportCancel",
		"accountExportRemove":    "integration.accounts.exportRemove",
	}
	for _, executor := range builtins {
		executor := executor
		e.builtinExecutorHandlers[executor] = func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
			reached[executor]++
			return []memorynodes.MemoryNode{{ID: "accounts:answer", Concept: "integration:accounts:result"}}, nil
		}
	}

	for name, executor := range builtins {
		fn, err := e.functions.Get(name)
		if err != nil || fn == nil {
			t.Fatalf("%s is not in the loaded tree: %v", name, err)
		}
		if fn.Executor != executor {
			t.Fatalf("%s runs %q, want %q", name, fn.Executor, executor)
		}
		if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:accounts/export"}) {
			t.Fatalf("%s declares %+v, want execute on app:accounts/export", name, fn.RequiresCapability)
		}
		call := &BuiltinFunctionExpression{Name: name, Executor: executor, Args: map[string]any{"accountId": "acme", "exportId": "aex-1"}}

		for _, role := range []string{"admin", "user", "viewer"} {
			nodes, err := e.evaluateBuiltinFunctionExpression(asCaller(role), call, 0)
			if err == nil {
				t.Fatalf("%s answered a %s with %d rows", name, role, len(nodes))
			}
			if !strings.HasPrefix(err.Error(), CodeCapabilityNotHeld+": ") || !strings.Contains(err.Error(), "app:accounts/export") {
				t.Fatalf("%s refused a %s with %q, want the capability refusal naming app:accounts/export", name, role, err)
			}
		}
		if reached[executor] != 0 {
			t.Fatalf("%s reached its handler %d times for refused callers", name, reached[executor])
		}
		for _, role := range []string{"owner", "developer"} {
			if _, err := e.evaluateBuiltinFunctionExpression(asCaller(role), call, 0); err != nil {
				t.Fatalf("%s refused a %s, whom the seeds grant it: %v", name, role, err)
			}
		}
		if reached[executor] != 2 {
			t.Fatalf("%s reached its handler %d times, want once per admitted caller", name, reached[executor])
		}
	}
}
