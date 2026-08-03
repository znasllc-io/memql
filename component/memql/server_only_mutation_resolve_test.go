package memql

// server_only_mutation_resolve_test.go -- memql#2991.
//
// `updateUser` is the tree's FIRST @serverOnly MUTATION. Every incumbent is a
// query, and queries do not reach this gate: they expand through
// functionValidator.expandFunctionCall, mutations dispatch through
// (*MemQLEngine).executeMutationFunctionCall. engine.go says so where the check
// lives -- "Mutations and logic dispatch here rather than through the query
// expansion path, so the gate is repeated at each entry point ... a single
// check in one of the three would leave the other two open."
//
// So server_only_resolve_test.go, which covers the six authored @serverOnly
// QUERIES, structurally cannot cover this one. Adding "updateUser" to its map
// does not work and was tried: expandFunctionCall rejects mutations at
// function_validator.go before it ever reaches the @serverOnly check, with
// `function "updateUser" is a mutation and cannot be used inside query
// expressions` -- an error that does not contain "server-only", so the harness
// reads it as NOT refused. That addition fails identically before and after the
// fix: a permanently-red test, not a failing-first one.
//
// Until this file, the mutation dispatch gate was exercised only against a
// synthetic fixture in server_only_gate_test.go (`mk("mutation")`, name
// "secret"). The authored construct had no behavioural coverage at the point
// that actually refuses it, which means an annotation-to-loader divergence for
// mutations would have shipped green.
//
// No database: the assertion is about which error comes back from the gate, and
// the gate runs before anything touches storage.

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// TestServerOnlyMutationRefusesClientOrigin proves the gate is live against the
// REAL authored mutation, at the real dispatch point, and that it discriminates
// rather than refusing everything.
//
// Failing-first: remove @serverOnly from `mutate user updateUser` in
// dsl/identity/mutations.memql and the client-origin case stops being refused.
func TestServerOnlyMutationRefusesClientOrigin(t *testing.T) {
	fns, _ := loadRealTree(t)
	e := &MemQLEngine{}

	// Every construct this test names must actually be in the tree, asserted
	// UP FRONT rather than inside a subtest. Without it a RENAMED construct
	// returns `function "X" not found`, which does not contain "server-only" --
	// so the control subtest below would pass while guarding nothing (caught in
	// review). resolveAuthored in server_only_resolve_test.go does the same
	// check for the same reason.
	//
	// Hoisted out of the subtests deliberately: t.Fatalf on the parent's *T from
	// inside a subtest goroutine does not reliably stop the run.
	for _, name := range []string{"updateUser", "setUserActiveSpace"} {
		if fn, err := fns.Get(name); err != nil || fn == nil {
			t.Fatalf("construct %q is not in the loaded tree (%v) -- if it was renamed, move "+
				"this guard with it rather than letting the assertion pass vacuously", name, err)
		}
	}

	call := func(name string) *FunctionCallExpression {
		return &FunctionCallExpression{
			Name: name,
			Args: map[string]any{
				"userId":  "v1:identity:user:someone",
				"payload": map[string]any{"displayName": "someone"},
				"spaceId": "v1:cognition:space:somewhere",
			},
		}
	}

	// serverOnly reports whether the gate -- and not some later, unrelated
	// failure -- is what stopped the call. Matching on the message rather than
	// on err != nil matters: these calls DO fail afterwards (no concept
	// registry, no database), and treating that as a refusal would make this
	// test pass for the wrong reason.
	serverOnly := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "server-only")
	}

	t.Run("client origin is refused", func(t *testing.T) {
		_, err := e.executeMutationFunctionCall(context.Background(), call("updateUser"), fns)
		if !serverOnly(err) {
			t.Errorf("updateUser was NOT refused at client origin (err=%v).\n"+
				"@serverOnly is the only thing standing between a wire caller and "+
				"`update { id: args.userId; args.payload }` on the row that holds "+
				"v1:identity:user.role -- i.e. naming any user and any role. If the "+
				"annotation is being removed, memql#2803 Phase 5 has to land first.", err)
		}
	})

	t.Run("internal origin passes the gate", func(t *testing.T) {
		ctx := auth.ContextWithInternalOrigin(context.Background())
		_, err := e.executeMutationFunctionCall(ctx, call("updateUser"), fns)
		if serverOnly(err) {
			t.Errorf("updateUser was refused at INTERNAL origin: %v\n"+
				"This is the half that breaks the product rather than securing it: the "+
				"identity admin server is the one production caller, and it stamps an "+
				"internal origin precisely so this call keeps working.", err)
		}
	})

	t.Run("a non-serverOnly mutation is not refused", func(t *testing.T) {
		// The control. Without it, a gate that refused EVERY mutation would
		// satisfy the first subtest and look correct.
		_, err := e.executeMutationFunctionCall(context.Background(), call("setUserActiveSpace"), fns)
		if serverOnly(err) {
			t.Errorf("setUserActiveSpace carries no @serverOnly but was refused as server-only: %v\n"+
				"The gate is over-firing -- it should key on the construct's own flag.", err)
		}
	})
}
