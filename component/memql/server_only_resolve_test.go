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

// argsFor returns the call args for a construct under test. The memql#2883
// constructs declare NO required args -- which is the whole reason they were
// dangerous, since calling them with nothing returned the entire user table --
// so they take an empty map rather than userIdArg(), whose `userId` they do not
// declare.
func argsFor(name string) map[string]any {
	switch name {
	case "userByEmail":
		return emailArg()
	case "searchUsers", "activeUsers", "usersInDeletionCooldown", "usersScheduledForDeletion":
		return map[string]any{}
	default:
		return userIdArg()
	}
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
		// memql#2883. searchUsers matters most here: it gained a
		// requiresOwnerOrAdmin conjunct, and this is the test that would have
		// caught #2800's dead `spec("...")` gate. A context-spec that does not
		// resolve is a gate that refuses everyone including admins.
		"searchUsers", "activeUsers", "usersInDeletionCooldown", "usersScheduledForDeletion",
	} {
		// Internal origin, so @serverOnly is not what fails here and any
		// error is a genuine resolution problem.
		if err := resolveAuthored(t, fns, specs, name, auth.OriginInternal, argsFor(name)); err != nil {
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
		// memql#2883. All three project userFull -- every @pii field plus the
		// cluster-wide auth role -- and all three take only optional args, so
		// before the gate a bare client call returned every matching user.
		"activeUsers":               true,
		"usersInDeletionCooldown":   true,
		"usersScheduledForDeletion": true,
		// The client-callable siblings must still resolve.
		"userDisplayById": false,
		"currentUser":     false,
		"userById":        false, // admin-gated, but reachable from the wire
		// memql#2883: searchUsers is gated by ROLE, not by origin, because
		// unlike its siblings it has a real client caller (the MCP tool). It
		// must therefore still RESOLVE from the wire -- the role check happens
		// when the predicate evaluates, not at expansion.
		"searchUsers": false,
	} {
		err := resolveAuthored(t, fns, specs, name, auth.OriginClient, argsFor(name))
		refused := err != nil && strings.Contains(err.Error(), "server-only")
		if refused != wantRefused {
			t.Errorf("%s: refused=%v want %v (err=%v)", name, refused, wantRefused, err)
		}
	}
}

// TestServerOnlyQueriesPassInternalOrigin is the other half: the server-side
// callers must keep working, which is the entire reason @serverOnly exists
// rather than @disabled.
func TestServerOnlyQueriesPassInternalOrigin(t *testing.T) {
	fns, specs := loadRealTree(t)

	for _, name := range []string{
		"userByIdSystem", "runningPlansForUser",
		// memql#2883. These three have live server-side callers that MUST keep
		// working: the identity store's bootstrap counts, the admin user list,
		// the PAT roll-up, the seed materializer, and the two deletion sweeps.
		// Gating them by origin is only correct if internal origin still passes.
		"activeUsers", "usersInDeletionCooldown", "usersScheduledForDeletion",
	} {
		// Asserts on ANY error, not just a "server-only" one. Gating on the
		// message let an UNRESOLVABLE construct pass -- the #2800 dead-gate
		// defect this file exists to catch. Measured: mutating
		// runningPlansForUser's filter to the `spec("...")` form left this
		// test, ./dsl/... and ./component/automations all green.
		// runningPlansForUser is not in TestAuthoredQueriesResolve, so this
		// was its only coverage (memql#2881 review round 2).
		if err := resolveAuthored(t, fns, specs, name, auth.OriginInternal, argsFor(name)); err != nil {
			t.Errorf("%s did not resolve from INTERNAL origin: %v -- this is the path "+
				"authentication and the kill switch depend on", name, err)
		}
	}
}
