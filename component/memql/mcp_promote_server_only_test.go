package memql

import (
	"testing"
)

// mcp_promote_server_only_test.go -- memql#2800.
//
// The `if fn.ServerOnly { continue }` skip in mcpPromotedFunctions was added
// with no test. Review verified it was DELETABLE WITH A GREEN SUITE: nothing
// asserted that a construct carrying both @serverOnly and @mcp is dropped from
// the advertised surface.
//
// The failure that guards against is a future refactor deciding the skip looks
// redundant ("the engine gate refuses it at call time anyway"), CI staying
// green, and userByIdSystem / runningPlansForUser reappearing on the MCP
// connector's advertised tool list. That is precisely the
// advertised-but-always-refused dishonest surface the skip's own comment cites
// memql#2647 for.
//
// These drive the registry directly rather than the authored tree, because the
// property under test is "IF such a construct exists, it is dropped" -- and no
// authored construct carries both today. A test that only walked the tree
// would assert nothing, and would keep asserting nothing on the day someone
// adds the pair.

// engineWithFunctions builds a bare engine whose registry holds exactly the
// supplied functions.
func engineWithFunctions(t *testing.T, fns ...*Function) *MemQLEngine {
	t.Helper()
	reg := newFunctionRegistry()
	for _, fn := range fns {
		if err := reg.Upsert(fn); err != nil {
			t.Fatalf("register %s: %v", fn.Name, err)
		}
	}
	return &MemQLEngine{functions: reg}
}

func promotableFn(name string) *Function {
	return &Function{
		Name:         name,
		FunctionKind: "query",
		MCPPromoted:  true,
		Enabled:      true,
	}
}

// TestMCPPromotionDropsServerOnlyConstructs is the assertion whose absence let
// an untested security filter ship.
func TestMCPPromotionDropsServerOnlyConstructs(t *testing.T) {
	open := promotableFn("zzOpenReader")
	gated := promotableFn("zzServerOnlyReader")
	gated.ServerOnly = true

	e := engineWithFunctions(t, open, gated)

	var got []string
	for _, fn := range e.mcpPromotedFunctions() {
		got = append(got, fn.Name)
	}

	// Guard the guard: if the open construct is missing too, the assertion
	// below would pass for the wrong reason (nothing is promoted at all).
	if len(got) != 1 || got[0] != open.Name {
		t.Fatalf("MCP promotion returned %v, want exactly [%s].\n"+
			"A @serverOnly construct must NOT be advertised on the MCP surface -- it is "+
			"refused for every client-originated call, so advertising it is a dishonest "+
			"surface (memql#2647), and these constructs exist precisely because they read "+
			"across users.", got, open.Name)
	}
}

// TestMCPPromotionSkipIsNotRedundantWithEnabled pins the two gates apart.
//
// A reader could reasonably assume @serverOnly implies !Enabled and delete one
// check. It does not: a @serverOnly construct is fully enabled and fully
// callable -- from server-side Go. Only its ORIGIN is constrained.
func TestMCPPromotionSkipIsNotRedundantWithEnabled(t *testing.T) {
	gated := promotableFn("zzEnabledButServerOnly")
	gated.ServerOnly = true
	gated.Enabled = true

	e := engineWithFunctions(t, gated)
	if n := len(e.mcpPromotedFunctions()); n != 0 {
		t.Fatalf("an ENABLED @serverOnly construct was promoted to the MCP surface (%d promoted). "+
			"@serverOnly constrains ORIGIN, not lifecycle -- the Enabled check does not cover it.", n)
	}
}
