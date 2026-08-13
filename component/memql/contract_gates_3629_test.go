package memql

// contract_gates_3629_test.go is the memql#3629 proof: the DSL contract gates
// run at LOAD time, over whatever tree the node mounted, not only over this
// repo's own tree at test time.
//
// The fixtures are a PRODUCT BUNDLE in miniature -- a domain registered through
// RegisterTree, which is exactly what dsl.MountRuntimeDomainsFromEnv does for a
// MEMQL_DSL_PATH bundle. Before this change every one of these bundles booted
// clean: the gates that refuse them were Go tests walking dsl.Tree() inside
// this repo, and a product's DSL never passes through a Go test at all.
//
// Each violating fixture is the CLEAN fixture with one line changed, and the
// clean one is asserted to boot with zero skips. That pairing is what makes the
// failures attributable: without it, a fixture that fails to parse for an
// unrelated reason would fail the test for the wrong reason and read as proof.

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql/dslgate"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

const gateFixtureDomain = "gatefixture"

const gateFixtureConcepts = `// gatefixture domain -- a product bundle in miniature (memql#3629 test).

/// A user-owned gadget. Per-row authz: owned -- ownerUserId is stamped from actor.userId on
/// every write and every read gates on ownerUserId==actor.userId.
@rowAuthz(owner="ownerUserId")
concept gadget {
  ownerUserId  string!  @description("v1:identity:user.id that owns this gadget. The load-bearing per-row authz key.")
  title        string!  @description("Short human-readable name for the gadget.")
  visibility   string   @description("Sharing scope for the gadget; private unless set.")
}
`

const gateFixtureShapes = `// gatefixture domain -- projections.

/// Full projection of a gadget.
@row
shape gadget gadgetFull {
  row.id
  ownerUserId
  title
  visibility
  row.createdAt
}
`

// gateFixtureQueries renders the fixture's queries.memql with one filter and
// one sort clause substituted, so every case differs from the clean baseline by
// exactly the line under test.
func gateFixtureQueries(filter, sortKey string) string {
	return `// gatefixture domain -- caller-scoped reads.

/// List the caller's gadgets. Owned: the row set is gated server-side.
@actor
query gadget gadgets {
  args {
    gadgetId  string!
  }
  filter  ` + filter + `
  sort    "` + sortKey + `", "desc"
  shape   gadgetFull
}
`
}

const (
	cleanFilter  = `ownerUserId==actor.userId && when(args.gadgetId) { row.id==args.gadgetId }`
	cleanSortKey = `row.createdAt`
)

// mountGateFixture registers the fixture domain, reloads the concept registry
// over the merged tree, and returns an engine ready to Init. Cleanup restores
// the embedded-only registry so the fixture concept does not leak into the rest
// of the package's tests.
func mountGateFixture(t *testing.T, queries string) (*MemQLEngine, concept.Registry) {
	t.Helper()

	memqldsl.RegisterTree(gateFixtureDomain, fstest.MapFS{
		"concepts.memql": {Data: []byte(gateFixtureConcepts)},
		"shapes.memql":   {Data: []byte(gateFixtureShapes)},
		"queries.memql":  {Data: []byte(queries)},
	})
	t.Cleanup(func() {
		memqldsl.UnregisterTree(gateFixtureDomain)
		concept.ReplaceAll(nil)
		_, _ = LoadUnifiedConcepts(nil)
	})

	concept.ReplaceAll(nil)
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts over core + fixture: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after load")
	}

	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return eng, registry
}

// TestContractGatesAcceptACleanBundle is the control. Without it, a violating
// fixture that failed to load for an unrelated reason would look like the gate
// working.
func TestContractGatesAcceptACleanBundle(t *testing.T) {
	eng, registry := mountGateFixture(t, gateFixtureQueries(cleanFilter, cleanSortKey))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("a clean product bundle must boot: %v", err)
	}
	if eng.loadReport == nil {
		t.Fatal("engine.loadReport should be set after Init")
	}
	if len(eng.loadReport.Skipped) != 0 {
		t.Fatalf("clean fixture produced load-report skips, so the violating cases below would be "+
			"unattributable: %+v", eng.loadReport.Skipped)
	}
}

