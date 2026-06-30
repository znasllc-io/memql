package steps

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/actions"
	"github.com/znasllc-io/memql/component/automations"
)

// deployActionSrc is an authored deploy-pack primitive (the shape I8 lands):
// a single control capability rendered from params. It is loaded with a
// deployment-pack origin so it inherits the cockpit/runner policy default.
const deployActionSrc = `use capabilities.integration.argocd.{ sync }
@description("sync an argocd application")
action syncArgoApp {
  args {
    app string @required
  }
  capability sync(application: args.app)
}`

// deployRegistry loads an action under a deployment-pack origin so
// PolicyDefaultSurface routes it to cockpit/runner.
func deployRegistry(t *testing.T, src, origin string) *actions.Registry {
	t.Helper()
	reg := actions.NewRegistry()
	acts, err := actions.LoadSource(src, origin)
	if err != nil {
		t.Fatalf("LoadSource: %v", err)
	}
	for _, a := range acts {
		if err := reg.Register(a); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	return reg
}

// TestAuthoredDeployActionResolvesCockpitRunner is the I13 acceptance: a deploy
// action resolves (via the policy-default tier) to the cockpit/runner surface,
// so it runs OUTSIDE the target cluster.
func TestAuthoredDeployActionResolvesCockpitRunner(t *testing.T) {
	exec := &ActionExecutor{
		Registry:   deployRegistry(t, deployActionSrc, "deployment/actions.memql"),
		Dispatcher: &fakeDispatcher{},
	}
	res := runAuthoredStep(t, exec, "syncArgoApp@1", map[string]any{"app": "memql-staging"})
	if res.Status != "success" {
		t.Fatalf("status = %q (err=%q)", res.Status, res.Error)
	}
	out := res.Result.(map[string]any)
	if out["surface"] != actions.CockpitRunnerSurface {
		t.Fatalf("surface = %#v, want %q", out["surface"], actions.CockpitRunnerSurface)
	}
}

// TestAuthoredActionExplicitSurfaceWins proves the explicit `surface:` binding
// is honored when it serves the capability (precedence: explicit > policy).
func TestAuthoredActionExplicitSurfaceWins(t *testing.T) {
	exec := &ActionExecutor{
		Registry:   authoredRegistry(t, authoredCloneSrc),
		Dispatcher: &fakeDispatcher{},
	}
	step := &automations.Step{
		ID:   "act",
		Type: automations.StepTypeAction,
		Action: &automations.ActionStepConfig{
			Ref:     "cloneRepoAtVersion@1",
			Args:    map[string]any{"workdir": "/w", "ref": "v1"},
			Surface: actions.CockpitRunnerSurface, // shell.script is served by cockpit/runner
		},
	}
	res, err := exec.Execute(context.Background(), step, &Context{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out := res.Result.(map[string]any)
	if out["surface"] != actions.CockpitRunnerSurface {
		t.Fatalf("explicit surface = %#v, want %q", out["surface"], actions.CockpitRunnerSurface)
	}
}

// TestAuthoredActionExplicitUnknownSurfaceFails proves an explicit binding that
// no surface can honor fails loud (an authoring error), not a silent failover.
func TestAuthoredActionExplicitUnknownSurfaceFails(t *testing.T) {
	exec := &ActionExecutor{
		Registry:   authoredRegistry(t, authoredCloneSrc),
		Dispatcher: &fakeDispatcher{},
	}
	step := &automations.Step{
		ID:   "act",
		Type: automations.StepTypeAction,
		Action: &automations.ActionStepConfig{
			Ref:     "cloneRepoAtVersion@1",
			Args:    map[string]any{"workdir": "/w", "ref": "v1"},
			Surface: "computer-use:nonexistent",
		},
	}
	if _, err := exec.Execute(context.Background(), step, &Context{}); err == nil {
		t.Fatalf("expected an error for an unhonorable explicit surface")
	}
}
