package memql

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/provenance"
	coreid "github.com/znasllc-io/memql/core/id"
)

// Issue #2885: createDeploymentNodeSpec stamped a raw
// concat(deploymentId, ":", nodeType) as the concept id. Concept.Create
// runs core/id ValidateShortId BEFORE it touches the store, and that
// admits only a colon-free shortId or the concept-prefixed form -- so the
// mutation was rejected on every call and no v1:cluster:deploymentNodeSpec
// row could ever be written. nodeSpecsForDeployment therefore returned
// nothing in every environment.
//
// Why these tests are DB-free: the rejection happens at
// component/database/memory-nodes/concept.go before the store is reached,
// so a fake store is enough to prove both the failure and the fix. That
// matters -- the sibling *_db_test.go suite green-by-skips wherever no
// Postgres is reachable, so a DB-gated test alone would not have caught
// this on most machines.
//
// Why the engine is booted rather than the expression hand-written: the
// point is that the DSL as authored renders a storable id. Asserting on a
// hand-copied string would pass even if dsl/deployment/mutations.memql
// changed underneath it.
//
// eng.Execute is deliberately NOT used: executeWith hard-rejects a nil
// database (component/memql/engine.go) before it parses anything, so a
// DB-free test must render the template directly.
//
// NOT asserted here, deliberately: that a bare and a canonical deploymentId
// derive the same id. They do not -- the hash is byte-level and the arg is
// not normalised first. That gap is #2925; it is unreachable in-tree today
// (every producer passes a bare id) and there are no rows to migrate.

// fakeConceptStore satisfies memorynodes.Store. Create only ever calls
// InsertMemoryNode; QueryMemoryNodes is present to satisfy the interface.
type fakeConceptStore struct{ inserted []*memorynodes.MemoryNode }

func (s *fakeConceptStore) InsertMemoryNode(_ context.Context, n *memorynodes.MemoryNode) error {
	s.inserted = append(s.inserted, n)
	return nil
}

func (s *fakeConceptStore) QueryMemoryNodes(_ context.Context, _ memorynodes.QueryParams) ([]memorynodes.MemoryNode, error) {
	return nil, nil
}

func nodeSpec2885Engine(t *testing.T) (*MemQLEngine, memorynodes.Registry) {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := memorynodes.DefaultRegistry()
	require.NotNil(t, registry, "concept registry must load")
	eng, err := New(nil) // nil DB: nothing below this reaches the store
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry), "engine.Init over the full DSL tree")
	return eng, registry
}

func renderNodeSpecMutation(t *testing.T, eng *MemQLEngine, name string, args map[string]any) MutationNode {
	t.Helper()
	fn, err := eng.Functions().Get(name)
	require.NoError(t, err, "%s must load from the DSL tree", name)
	require.NotNil(t, fn.MutationTemplate, "%s must be a mutation", name)
	mutation, err := eng.renderMutationTemplate(context.Background(), fn.MutationTemplate, args)
	require.NoError(t, err, "rendering %s", name)
	return mutation
}

func nodeSpecArgs(deploymentID string) map[string]any {
	return map[string]any{
		"deploymentId": deploymentID,
		"nodeType":     "bff",
		"replicas":     2,
		"version":      "1.0.0",
	}
}

// The headline #2885 assertion. Both mutations must render an id the
// storage gate accepts. This asserts the INVARIANT rather than a literal,
// so it stays correct for any future id derivation that remains storable.
func TestNodeSpecMutationIdsSatisfyTheStorageGate(t *testing.T) {
	eng, _ := nodeSpec2885Engine(t)
	for _, name := range []string{"createDeploymentNodeSpec", "updateDeploymentNodeSpec"} {
		t.Run(name, func(t *testing.T) {
			mutation := renderNodeSpecMutation(t, eng, name, nodeSpecArgs("d-abc123"))
			require.Equal(t, "v1:cluster:deploymentNodeSpec", mutation.Concept)
			require.NoError(t,
				coreid.ValidateShortId(mutation.Concept, mutation.ID),
				"memql#2885: %s renders id %q, which Concept.Create rejects before the store is reached",
				name, mutation.ID)
		})
	}
}

