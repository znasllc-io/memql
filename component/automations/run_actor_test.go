package automations

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// The execution records who the run acted as, and it is the actor the run's
// steps saw: the automation's own when the context carried no identity -- the
// maintenance principal for a listed automation of the engine's own tree, the
// ordinary automation actor for any other -- and the caller's when it did.
func TestTheExecutionRecordsWhoTheRunActedAs(t *testing.T) {
	compiled := func() *Automation {
		a, err := NewLoader(LoaderOptions{}).CompileSource(
			"@template\nautomation "+borrowedName+" {\n  run := logic probeLogic()\n}\n", borrowedDomain+"/automations.memql")
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return a
	}
	engines := compiled()
	engines.Source = memqldsl.SourceEngine
	caller := &auth.AccessContext{UserId: "v1:identity:user:zz-run-actor", Role: auth.RoleWriter}
	for _, tc := range []struct {
		why  string
		ctx  context.Context
		a    *Automation
		want RunActor
	}{
		{"the engine's listed automation, no identity", context.Background(), engines,
			RunActor{UserId: "system:maintenance:" + borrowedName, Role: string(auth.RoleOwner), MaintenancePrincipal: true}},
		{"another source's automation of the same name, no identity", context.Background(), compiled(),
			RunActor{UserId: "system:automation:" + borrowedName, Role: string(auth.RoleReader)}},
		{"the engine's listed automation, started by a caller", auth.ContextWithAccess(context.Background(), caller), engines,
			RunActor{UserId: caller.UserId, Role: string(auth.RoleWriter), Inherited: true}},
	} {
		probe := &actorProbe{}
		e := NewExecutor(ExecutorOptions{StepRegistry: probe})
		exec, err := e.Execute(tc.ctx, tc.a, "manual")
		e.Close()
		if err != nil || exec == nil {
			t.Fatalf("%s: run: %v", tc.why, err)
		}
		if exec.Actor != tc.want {
			t.Errorf("%s: the execution records %+v, want %+v", tc.why, exec.Actor, tc.want)
		}
		if len(probe.actors) != 1 || probe.actors[0] == nil {
			t.Fatalf("%s: recorded %d step actors, want the one step's", tc.why, len(probe.actors))
		}
		if probe.actors[0].UserId != exec.Actor.UserId {
			t.Errorf("%s: the step ran as %q and the execution records %q", tc.why, probe.actors[0].UserId, exec.Actor.UserId)
		}
	}
}