// TestContractGatesRefuseAViolatingBundle is the memql#3629 lock: each of these
// mounted bundles must REFUSE to boot. Every one of them booted green before
// this change, because the gate that catches it only ever ran as a Go test over
// this repo's own tree.
func TestContractGatesRefuseAViolatingBundle(t *testing.T) {
	cases := []struct {
		name    string
		filter  string
		sortKey string
		gate    dslgate.Gate
		why     string
	}{
		{
			// The memql#3612 shape. The engine reads `,` as a pure alias for
			// `||`, so the ownership conjunct becomes a DISJUNCT and every
			// row matching the other arm is returned. Both gates that refuse
			// it -- the retired-operator gate and the authz classifier -- were
			// test-only, which is why this issue leads with it.
			//
			// The clause keeps its args.gadgetId reference on purpose: an
			// args field declared and never referenced is refused by a
			// different load-time check (memql#3626), which would refuse this
			// fixture for a reason that has nothing to do with the gate.
			name:    "retired comma-as-OR inside parens is an authorization bypass",
			filter:  `(ownerUserId==actor.userId, title==args.gadgetId)`,
			sortKey: cleanSortKey,
			gate:    dslgate.GateRetiredOperator,
			why:     "`,` is the retired OR separator and the engine still honours it",
		},
		{
			name:    "a user-scope column selected with no caller check",
			filter:  `ownerUserId==args.gadgetId`,
			sortKey: cleanSortKey,
			gate:    dslgate.GateUserScopeSelection,
			why:     "the row set is picked by an owner column the caller supplies",
		},
		{
			name:    "an admin gate composed as a disjunct is switched off by the other arm",
			filter:  `title==args.gadgetId || actor.isClusterOwner==true`,
			sortKey: cleanSortKey,
			gate:    dslgate.GateAdminGateComposition,
			why:     "a false gate does not zero the result set",
		},
		{
			name:    "a bare row intrinsic in a filter",
			filter:  `ownerUserId==actor.userId && id==args.gadgetId`,
			sortKey: cleanSortKey,
			gate:    dslgate.GateFilterRowIntrinsic,
			why:     "a bare name is a payload property, so this filters a JSONB path rather than the row id",
		},
		{
			name:    "a bare row intrinsic as a sort key",
			filter:  cleanFilter,
			sortKey: `createdAt`,
			gate:    dslgate.GateSortRowIntrinsic,
			why:     "a bare sort key orders by a payload property of that name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng, registry := mountGateFixture(t, gateFixtureQueries(tc.filter, tc.sortKey))
			err := eng.Init(registry)
			if err == nil {
				t.Fatalf("mounted product bundle booted clean, but %s -- %s.\n"+
					"A gate that runs only as a Go test over this repo's tree does not protect a bundle "+
					"delivered at runtime through MEMQL_DSL_PATH (memql#3629).", tc.why, tc.gate)
			}
			if !strings.Contains(err.Error(), string(tc.gate)) {
				t.Fatalf("boot was refused, but not by %s -- the refusal must name the gate so an "+
					"operator can act on it.\n%v", tc.gate, err)
			}
			if !strings.Contains(err.Error(), AllowSkipsEnvVar) {
				t.Errorf("the refusal must name the operator break-glass %s: %v", AllowSkipsEnvVar, err)
			}
		})
	}
}

// TestContractGatesPassOnTheEmbeddedTree is the parity assertion between the
// load-time gates and the test-time ones they were extracted from: the corpus
// those tests pass on must also boot.
//
// It is also the check on how @serverOnly is resolved. The gate takes that
// verdict from the loaded FunctionRegistry rather than from a source regex
// (memql#2875 -- a regex is fail-open), and a mistake in the derivation would
// show up here as the embedded tree suddenly refusing to boot.
func TestContractGatesPassOnTheEmbeddedTree(t *testing.T) {
	concept.ReplaceAll(nil)
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("the embedded tree must boot under the load-time contract gates: %v", err)
	}
	for _, s := range eng.loadReport.Skipped {
		if strings.HasPrefix(s.Phase, "contract-gate:") {
			t.Errorf("embedded tree contract-gate violation: %s %q in %s (%s): %s",
				s.Keyword, s.Name, s.File, s.Phase, s.Err)
		}
	}
}