// A re-pin must land on the create's timeline. The two id expressions are
// the only thing holding that invariant, and a divergence forks a second
// timeline silently rather than failing.
func TestNodeSpecCreateAndUpdateDeriveTheSameId(t *testing.T) {
	eng, _ := nodeSpec2885Engine(t)
	created := renderNodeSpecMutation(t, eng, "createDeploymentNodeSpec", nodeSpecArgs("d-abc123"))
	repinned := renderNodeSpecMutation(t, eng, "updateDeploymentNodeSpec", nodeSpecArgs("d-abc123"))
	require.Equal(t, created.ID, repinned.ID,
		"createDeploymentNodeSpec and updateDeploymentNodeSpec must derive a byte-identical concept id, "+
			"or a re-pin appends to a second timeline instead of the first")
}

// Determinism across renders. `now` is stamped into the payload, never into
// the id, so the same key must always produce the same row id.
func TestNodeSpecIdIsDeterministic(t *testing.T) {
	eng, _ := nodeSpec2885Engine(t)
	first := renderNodeSpecMutation(t, eng, "createDeploymentNodeSpec", nodeSpecArgs("d-abc123"))
	second := renderNodeSpecMutation(t, eng, "createDeploymentNodeSpec", nodeSpecArgs("d-abc123"))
	require.Equal(t, first.ID, second.ID, "the nodeSpec id must be a pure function of (deploymentId, nodeType)")
}

// The end-to-end DB-free proof: the rendered mutation actually produces an
// insertable row. This is the assertion the issue asked for and that no
// existing test made -- cluster_nodespec_2094_test.go only checks the
// constructs are REGISTERED, which is why the defect shipped.
func TestCreateDeploymentNodeSpecRowReachesTheStore(t *testing.T) {
	eng, registry := nodeSpec2885Engine(t)
	mutation := renderNodeSpecMutation(t, eng, "createDeploymentNodeSpec", nodeSpecArgs("d-abc123"))

	c, err := registry.Get(mutation.Concept)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))

	ctx := provenance.ContextWithProvenance(context.Background(), provenance.Provenance{
		Kind: provenance.KindMutation,
		Name: "createDeploymentNodeSpec",
	})
	store := &fakeConceptStore{}
	node, err := c.Create(ctx, store, memorynodes.CreateParams{
		Actor:   "system:nodespec-2885",
		ID:      mutation.ID,
		Payload: payload,
	})
	require.NoError(t, err,
		"memql#2885: createDeploymentNodeSpec must produce an insertable row; rendered id was %q", mutation.ID)
	require.Len(t, store.inserted, 1, "exactly one row must reach the store")
	require.Equal(t, node.ID, store.inserted[0].ID)
	require.Equal(t, "v1:cluster:deploymentNodeSpec", store.inserted[0].Concept)

	// The key parts stay readable as payload fields: nodeSpecsForDeployment
	// filters on payload.deploymentId, not on the row id, so hashing the id
	// costs no query capability.
	require.Equal(t, "d-abc123", payload["deploymentId"])
	require.Equal(t, "bff", payload["nodeType"])
}

// Hashing launders a degenerate key into a valid-looking id, which the old
// expression rejected by accident: an empty deploymentId rendered ":bff",
// which the colon gate refused, but hash(":bff") is a perfectly good 64-hex
// shortId that ValidateShortId accepts. Required-arg validation does not
// close this -- it only rejects an ABSENT or nil arg, not "" -- so both key
// fields carry @minLength(1) and the schema check rejects the row before
// the id is ever considered.
func TestNodeSpecRejectsEmptyKeyParts(t *testing.T) {
	eng, registry := nodeSpec2885Engine(t)
	for _, tc := range []struct {
		name         string
		deploymentID string
		nodeType     string
	}{
		{"empty deploymentId", "", "bff"},
		{"empty nodeType", "d-abc123", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := nodeSpecArgs(tc.deploymentID)
			args["nodeType"] = tc.nodeType
			mutation := renderNodeSpecMutation(t, eng, "createDeploymentNodeSpec", args)

			c, err := registry.Get(mutation.Concept)
			require.NoError(t, err)
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))

			ctx := provenance.ContextWithProvenance(context.Background(), provenance.Provenance{
				Kind: provenance.KindMutation,
				Name: "createDeploymentNodeSpec",
			})
			store := &fakeConceptStore{}
			_, err = c.Create(ctx, store, memorynodes.CreateParams{
				Actor:   "system:nodespec-2885",
				ID:      mutation.ID,
				Payload: payload,
			})
			require.Error(t, err,
				"an empty key part must not produce an insertable row; hashing made it look valid as id %q", mutation.ID)
			require.Empty(t, store.inserted, "nothing may reach the store for a degenerate key")
		})
	}
}
