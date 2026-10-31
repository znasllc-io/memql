package router

// A @usesDeployerFleet workflow's call through the router
// (docs/superpowers/specs/2026-10-31-deployer-fleet-design.md, D8). The chain the rule
// would pick starts at a vendor, and a signed-in app is available, so a call that
// leaked past the Local route would show.

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

func deployerRequest(person string, level airoute.Level) ResolveRequest {
	return ResolveRequest{Level: level, Modality: airoute.ModalityStructured, UserId: person,
		Needs: airoute.Needs{MinContextTokens: 4000}}
}

func deployerCall(t *testing.T, person string) context.Context {
	t.Helper()
	workflow := auth.ContextWithSystemActor(context.Background(), "status-brief")
	ctx, err := memql.DeployerCallContext(common.ContextWithRun(workflow, common.RunContext{RunId: "run-1", StepKey: "generated"}), person)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestADeployersCallServesFromTheirFleetAndNeverAnApp(t *testing.T) {
	const person = "v1:identity:user:alice"
	r, cloud, apps := routeRouter(t, []memql.FleetModel{fleetModel("qwen3.8:27b", true)}, []memql.AppDoor{openApp("claude-code")})

	got, err := r.ResolveFor(deployerCall(t, person), deployerRequest(person, airoute.LevelStrong))
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	if got.Resolution.ProviderName != "fleet:qwen3.8:27b" || got.Resolution.Decision.Route != memql.RouteIdLocal {
		t.Fatalf("resolved %q under route %q, want the fleet model under Local",
			got.Resolution.ProviderName, got.Resolution.Decision.Route)
	}
	if cloud.calls != 0 || apps.calls != 0 {
		t.Fatalf("a vendor (%d) or an app (%d) was called", cloud.calls, apps.calls)
	}
}

// Every machine asleep is a refusal naming the route: the app is right there, signed
// in, and is not reached, because a visitor's click must not spend a person's session.
func TestADeployersCallRefusesWhenNoMachineIsUp(t *testing.T) {
	const person = "v1:identity:user:alice"
	r, cloud, apps := routeRouter(t, []memql.FleetModel{fleetModel("qwen3.8:27b", false)}, []memql.AppDoor{openApp("claude-code")})

	_, err := r.ResolveFor(deployerCall(t, person), deployerRequest(person, airoute.LevelStrong))
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) || refusal.Code != work.RefusalEveryDoorShut || refusal.Route != "Local" {
		t.Fatalf("err = %v, want every_door_shut naming Local", err)
	}
	if cloud.calls != 0 || apps.calls != 0 {
		t.Fatalf("a vendor (%d) or an app (%d) was called", cloud.calls, apps.calls)
	}
}
