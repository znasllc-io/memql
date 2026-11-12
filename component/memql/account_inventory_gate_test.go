package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// account_inventory_gate_test.go -- the capability on the Accounts app's two
// inventory reads (account row ownership, design record 2026-11-12, section H).
// The handlers are integrations/accounts' and are tested against a database
// there; this file holds the wall in front of them, which is the engine's, and
// reads the annotation from the embedded tree rather than from the test.

func TestAccountInventoryBuiltinsRequireTheInventoryPart(t *testing.T) {
	installSeededCatalog(t)
	e := treeBuiltinEngine(t)
	if err := e.initBuiltinExecutorHandlers(); err != nil {
		t.Fatalf("initBuiltinExecutorHandlers: %v", err)
	}
	// Stand-ins for integrations/accounts, which this package cannot import:
	// the gate runs before the handler, so a stand-in that answers is all the
	// admitted half needs.
	reached := map[string]int{}
	builtins := map[string]string{
		"accountInventory":        "integration.accounts.inventory",
		"accountAssignmentStatus": "integration.accounts.assignmentStatus",
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
		// THE CONTROL COMES FROM THE TREE: a builtin that lost its annotation
		// would admit everybody below and this test would call that a pass.
		if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:accounts/inventory"}) {
			t.Fatalf("%s declares %+v, want execute on app:accounts/inventory", name, fn.RequiresCapability)
		}
		call := &BuiltinFunctionExpression{Name: name, Executor: executor, Args: map[string]any{"accountId": "acme"}}

		// An admin opens the Accounts app and still may not read the ledger:
		// the part is seeded on owner and developer only.
		for _, role := range []string{"admin", "user", "viewer"} {
			nodes, err := e.evaluateBuiltinFunctionExpression(asCaller(role), call, 0)
			if err == nil {
				t.Fatalf("%s answered a %s with %d rows", name, role, len(nodes))
			}
			if !strings.HasPrefix(err.Error(), CodeCapabilityNotHeld+": ") || !strings.Contains(err.Error(), "app:accounts/inventory") {
				t.Fatalf("%s refused a %s with %q, want the capability refusal naming app:accounts/inventory", name, role, err)
			}
		}
		if reached[executor] != 0 {
			t.Fatalf("%s reached its handler %d times for refused callers", name, reached[executor])
		}

		// THE REACHABLE POSITIVE: a gate that refused everybody would pass the
		// half above.
		for _, role := range []string{"owner", "developer"} {
			nodes, err := e.evaluateBuiltinFunctionExpression(asCaller(role), call, 0)
			if err != nil {
				t.Fatalf("%s refused a %s, whom the seeds grant it: %v", name, role, err)
			}
			if len(nodes) != 1 {
				t.Fatalf("%s answered a %s with %d rows, want 1", name, role, len(nodes))
			}
		}
		if reached[executor] != 2 {
			t.Fatalf("%s reached its handler %d times, want once per admitted caller", name, reached[executor])
		}
	}
}
