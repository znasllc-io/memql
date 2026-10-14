package router

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

type evaluationEmbedder struct{ calls int }

func (e *evaluationEmbedder) Call(context.Context, string) (any, error) { return nil, nil }
func (e *evaluationEmbedder) Embed(context.Context, string) ([]float32, error) {
	e.calls++
	return []float32{1, 2}, nil
}
func (e *evaluationEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	e.calls++
	return [][]float32{{1, 2}}, nil
}
func (e *evaluationEmbedder) Dimensions() int { return 2 }

func TestNoPaidCeilingCoversEmbeddingAndCallTimeFallback(t *testing.T) {
	embed := &evaluationEmbedder{}
	cloud := &countingCloud{}
	providers := memql.NewProviderRegistryForTest()
	providers.RegisterWithParamsForTest("paidEmbedder", "OpenAIEmbedding", "test-embedder", nil, embed)
	providers.RegisterWithParamsForTest("paidChat", "OpenAI", "test-chat", map[string]any{"contextWindow": 200000}, cloud)
	r := New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"p": {"paidChat"}}), testRules(t, defaultRule("p")), nil, nil)
	// Only the serialized row travels to the second host; no original request
	// context or process-local budget flag participates in this refusal.
	row := map[string]any{"source": "fleet:qwen3.8:27b", "by": "alice", "noPaidProviders": true}
	ctx := chosenRun("reason", common.RouteChoiceFrom(row))
	req := ResolveRequest{Level: airoute.LevelEmbeddings, Modality: airoute.ModalityEmbedding, ExplicitProvider: "paidEmbedder"}
	if got, err := r.ResolveFor(ctx, req); err == nil {
		_, _ = got.Client.(memql.EmbeddingAIProvider).Embed(context.Background(), "probe")
		t.Fatal("paid embedding admitted")
	}
	if embed.calls != 0 {
		t.Fatal("embedding spent before refusal")
	}
	// Prove the candidate really serves embeddings when no ceiling applies.
	got, err := r.ResolveFor(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got.Client.(memql.EmbeddingAIProvider).Embed(context.Background(), "control"); err != nil {
		t.Fatal(err)
	}
	if embed.calls != 1 {
		t.Fatal("invalid control")
	}
	// Fallback/consent uses providerLookup after selection and must enforce
	// the same ceiling even when a registered vendor is otherwise available.
	if _, _, ok := r.providerLookup(context.Background(), ResolveRequest{NoPaidProviders: true}, "paidChat", modalityChat); ok {
		t.Fatal("call-time fallback escaped the ceiling")
	}
	if _, _, ok := r.providerLookup(context.Background(), ResolveRequest{}, "paidChat", modalityChat); !ok {
		t.Fatal("invalid chat control")
	}
	if cloud.calls != 0 {
		t.Fatal("a vendor was called")
	}
}

func TestNoPaidCeilingSurvivesAnExplicitStepOverride(t *testing.T) {
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "run", OwnerUserId: "alice", StepKey: "step", Routing: common.RouteChoice{NoPaidProviders: true, By: "alice"}, Override: &common.StepOverride{Model: "paidChat"}})
	req, err := memql.ApplyRunRouting(ctx, ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat, ExplicitProvider: "paidChat"})
	if err != nil || !req.NoPaidProviders || req.ExplicitProvider != "paidChat" {
		t.Fatalf("override removed execution ceiling: %+v %v", req, err)
	}
}
