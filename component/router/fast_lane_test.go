package router

import (
	"context"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"testing"
)

func shippedFleetRouter(t *testing.T, models []memql.FleetModel) *Router {
	t.Helper()
	providers := registryOpenToEverybody()
	providers.SetFleetInference(&stubFleetInference{models: models})
	policies := memql.NewPolicyRegistryForTest(nil)
	if _, err := memql.LoadUnifiedPolicies(nil, policies); err != nil {
		t.Fatal(err)
	}
	rules := memql.NewRuleRegistry()
	if _, err := memql.LoadUnifiedRules(nil, rules); err != nil {
		t.Fatal(err)
	}
	return New(providers, policies, rules, nil, nil)
}

func TestShippedFastAndStrongChooseDifferentResidentModels(t *testing.T) {
	r := shippedFleetRouter(t, []memql.FleetModel{sizedModel("dense27", 27000000000, 131072), sizedModel("dense9", 9000000000, 131072), sizedModel("tiny", 800000000, 131072)})
	for _, tt := range []struct {
		level airoute.Level
		want  string
	}{{airoute.LevelFast, "fleet:dense9"}, {airoute.LevelStrong, "fleet:dense27"}} {
		_, got, err := r.ResolveChat(ResolveRequest{UserId: "alice", Level: tt.level})
		if err != nil {
			t.Fatal(err)
		}
		if got.ProviderName != tt.want || got.Decision.Considered[len(got.Decision.Considered)-1].Entry != tt.want {
			t.Errorf("%s resolved %+v decision %+v, want %s", tt.level, got, got.Decision, tt.want)
		}
	}
}

func TestFastSelectorFloorCannotFallBackToTinyOrUnknown(t *testing.T) {
	r := fleetRouter(t, memql.FleetFastest, []memql.FleetModel{sizedModel("tiny", 800000000, 131072), sizedModel("unknown", 0, 131072)})
	if _, _, err := r.ResolveChat(ResolveRequest{UserId: "alice", Level: airoute.LevelFast}); err == nil {
		t.Fatal("fast selector admitted a model below its quality floor")
	}
	_, got, err := fleetRouter(t, "fleet:tiny", []memql.FleetModel{sizedModel("tiny", 800000000, 131072)}).ResolveChat(ResolveRequest{UserId: "alice", Level: airoute.LevelFast})
	if err != nil || got.ProviderName != "fleet:tiny" {
		t.Fatalf("explicit pin refused: %+v %v", got, err)
	}
}

func TestStrongestUsesActiveParameters(t *testing.T) {
	mixture := sizedModel("mixture35", 35000000000, 131072)
	mixture.ActiveParams = 3000000000
	r := fleetRouter(t, memql.FleetStrongest, []memql.FleetModel{mixture, sizedModel("dense27", 27000000000, 131072)})
	_, got, err := r.ResolveChat(ResolveRequest{UserId: "alice", Level: airoute.LevelStrong})
	if err != nil || got.ProviderName != "fleet:dense27" {
		t.Fatalf("strongest=%+v err=%v", got, err)
	}
}

func TestFastSelectorUsesActiveParametersForOrderAndFloor(t *testing.T) {
	mixture := sizedModel("mixture35", 35_000_000_000, 131072)
	mixture.ActiveParams = 3_000_000_000
	tinyMixture := sizedModel("tinyActive", 100_000_000_000, 131072)
	tinyMixture.ActiveParams = 800_000_000
	r := shippedFleetRouter(t, []memql.FleetModel{tinyMixture, sizedModel("dense9", 9_000_000_000, 131072), mixture})
	_, got, err := r.ResolveChat(ResolveRequest{UserId: "alice", Level: airoute.LevelFast})
	if err != nil || got.ProviderName != "fleet:mixture35" {
		t.Fatalf("fast=%+v err=%v", got, err)
	}
}

func TestFastLanePreservesExplicitEscalationAndLocalCompilation(t *testing.T) {
	r := shippedFleetRouter(t, []memql.FleetModel{sizedModel("dense27", 27_000_000_000, 131072), sizedModel("dense9", 9_000_000_000, 131072)})
	for _, req := range []ResolveRequest{
		{UserId: "alice", Level: airoute.LevelFast, Tags: []string{"backgroundEscalation"}},
		{UserId: "alice", Level: airoute.LevelFast, PromptName: "compileRule"},
	} {
		_, got, err := r.ResolveChat(req)
		if err != nil || got.ProviderName != "fleet:dense27" {
			t.Fatalf("request=%+v resolved=%+v err=%v", req, got, err)
		}
	}
}

type quantPreferredFleet struct {
	*stubFleetInference
	preference []string
}

func (f *quantPreferredFleet) ModelPreference(context.Context, string) ([]string, error) {
	return f.preference, nil
}

func TestStrongDefaultUsesQ8AfterUpgradeWithQ4StillInstalled(t *testing.T) {
	q4 := sizedModel("qwen3.8:27b", 27_300_000_000, 262144)
	q4.Quant = "Q4_K_M"
	q8 := sizedModel("qwen3.8:27b-q8_0", 27_300_000_000, 262144)
	q8.Quant = "Q8_0"
	for _, preferQ4 := range []bool{false, true} {
		r := shippedFleetRouter(t, []memql.FleetModel{q4, q8})
		fleet := &quantPreferredFleet{stubFleetInference: &stubFleetInference{models: []memql.FleetModel{q4, q8}}}
		want := "fleet:" + q8.ModelId
		if preferQ4 {
			fleet.preference = []string{q4.ModelId}
			want = "fleet:" + q4.ModelId
		}
		r.providers.SetFleetInference(fleet)
		_, got, err := r.ResolveChat(ResolveRequest{UserId: "alice", Level: airoute.LevelStrong})
		if err != nil || got.ProviderName != want {
			t.Fatalf("preferQ4=%v: got %s err=%v want %s", preferQ4, got.ProviderName, err, want)
		}
	}
}
