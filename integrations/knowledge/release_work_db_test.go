package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/knowledge/seeddata"
)

func releaseWorkFixture(t *testing.T) (*seedFixture, context.Context, func(string, map[string]any)) {
	t.Helper()
	f, system := releaseFixture(t)
	for _, pair := range []struct {
		e *memql.MemQLEngine
		i *Integration
	}{{f.ea, f.a}, {f.eb, f.b}} {
		require.NoError(t, pair.e.RegisterIntegration(pair.i))
	}
	require.NoError(t, f.a.ReconcileReleaseSeeds(system, false))
	write := func(name string, args map[string]any) {
		call, err := parser.RenderCall(name, args)
		require.NoError(t, err)
		_, err = f.ea.Execute(auth.ContextWithInternalOrigin(f.ctx), "mutation "+call)
		require.NoError(t, err)
	}
	write("createWorkGoal", map[string]any{"goalId": f.domain + "goal", "statement": "Verify reviewed knowledge availability", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": f.domain + "run", "goalId": f.domain + "goal", "automationName": "fixture", "templateFingerprint": "fixture", "mode": "live", "status": "running", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	ctx := auth.ContextWithInternalOrigin(common.ContextWithRun(f.ctx, common.RunContext{RunId: f.domain + "run", GoalId: f.domain + "goal", OwnerUserId: f.domain, StepKey: "section", Mode: common.RunModeLive}))
	old, had := memql.ActiveEmbedderBinding()
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: f.provider, Dimensions: 1024})
	t.Cleanup(func() {
		if had {
			memql.SetActiveEmbedderBinding(old)
		} else {
			memql.ClearActiveEmbedderBinding()
		}
	})
	return f, ctx, write
}

func TestOwnedReleaseIndexingFreshScopeAcrossReplicas(t *testing.T) {
	f, ctx, write := releaseWorkFixture(t)
	p := &releaseEmbedding{calls: map[string]int{}}
	for _, i := range []*Integration{f.a, f.b} {
		i.SetEmbeddingProvider(func(call context.Context, _ string) (memql.EmbeddingAIProvider, error) {
			want, _ := common.RunFromContext(ctx)
			got, _ := common.RunFromContext(call)
			require.Equal(t, want, got)
			actor, _ := auth.AccessFromContext(call)
			require.Equal(t, f.domain, actor.UserId)
			return p, nil
		})
	}
	call := func(c context.Context) error {
		_, err := f.eb.Execute(c, "builtin knowledgeIndexReleaseForWork()")
		return err
	}
	run, _ := common.RunFromContext(ctx)
	foreign := run
	foreign.OwnerUserId = "someone-else"
	wrong := run
	wrong.RunId = "absent"
	moved := run
	moved.RerunRequestId = "new-request"
	replay := run
	replay.Mode = common.RunModeReplay
	for _, bad := range []context.Context{auth.ContextWithClientOrigin(ctx), auth.ContextWithInternalOrigin(context.Background()), common.ContextWithRun(ctx, foreign), common.ContextWithRun(ctx, wrong), common.ContextWithRun(ctx, moved), common.ContextWithRun(ctx, replay)} {
		require.Error(t, call(bad))
		require.Empty(t, p.calls)
	}
	for _, change := range []map[string]any{{"status": "failed"}, {"status": "running", "cancelRequested": true}} {
		change["runId"] = run.RunId
		write("updateWorkRun", change)
		require.Error(t, call(ctx))
		require.Empty(t, p.calls)
	}
	write("updateWorkRun", map[string]any{"runId": run.RunId, "status": "running", "cancelRequested": false})
	write("updateWorkGoal", map[string]any{"goalId": run.GoalId, "status": "closed"})
	require.Error(t, call(ctx))
	require.Empty(t, p.calls)
	// An older replica's warmed read must not turn a closed goal into authority.
}

type releaseInterruptEmbedding struct {
	before func(context.Context) error
	calls  atomic.Int32
}

func (p *releaseInterruptEmbedding) Embed(ctx context.Context, s string) ([]float32, error) {
	p.calls.Add(1)
	if err := p.before(ctx); err != nil {
		return nil, err
	}
	return seedTestProvider{}.Embed(ctx, s)
}
func (p *releaseInterruptEmbedding) EmbedBatch(ctx context.Context, ss []string) ([][]float32, error) {
	var out [][]float32
	for _, s := range ss {
		v, e := p.Embed(ctx, s)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func (*releaseInterruptEmbedding) Dimensions() int { return 1024 }

func TestOwnedReleaseIndexingKeepsCancellationAndStorageFailures(t *testing.T) {
	for _, mode := range []string{"cancel", "native-budget", "peer-cancel", "storage"} {
		t.Run(mode, func(t *testing.T) {
			f, ctx, write := releaseWorkFixture(t)
			cancelled, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			p := &releaseInterruptEmbedding{before: func(call context.Context) error {
				switch mode {
				case "cancel":
					cancel(context.Canceled)
					return call.Err()
				case "native-budget":
					cancel(&memql.RunCeilingError{RunId: f.domain + "run", StepKey: "section", Breach: memql.RunCeilingBreach{Ceiling: "wallClock"}})
					return call.Err()
				case "peer-cancel":
					write("updateWorkRun", map[string]any{"runId": f.domain + "run", "cancelRequested": true})
				}
				return nil
			}}
			f.b.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return p, nil })
			if mode == "storage" {
				f.b.SetDBGetter(func() *sql.DB { return nil })
			}
			_, err := f.eb.Execute(cancelled, "builtin knowledgeIndexReleaseForWork()")
			require.Error(t, err)
			if mode == "storage" {
				require.Zero(t, p.calls.Load())
			} else {
				require.Equal(t, int32(1), p.calls.Load())
			}
			if mode == "cancel" || mode == "native-budget" {
				require.ErrorIs(t, err, context.Canceled)
			}
			for _, s := range seeddata.Release() {
				present, e := f.a.hasVector(ctx, id.BuildNodeId(conceptKnowledgeDocumentChunk, s.ID), f.provider, 1024)
				require.NoError(t, e)
				require.False(t, present)
			}
		})
	}
}

func TestScheduledReleaseIndexingDefersWithoutInventingFleetAuthority(t *testing.T) {
	f, ctx, _ := releaseWorkFixture(t)
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: "fleet:private-embedder", Dimensions: 1024})
	var calls atomic.Int32
	f.a.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) {
		calls.Add(1)
		return nil, errors.New("must not reach private inference")
	})
	require.NoError(t, f.a.ReconcileReleaseSeeds(auth.ContextWithInternalOrigin(context.Background()), true))
	// A work context without an accepted assertion is also deferred. Neither
	// lifecycle nor first use manufactures credentials from an actor ID.
	rows, err := f.a.indexReleaseForWorkHandler(ctx, nil, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, string(rows[0].Payload), "fleet-authority-unavailable")
	require.Zero(t, calls.Load())
}
