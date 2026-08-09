package referencepack_test

// reference_pack_test.go proves the reference pack LOADS into the engine
// registries alongside core and EXTENDS a core service surface, with NO
// database. It runs under the default `go test ./...` (no build tag).
//
// It covers the exported-API-provable half of the acceptance:
//   - the pack's embedded concept lands in the engine concept registry
//     (LoadUnifiedConcepts -> memorynodes.DefaultRegistry) -- load + extend.
//   - the pack's IntegrationProvider exposes the builtin's capability under
//     the FQN the @executor names -- the Go integration half.
//   - the contract-version gate accepts the pack's pinned ContractVersion.
//
// The tool-registry half (the pack's tool resolving in the engine's
// ToolRegistry) needs the unexported newToolRegistry constructor, so it lives
// in an in-package test at component/memql/reference_pack_load_test.go. Both run
// under the default test command and together prove load + extend.

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	referencepack "github.com/znasllc-io/memql/examples/referencepack"
)

// uniqueDomain is a throwaway namespace so this test can mount the pack tree
// without colliding with (or leaking into) any other test or the canonical
// referencepack domain. RegisterTree fails loud on a duplicate domain (issue
// 2.4), so a unique name + UnregisterTree cleanup is the contract.
const uniqueDomain = "referencepacktest"

// TestReferencePackLoadsAndExtends mounts the pack's embedded tree under a
// unique domain, runs the unified concept loader the engine runs at boot, and
// asserts the pack's concept is registered alongside core concepts.
func TestReferencePackLoadsAndExtends(t *testing.T) {
	logger := slog.Default()

	// Mount the pack's embedded .memql subtree the way a pack does at init().
	memqldsl.RegisterTree(uniqueDomain, referencepack.Tree())
	t.Cleanup(func() { memqldsl.UnregisterTree(uniqueDomain) })

	// Sanity: the overlay is reachable through the layered Tree().
	if _, err := memqldsl.Tree().Open(uniqueDomain + "/concepts.memql"); err != nil {
		t.Fatalf("pack concepts.memql not reachable via dsl.Tree(): %v", err)
	}

	// Run the same unified concept loader the engine runs at boot. It walks
	// dsl.Tree() (which now includes our overlay) and registers every concept
	// into the global memorynodes registry.
	if _, err := memql.LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts failed: %v", err)
	}

	// The pack's concept must now be in the engine's concept registry. Its id
	// assembles from @version + @namespace + name: 1.0.0 + referencepack +
	// greeting -> v1:referencepack:greeting.
	const wantConceptID = "v1:referencepack:greeting"
	if _, err := memorynodes.DefaultRegistry().Get(wantConceptID); err != nil {
		t.Fatalf("pack concept %q MUST be registered after LoadUnifiedConcepts -- "+
			"the pack failed to extend the core concept registry: %v", wantConceptID, err)
	}
}

// TestReferencePackProviderExposesCapability builds the pack's
// IntegrationProvider from a minimal PluginContext and asserts it exposes the
// builtin's capability, so the DSL @executor resolves to a real Go handler.
func TestReferencePackProviderExposesCapability(t *testing.T) {
	provider, err := referencepack.NewProvider(memql.PluginContext{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("NewProvider failed: %v", err)
	}

	if got := provider.IntegrationName(); got != "referencepack" {
		t.Fatalf("IntegrationName() = %q, want referencepack", got)
	}

	// The capability Name combines with IntegrationName into the FQN the
	// builtin's @executor("integration.referencepack.composeGreeting") names.
	const wantCap = "composeGreeting"
	var found bool
	for _, c := range provider.Capabilities() {
		if c.Name == wantCap {
			found = true
			if c.Handler == nil {
				t.Fatalf("capability %q has a nil Handler -- the @executor would dangle", wantCap)
			}
		}
	}
	if !found {
		t.Fatalf("provider Capabilities() MUST include %q so "+
			"integration.referencepack.%s resolves to a Go handler", wantCap, wantCap)
	}
}

// TestReferencePackCapabilityHandlerRuns exercises the handler directly to
// prove it produces a result (the executable Go half of the pack).
func TestReferencePackCapabilityHandlerRuns(t *testing.T) {
	provider, err := referencepack.NewProvider(memql.PluginContext{Logger: slog.Default()})
	if err != nil {
		t.Fatalf("NewProvider failed: %v", err)
	}
	var handler func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error)
	for _, c := range provider.Capabilities() {
		if c.Name == "composeGreeting" {
			handler = c.Handler
		}
	}
	if handler == nil {
		t.Fatal("composeGreeting handler not found")
	}
	nodes, err := handler(context.Background(), map[string]any{"userName": "Ada"}, 1)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("handler returned %d nodes, want 1", len(nodes))
	}
}

// TestReferencePackContractCompat asserts the pack's pinned ContractVersion is
// accepted by the loader's compatibility gate, both via the pure check and via
// the PluginRegistration wrapper the app bootstrap calls per pack.
func TestReferencePackContractCompat(t *testing.T) {
	if err := memql.CheckPluginContractCompat(referencepack.ContractVersion); err != nil {
		t.Fatalf("pack ContractVersion %d should be compatible with core "+
			"PluginContractVersion %d: %v", referencepack.ContractVersion, memql.PluginContractVersion, err)
	}

	reg := memql.PluginRegistration{
		Name:                    referencepack.Domain,
		RequiresContractVersion: referencepack.ContractVersion,
	}
	if err := reg.ValidateContract(); err != nil {
		t.Fatalf("PluginRegistration.ValidateContract for the pack should pass: %v", err)
	}
}

// TestReferencePackTreeParses parses every .memql file in the pack's embedded
// tree through the real load pipeline (struct-form rewriter -> parser).
//
// The other tests here load CONCEPTS only, so a construct that fails to parse
// in any other file sails through -- which is exactly how an args-field
// @description survived in automations.memql until memql#3336 turned that
// annotation into a load error. This gate closes that hole: the pack is the
// worked example downstream repos copy, so it has to actually load.
func TestReferencePackTreeParses(t *testing.T) {
	tree := referencepack.Tree()
	err := fs.WalkDir(tree, ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return nil
		}
		raw, readErr := fs.ReadFile(tree, path)
		if readErr != nil {
			t.Errorf("%s: read: %v", path, readErr)
			return nil
		}
		rewritten, rewriteErr := parser.NormaliseAll(string(raw))
		if rewriteErr != nil {
			t.Errorf("%s: rewrite: %v", path, rewriteErr)
			return nil
		}
		// A file whose constructs are all non-procedural (concepts, shapes,
		// builtins, tools) is stripped to bare comments by the rewriter, so
		// the generic parser sees no definitions -- not an error; those load
		// through their own dedicated loaders.
		if _, parseErr := parser.ParseFile(rewritten); parseErr != nil && !errors.Is(parseErr, parser.ErrEmptyInput) {
			t.Errorf("%s: parse: %v", path, parseErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk pack tree: %v", err)
	}
}
