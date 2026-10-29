package router

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// WORK A PIPELINE STARTED RUNS LOCAL FIRST, IN ORDER (spec 2026-10-29, D5).
//
// On 2026-10-28 a pipeline's reasoning calls took reasoningParks' chain, which
// Auto re-ranks per call, and at `reasoning` Auto's fixed quality puts an app
// (0.8) above a local model (0.6): the PR review went to the app first and the
// diagnosis tried the app before either local model. The owner wants the fleet
// first, then a subscription app, then a vendor. These are the two shipped
// rules exactly as dsl/rules/rules.memql declares them.

func pipelineRules(t *testing.T) *Router {
	t.Helper()
	return New(nil, nil, testRules(t,
		defaultRule("localFirst"),
		&memql.RuleConfig{Name: "reasoningParks", When: when("level", "reasoning"), Policy: "federationStrongest",
			Precedence: 100, OnUnavailable: memql.OnUnavailablePark, Locked: true},
		&memql.RuleConfig{Name: "pipelineLocalFirst", When: when("level", "reasoning", "tag", airoute.TagPipeline), Policy: "localFirstStrongest",
			Precedence: 105, OnUnavailable: memql.OnUnavailablePark, Locked: true},
	), nil, nil)
}

func TestAPipelineReasoningCallMatchesThePipelineRule(t *testing.T) {
	r := pipelineRules(t)
	if got := r.MatchRule(ResolveRequest{Level: airoute.LevelReasoning, Tags: []string{airoute.TagPipeline}}); got == nil || got.Name != "pipelineLocalFirst" {
		t.Fatalf("a pipeline's reasoning call matched %v, want pipelineLocalFirst", ruleName(got))
	}
	if got := r.MatchRule(ResolveRequest{Level: airoute.LevelReasoning}); got == nil || got.Name != "reasoningParks" {
		t.Fatalf("every other reasoning call keeps reasoningParks, got %v", ruleName(got))
	}
	if got := r.MatchRule(ResolveRequest{Level: airoute.LevelFast, Tags: []string{airoute.TagPipeline}}); got != nil && got.Name == "pipelineLocalFirst" {
		t.Fatal("a pipeline call at another level keeps its own lane")
	}
}

// Auto ranks only the general chains; the pipeline's chain is walked as
// written. federationStrongest, with the same entries and nothing measured, is
// re-ordered app first -- which is the difference this rule exists to make.
func TestAutoLeavesThePipelineChainInItsWrittenOrder(t *testing.T) {
	chain := []string{memql.FleetStrongest, memql.AppWildcard, "federation:strongest"}
	r := &Router{}
	got, note := r.autoRankChain(context.Background(), ResolveRequest{}, airoute.LevelReasoning, "localFirstStrongest", chain, memql.NewPolicyRegistryForTest(nil))
	if strings.Join(got, ",") != strings.Join(chain, ",") || note != "" {
		t.Fatalf("pipeline chain ranked to %v (%q), want it as written", got, note)
	}
	ranked, _ := rankAutoEntries(chain, airoute.LevelReasoning, nil, memql.RecommendedAutoBalance, "none")
	if ranked[0] != memql.AppWildcard {
		t.Fatalf("the general chain at reasoning starts with %q; this test no longer shows the difference", ranked[0])
	}
}

// A review renders at most 1 MiB. Its floor is the rendered bytes over four
// plus the default completion budget, which no 262,144-token local window
// holds, so the full-size review skips the local door by the floor alone and
// goes to the app.
func TestAFullSizeReviewSkipsTheLocalDoor(t *testing.T) {
	floor := airoute.EstimateMinContextTokens(strings.Repeat("x", 1<<20), 0)
	if floor != 266240 {
		t.Fatalf("floor = %d, want 266240", floor)
	}
	r := fleetRouter(t, memql.FleetStrongest, []memql.FleetModel{sizedModel("qwen3.8:27b", 27_000_000_000, 262144)})
	if _, _, err := r.ResolveStructured(ResolveRequest{UserId: "alice", Level: airoute.LevelReasoning, Needs: airoute.Needs{Structured: true, MinContextTokens: floor}}); err == nil ||
		!strings.Contains(err.Error(), "context window 262144 is under the floor 266240") {
		t.Fatalf("a 1 MiB review must not be offered to a 262,144-token local model: %v", err)
	}
}

// A request built outside the engine -- the agent replier's own -- reaches the
// router without the run's tags; ResolveFor, the one function every model call
// reaches, applies them from the run context the executing node built.
func TestResolveForAppliesTheRunsCallTags(t *testing.T) {
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{models: []memql.FleetModel{sizedModel("qwen3.8:27b", 27_000_000_000, 131072)}})
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"localFirst": {memql.FleetStrongest}, "federationStrongest": {memql.FleetStrongest}, "localFirstStrongest": {memql.FleetStrongest},
	})
	r := New(providers, policies, pipelineRules(t).rules, nil, nil)
	ctx := common.ContextWithRun(context.Background(), common.RunContext{RunId: "v1:work:run:diagnosis", StepKey: "diagnosed", Tags: []string{airoute.TagPipeline}})
	resolved, err := r.ResolveFor(ctx, ResolveRequest{UserId: "alice", Level: airoute.LevelReasoning, Modality: airoute.ModalityChat})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.Resolution.Decision.Rule; got != "pipelineLocalFirst" {
		t.Fatalf("rule = %q, want the run's pipeline tag to choose pipelineLocalFirst", got)
	}
}
