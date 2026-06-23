package memql

import (
	"io"
	"log/slog"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestRBACEnforcementWiringLoadsEndToEnd is the E1.6 (memql#2074) end-to-end
// wiring gate, run on the SAME full-DSL load path the cluster executes at boot
// (LoadUnifiedConcepts + the per-construct loaders). It asserts every piece of
// the enforcement chain is present and loads cleanly:
//
//   - the role + capability concepts (the model the request path resolves
//     decisions through);
//   - the governance LOGIC entry points (governanceCanManagePrincipal /
//     governanceCanCreatePrincipal) the enforcement path calls by name;
//   - the governance BUILTINS (rbacGovernPrincipal / rbacCanCreatePrincipal)
//     they delegate to (the @executor surface onto the Go core).
//
// This is the "wire enforcement through specs + executor + handlers ... E2E via
// the full DSL load-test" acceptance: if a future edit drops the governance
// logic, the builtin, or the role catalog from the loadable tree, the
// enforcement chain breaks and this fails on the boot path rather than silently
// at runtime on one node.
func TestRBACEnforcementWiringLoadsEndToEnd(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := memoryNodes.DefaultRegistry()

	// Model concepts present.
	for _, id := range []string{"v1:rbac:role", "v1:rbac:capability"} {
		if _, err := concepts.Get(id); err != nil {
			t.Errorf("enforcement model concept %q not loaded: %v", id, err)
		}
	}

	// Governance logic + builtins registered (the named entry points the
	// request path resolves enforcement through).
	functionRegistry, err := loadEmbeddedFunctions(logger, concepts)
	if err != nil {
		t.Fatalf("loadEmbeddedFunctions: %v", err)
	}
	if _, _, err := LoadUnifiedFunctions(logger, functionRegistry, concepts); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	if _, err := LoadUnifiedBuiltins(logger, functionRegistry); err != nil {
		t.Fatalf("LoadUnifiedBuiltins: %v", err)
	}
	for _, name := range []string{
		// governance logic entry points
		"governanceCanManagePrincipal", "governanceCanCreatePrincipal",
		// governance builtins (the @executor surface onto the Go core)
		"rbacGovernPrincipal", "rbacCanCreatePrincipal",
		// capability + role catalog read surface the resolver warms from
		"activeCapabilities", "capabilitiesForRole", "capabilityGrant",
		"activeRoles", "roleBySlug",
	} {
		if !functionRegistry.Has(name) {
			t.Errorf("enforcement function %q not registered -- the enforcement chain is broken on the boot load path", name)
		}
	}
}
