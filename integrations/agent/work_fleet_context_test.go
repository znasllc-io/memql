package agent

import (
	"context"
	"fmt"
	"testing"
	"text/template"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	"github.com/znasllc-io/memql/core/common"
)

type workContextFleet struct {
	owner                         string
	visits, calls, missingContext int
}

func (f *workContextFleet) Catalog(ctx context.Context, owner string) ([]memql.FleetModel, error) {
	f.visits++
	ac, _ := auth.AccessFromContext(ctx)
	run, _ := common.RunFromContext(ctx)
	if ac == nil || ac.UserId != f.owner || owner != f.owner || run.RunId != "adopted-run" {
		f.missingContext++
		return nil, nil
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("catalog lost cancellation deadline")
	}
	return []memql.FleetModel{{ModelId: "qwen", Params: 27_000_000_000, ContextWindow: 131072, Tools: true, StructuredOutput: true, Machines: []memql.FleetMachine{{RegistrationId: "remote-machine", Name: "machine on another replica", Online: true}}}}, nil
}

func (*workContextFleet) ModelPreference(context.Context, string) ([]string, error) { return nil, nil }

func (f *workContextFleet) Call(ctx context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	f.calls++
	ac, _ := auth.AccessFromContext(ctx)
	run, _ := common.RunFromContext(ctx)
	if req.ActingUserId != f.owner || ac == nil || ac.UserId != f.owner || run.RunId != "adopted-run" {
		return memql.FleetCallResult{}, fmt.Errorf("dispatch lost work owner or run")
	}
	if req.OnDelta != nil {
		req.OnDelta("Completed answer")
	}
	return memql.FleetCallResult{Content: "Completed answer"}, nil
}

// Start on the execution replica with identity restored from a persisted run,
// not an originating session. The actual lane must carry that identity into
// catalog selection before any provider can be dispatched to a sibling.
func TestOwnedWorkLanesPreserveFleetSelectionContext(t *testing.T) {
	prompts := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, prompts, template.New("partials")); err != nil {
		t.Fatal(err)
	}
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%v", background), func(t *testing.T) {
			owner := "v1:identity:user:fleet-work-owner"
			ctx, err := auth.ContextWithPersistedOwner(context.Background(), owner, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "adopted-run", GoalId: "goal", OwnerUserId: owner})
			ctx, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			fleet := &workContextFleet{owner: owner}
			providers := memql.NewProviderRegistryForTest()
			providers.SetFleetInference(fleet)
			rules := memql.NewRuleRegistry()
			if err := rules.Register(&memql.RuleConfig{Name: memql.DefaultRuleName, When: memql.RuleWhen{Present: map[string]bool{}}, Policy: "local", Locked: true, OnUnavailable: memql.OnUnavailableDegrade, SourceFile: "dsl/rules/rules.memql"}); err != nil {
				t.Fatal(err)
			}
			if err := rules.Finalize(); err != nil {
				t.Fatal(err)
			}
			engine := &workPromptEngine{registryEngine: registryEngine{registered: map[string]bool{"composeFile": true}}, prompts: prompts}
			r := newTestReplier(engine)
			r.router = router.New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"local": {"fleet:strongest"}}), rules, nil, nil)
			msg := &memqlv1.AgentGenerateTurnMsg{RequestId: "work-fleet", AgentId: "assistant", ActingAgent: &memqlv1.ActingAgentIdentity{Id: "assistant", Name: "Ada", Role: "assistant"}, History: []*memqlv1.AgentTurnMessage{{Role: "user", Content: "Answer this goal"}}}
			if background {
				msg.Hints = map[string]string{ExecutionLaneHintKey: ExecutionLaneBackground}
			}
			result, err := r.Handle(ctx, msg, &captureSink{})
			if err != nil {
				t.Fatalf("owned fleet was hidden on execution replica: %v", err)
			}
			if fleet.visits == 0 || fleet.calls != 1 || fleet.missingContext != 0 || result.FinalText != "Completed answer" {
				t.Fatalf("actual lane did not use owned fleet: visits=%d calls=%d result=%+v", fleet.visits, fleet.calls, result)
			}
		})
	}
}
