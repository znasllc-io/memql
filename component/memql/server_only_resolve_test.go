package memql

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// loadRealTree builds an engine whose registry holds the ACTUAL dsl/ tree, so
// these tests exercise the authored constructs rather than fixtures.
func loadRealTree(t *testing.T) (*FunctionRegistry, *SpecRegistry) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := newFunctionRegistry()
	if _, _, err := LoadUnifiedFunctions(logger, registry, memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	specs := newSpecRegistry()
	if _, err := LoadUnifiedSpecs(logger, specs); err != nil {
		t.Fatalf("LoadUnifiedSpecs: %v", err)
	}
	return registry, specs
}

// resolveAuthored expands the named construct through the same frame a real
// query goes through (functionValidator.expandFunctionCall), at the given
// origin. It deliberately avoids the engine's Execute path, which needs a
// database -- the defect this catches lives in expansion, not in execution.
func resolveAuthored(t *testing.T, fns *FunctionRegistry, specs *SpecRegistry, name string, origin auth.CallOrigin, args map[string]any) error {
	t.Helper()
	fn, err := fns.Get(name)
	if err != nil || fn == nil {
		t.Fatalf("construct %q not in the loaded tree: %v", name, err)
	}
	v := newFunctionValidatorWithOrigin(fns.Snapshot(), specs, origin)
	_, err = v.expandFunctionCall(&FunctionCallExpression{Name: name, Args: args})
	return err
}

// userIdArg is the required arg most constructs under test declare.
func userIdArg() map[string]any {
	return map[string]any{"userId": "v1:identity:user:someone"}
}

// emailArg is userByEmail's required arg -- it keys on the address, not an id,
// which is precisely what made it the sharpest ungated read (memql#2881).
func emailArg() map[string]any {
	return map[string]any{"primaryEmail": "someone@example.com"}
}

// TestAuthoredQueriesResolve is the test whose absence let a dead gate ship.
//
// memql#2800 first wrote userById's gate as `spec("requiresOwnerOrAdmin")`.
// That call form was REMOVED in #2335 -- but the normalizer that rejects it
// (ast_converter.go) only runs on SPEC BODIES, never on a query filter, so in
// a filter it survived as a call to a function literally named `spec` and
// every invocation failed with `function "spec" not found`.
//
// Nothing caught it. The DSL tree loaded clean, TestPerRowAuthzClassification
// and TestAdminGateIsATopLevelConjunct both PASSED -- they match the gate
// name textually and never evaluate the predicate -- so the authz audit
// happily reported the construct as correctly admin-gated while it was in fact
// broken for every caller including admins.
//
// The lesson generalises past this one filter: a lexical gate cannot tell a
// working predicate from an unresolvable one. Something has to resolve it.
func TestAuthoredQueriesResolve(t *testing.T) {
	fns, specs := loadRealTree(t)

	for _, name := range []string{
		"userById", "userByIdSystem", "userDisplayById", "userActiveSpace", "currentUser",
		"userByEmail", // memql#2881
	} {
		args := userIdArg()
		if name == "userByEmail" {
			args = emailArg()
		}
		// Internal origin, so @serverOnly is not what fails here and any
		// error is a genuine resolution problem.
		if err := resolveAuthored(t, fns, specs, name, auth.OriginInternal, args); err != nil {
			t.Errorf("%s did not resolve: %v", name, err)
		}
	}
}

// TestServerOnlyQueriesRefuseClientOrigin proves the gate is live against the
// REAL authored constructs, not just a fixture -- and that it refuses by name.
func TestServerOnlyQueriesRefuseClientOrigin(t *testing.T) {
	fns, specs := loadRealTree(t)

	for name, wantRefused := range map[string]bool{
		"userByIdSystem":      true,
		"runningPlansForUser": true,
		"userByEmail":         true, // memql#2881
		// The client-callable siblings must still resolve.
		"userDisplayById": false,
		"currentUser":     false,
		"userById":        false, // admin-gated, but reachable from the wire
	} {
		args := userIdArg()
		if name == "userByEmail" {
			args = emailArg()
		}
		err := resolveAuthored(t, fns, specs, name, auth.OriginClient, args)
		refused := err != nil && strings.Contains(err.Error(), "server-only")
		if refused != wantRefused {
			t.Errorf("%s: refused=%v want %v (err=%v)", name, refused, wantRefused, err)
		}
	}
}

// TestUserByEmailPassesInternalOrigin is the gate half for memql#2881: the
// construct must still RESOLVE from internal origin, or magic-link completion
// and the /login existing-user branch break.
//
// It asserts on ANY error, not just a "server-only" one. The first version
// guarded with `err != nil && strings.Contains(err.Error(), "server-only")`,
// which passed happily when the filter was made unresolvable -- the exact
// #2800 dead-gate defect this file exists to catch, waved through by the test
// written to catch it.
//
// The store's half of the contract -- that it STAMPS internal origin rather
// than being handed it -- is not testable here, because this passes the origin
// in by hand. That lives in
// component/identity/server_only_origin_test.go.
func TestUserByEmailPassesInternalOrigin(t *testing.T) {
	fns, specs := loadRealTree(t)
	if err := resolveAuthored(t, fns, specs, "userByEmail", auth.OriginInternal, emailArg()); err != nil {
		t.Errorf("userByEmail did not resolve from internal origin: %v -- that is the "+
			"path magic-link verification and the /login branch depend on", err)
	}
}

// TestServerOnlyQueriesPassInternalOrigin is the other half: the server-side
// callers must keep working, which is the entire reason @serverOnly exists
// rather than @disabled.
func TestServerOnlyQueriesPassInternalOrigin(t *testing.T) {
	fns, specs := loadRealTree(t)

	for _, name := range []string{"userByIdSystem", "runningPlansForUser"} {
		err := resolveAuthored(t, fns, specs, name, auth.OriginInternal, userIdArg())
		if err != nil && strings.Contains(err.Error(), "server-only") {
			t.Errorf("%s was refused from INTERNAL origin: %v -- this is the path "+
				"authentication and the kill switch depend on", name, err)
		}
	}
}
