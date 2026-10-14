//go:build agent

package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/knowledge"
	"github.com/znasllc-io/memql/integrations/knowledge/seeddata"
)

// The provider/catalog/router/forwarder are real. Only the remote machine's
// final inference is deterministic. No originating registry or context is
// present on the receiving replica; plannerModelLink serializes the wire.
func TestReleaseSeedFirstUseKeepsPrivateFleetAuthorityAcrossHop(t *testing.T) {
	reachable, err := dbtest.EnsureSchema(context.Background())
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "release fleet indexing", dbtest.DSN(), fmt.Errorf("database unavailable"))
		return
	}
	_, err = memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := id.NewShortId()
	model := "seed-embedding-" + key
	providerRef := "fleet:" + model
	old, had := memql.ActiveEmbedderBinding()
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: providerRef, Dimensions: 1024})
	t.Cleanup(func() {
		if had {
			memql.SetActiveEmbedderBinding(old)
		} else {
			memql.ClearActiveEmbedderBinding()
		}
	})
	store := &remoteFleetStore{candidates: []Candidate{{RegistrationId: "laptop", ConnectedNodeId: "agent-with-stream", LastSeenAt: time.Now(), Capabilities: []string{workerservice.ModelCapability}, Labels: map[string]string{workerservice.ModelLabel(model): (ModelAttributes{Embeddings: true, ContextWindow: 32768}).String()}}}}
	registry := workerservice.NewRegistry(logger, time.Now)
	machine := &workerservice.Worker{RegistrationId: "laptop", OwnerUserId: "alice", Capabilities: []string{workerservice.ModelCapability}, Labels: store.candidates[0].Labels, Concurrency: map[string]uint32{workerservice.ModelCapability: 1}}
	var mu sync.Mutex
	calls := map[string]int{}
	machine.SetModelCallFunc(func(ctx context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		access, ok := auth.AccessFromContext(ctx)
		if !ok || access.UserId != "alice" {
			return nil, fmt.Errorf("original owner did not reach worker")
		}
		if _, ok := auth.ForwardedAuthorityFromContext(ctx); !ok {
			return nil, fmt.Errorf("accepted assertion missing at worker")
		}
		if req.Kind != workerservice.ModelCallKindEmbedding || len(req.EmbeddingInput) != 1 {
			return nil, fmt.Errorf("unexpected model request")
		}
		mu.Lock()
		calls[req.EmbeddingInput[0]]++
		mu.Unlock()
		handle, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
		vector := make([]float32, 1024)
		vector[0] = 1
		go finish(workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop, Embeddings: [][]float32{vector}})
		return handle, nil
	})
	registry.Add(machine)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	require.NoError(t, err)
	defer gates.Close()
	var engines []*memql.MemQLEngine
	var integrations []*knowledge.Integration
	var db *bun.DB
	expectedRun := common.RunContext{RunId: key + "run", GoalId: key + "goal", OwnerUserId: "alice", StepKey: "section", Mode: common.RunModeLive}
	for n := 0; n < 2; n++ {
		pool := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
		t.Cleanup(func() { pool.Close() })
		db = pool
		e, err := memql.New(pool)
		require.NoError(t, err)
		e.Logger = logger
		require.NoError(t, e.Init(memorynodes.DefaultRegistry()))
		link := &plannerModelLink{handler: NewForwardHandler(registry, store, logger)}
		link.router = newForwardRouter(link, func() (string, string) { return fmt.Sprintf("bff-%d", n), "bff" }, logger)
		fleet := NewRemoteFleetInference(store, link.router, fmt.Sprintf("bff-%d", n), logger)
		providers := memql.NewProviderRegistryForTest()
		providers.SetFleetInference(fleet)
		rules := memql.NewRuleRegistry()
		require.NoError(t, rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, Policy: "embedding", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}))
		require.NoError(t, rules.Finalize())
		rtr := router.New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"embedding": {providerRef}}), rules, nil, logger)
		e.SetAIResolver(rtr)
		k := knowledge.New(logger)
		k.SetEngine(e)
		k.SetGateDB(func() *sql.DB { return gates })
		k.SetDBGetter(func() *sql.DB { return pool.DB })
		k.SetStagedConceptPredicate(e.ConceptDataIsStaged)
		k.SetEmbeddingProvider(func(ctx context.Context, name string) (memql.EmbeddingAIProvider, error) {
			if run, ok := common.RunFromContext(ctx); ok && run.GoalId != "" {
				require.Equal(t, expectedRun, run, "budget/run attribution must reach the real router")
			}
			returnProvider, _, resolveErr := memql.ResolveAITyped[memql.EmbeddingAIProvider](ctx, e, airoute.ResolveRequest{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding, ExplicitProvider: name})
			return returnProvider, resolveErr
		})
		require.NoError(t, e.RegisterIntegration(k))
		engines = append(engines, e)
		integrations = append(integrations, k)
	}
	clean := func() {
		for _, seed := range seeddata.Release() {
			_, err := db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=?`, "v1:knowledge:documentChunk:"+seed.ID)
			require.NoError(t, err)
		}
	}
	clean()
	t.Cleanup(clean)
	table, err := memql.EnsureEmbeddingVectorTable(context.Background(), db.DB, providerRef, 1024)
	require.NoError(t, err)
	t.Cleanup(func() { db.ExecContext(context.Background(), "DROP TABLE "+table) })
	authority, err := auth.ForwardedAuthorityForUser(&auth.AccessContext{UserId: "alice", Role: auth.RoleOwner}, "", "", time.Time{}, time.Now())
	require.NoError(t, err)
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	require.NoError(t, err)
	person := auth.BindForwardedContext(context.Background(), authority.Principal().Claims, access, authority)
	ctx := memql.ContextWithBackgroundLane(auth.ContextWithInternalOrigin(common.ContextWithRun(person, expectedRun)))
	write := func(name string, args map[string]any) {
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = engines[0].Execute(auth.ContextWithInternalOrigin(person), "mutation "+call)
		require.NoError(t, err)
	}
	write("createWorkGoal", map[string]any{"goalId": expectedRun.GoalId, "statement": "Use shipped reviewed guidance", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": expectedRun.RunId, "goalId": expectedRun.GoalId, "automationName": "fixture", "templateFingerprint": "fixture", "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	// First boot publishes everything, but does not borrow alice's private fleet.
	require.NoError(t, integrations[0].ReconcileReleaseSeeds(auth.ContextWithInternalOrigin(context.Background()), true))
	require.Empty(t, calls)
	// The old indexing context fails at the REAL catalog/router boundary. Merely
	// binding an embedder never gives the lifecycle actor access to alice's model.
	_, _, err = memql.ResolveAITyped[memql.EmbeddingAIProvider](auth.ContextWithSystemActor(ctx, "knowledge-release"), engines[0], airoute.ResolveRequest{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding, ExplicitProvider: providerRef})
	require.ErrorContains(t, err, "every_door_shut")
	require.Empty(t, calls)
	// Existing manual vectors model a cluster that gains new release lessons.
	// Keep this actual router/hop witness below the ordinary model rate ceiling;
	// the empty-corpus suite separately proves the per-pass eight-vector bound.
	vector := make([]float32, 1024)
	vector[0] = 1
	encoded, err := json.Marshal(vector)
	require.NoError(t, err)
	missing := 0
	for _, seed := range seeddata.Release() {
		if seed.Lesson {
			missing++
			continue
		}
		_, err = db.ExecContext(context.Background(), "INSERT INTO "+table+"(id,concept,vector_field,embedding) VALUES(?, 'v1:knowledge:documentChunk','content',?::vector)", "v1:knowledge:documentChunk:"+seed.ID, string(encoded))
		require.NoError(t, err)
	}
	for pass := 0; pass < 2; pass++ {
		results := make(chan error, 2)
		for _, e := range engines {
			go func(e *memql.MemQLEngine) {
				_, err := e.Execute(ctx, "builtin knowledgeIndexReleaseForWork()")
				results <- err
			}(e)
		}
		require.NoError(t, <-results)
		require.NoError(t, <-results)
	}
	mu.Lock()
	require.Len(t, calls, missing)
	for _, count := range calls {
		require.Equal(t, 1, count, "peer indexing must reuse the completed vector")
	}
	mu.Unlock()
	var count int
	require.NoError(t, db.NewRaw("SELECT count(*) FROM "+table).Scan(context.Background(), &count))
	require.Equal(t, len(seeddata.Release()), count)
}
