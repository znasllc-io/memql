package memql

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/airoute"
)

type activationProbe struct {
	calls  int
	vector []float32
}

func (p *activationProbe) Embed(context.Context, string) ([]float32, error) {
	p.calls++
	return p.vector, nil
}
func (p *activationProbe) EmbedBatch(context.Context, []string) ([][]float32, error) { return nil, nil }
func (*activationProbe) Dimensions() int                                             { return 0 }

// declaredWidthProbe is an embedder that says its width before it is asked,
// as a vendor record does (its declared dimensions).
type declaredWidthProbe struct {
	*activationProbe
	width int
}

func (p declaredWidthProbe) Dimensions() int                         { return p.width }
func (declaredWidthProbe) Call(context.Context, string) (any, error) { return nil, nil }

// The readiness probe refuses a model wider than memory search's index can
// hold BEFORE it embeds anything, whichever path asked for it: bindEmbedder (a
// cluster's first model, the automatic choice, the bindEmbedder builtin) and
// memorySearchSwitch learn a width through it. The resolved model declares its
// width before it is called, so nothing is paid for a vendor's.
func TestTheProbeRefusesAModelTooWideForTheIndexBeforeEmbedding(t *testing.T) {
	probe := declaredWidthProbe{activationProbe: &activationProbe{vector: make([]float32, 3072)}, width: 3072}
	probe.vector[0] = 1
	e := &MemQLEngine{}
	e.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: probe}, nil
	}})
	_, err := e.probeEmbedder(context.Background(), "embedding3Large", "")
	require.EqualError(t, err, "embedding3Large produces vectors 3072 wide, and memory search's index holds at most 2000")
	require.Zero(t, probe.calls, "nothing embedded")

	// One the index holds is embedded and measured.
	fits := declaredWidthProbe{activationProbe: &activationProbe{vector: make([]float32, 1536)}, width: 1536}
	fits.vector[0] = 1
	e.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: fits}, nil
	}})
	dims, err := e.probeEmbedder(context.Background(), "embedding3Small", "")
	require.NoError(t, err)
	require.Equal(t, 1536, dims)
	require.Equal(t, 1, fits.calls)
}

func TestEmbedderActivationProbesPersistsAndReadsAcrossReplicas(t *testing.T) {
	a, db, _ := readMergeTestEngine(t)
	b, _, _ := readMergeTestEngine(t)
	ctx := askTestActor()
	access, _ := auth.AccessFromContext(ctx)
	owner := *access
	owner.Role = auth.RoleOwner
	ownerCtx := auth.ContextWithAccess(ctx, &owner)
	// Preserve any other fixture's binding. This suite runs only against dbtest's
	// disposable database, and never reaches a running cluster's database.
	var saved string
	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(n)),'[]')::text FROM "MemoryNodes" n WHERE concept=$1`, EmbedderBindingConceptID).Scan(&saved))
	_, err := db.DB.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept=$1`, EmbedderBindingConceptID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept=$1`, EmbedderBindingConceptID)
		_, _ = db.DB.ExecContext(context.Background(), `INSERT INTO "MemoryNodes" SELECT * FROM jsonb_populate_recordset(NULL::"MemoryNodes",$1::jsonb)`, saved)
	})
	probe := &activationProbe{vector: make([]float32, 1024)}
	probe.vector[0] = 1
	a.SetAIResolver(testAIResolver{fn: func(_ context.Context, req airoute.ResolveRequest) (ResolvedProvider, error) {
		require.Equal(t, airoute.ModalityEmbedding, req.Modality)
		return ResolvedProvider{Client: probe}, nil
	}})
	args := map[string]any{"provider": "activation-probe"}
	_, err = a.bindEmbedderBuiltin(ctx, args, 0)
	require.ErrorContains(t, err, "cluster owner")
	require.Zero(t, probe.calls)
	// A dormant plan cannot be activated under another provider's name, and
	// the refusal names the model that finishes it.
	_, err = a.Execute(auth.ContextWithInternalOrigin(ownerCtx), `mutation platform.recordEmbedderBindingPlan(bindingId:"active",providerRef:"activation-probe",dimensions:1024,versionTime:"`+time.Now().UTC().Format(time.RFC3339Nano)+`")`)
	require.NoError(t, err)
	_, err = b.ReadEmbedderBinding(ctx)
	require.ErrorIs(t, err, ErrNoEmbedderBound)
	_, err = a.bindEmbedderBuiltin(ownerCtx, map[string]any{"provider": "other-model"}, 0)
	require.EqualError(t, err, "an unfinished binding of activation-probe is in the way; choose activation-probe to finish it")
	require.Zero(t, probe.calls)
	_, err = a.bindEmbedderBuiltin(ownerCtx, args, 0)
	require.NoError(t, err)
	table, err := EmbeddingVectorTable("activation-probe", 1024)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.DB.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table) })
	binding, err := b.ReadEmbedderBinding(ctx)
	require.NoError(t, err)
	require.Equal(t, 1024, binding.Dimensions)
	require.NotEmpty(t, binding.ActivatedAt)
	// No resolver is installed on b: an idempotent second activation must read
	// the first replica's durable result rather than probe or depend on local state.
	_, err = b.bindEmbedderBuiltin(ownerCtx, args, 0)
	require.NoError(t, err)
	require.Equal(t, 1, probe.calls)
	_, err = b.bindEmbedderBuiltin(ownerCtx, map[string]any{"provider": "other-model"}, 0)
	require.ErrorContains(t, err, "Cluster > Settings > Memory search", "a change of model is a switch, made where it is offered")
}

func TestEmbeddingSpacesAndInvalidVectors(t *testing.T) {
	a, err := EmbeddingVectorTable("one", 1024)
	require.NoError(t, err)
	b, err := EmbeddingVectorTable("two", 1024)
	require.NoError(t, err)
	require.NotEqual(t, a, b)
	_, err = EmbeddingVectorTable("one", 4096)
	require.Error(t, err)
	for _, v := range [][]float32{nil, {0, 0}, {float32(math.NaN()), 1}, {float32(math.Inf(1)), 1}, {1}} {
		require.Error(t, ValidateEmbeddingVector(context.Background(), "validation-test", 2, v))
	}
	require.NoError(t, ValidateEmbeddingVector(context.Background(), "validation-test", 2, []float32{1, 0}))
}
