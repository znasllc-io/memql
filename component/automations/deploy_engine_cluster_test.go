package automations_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// TestDeployEngineClusterCompiles is the I10 (#2224) acceptance guard for the
// DevOps deployment bundle's keystone automation. It loads the live concept
// registry (engine-bootstrap parity), extracts the authored
// deployEngineCluster slice exactly as the dry-run path feeds it (file-top
// `use` preamble + the automation body), compiles it through the automations
// loader, and asserts the orchestration shape end to end:
//
//   - the deploy.requested trigger on v1:cluster:deployment,
//   - the decide -> record -> clone -> build -> place -> gate -> finish step
//     sequence (including the env `switch` and the per-node `forEach` build +
//     dev k3d import),
//   - the deployment-record lifecycle: createDeployment at the start,
//     updateDeploymentStatus(succeeded|failed) in the success/failure forks,
//   - the env split: development takes no GitOps action; staging/production
//     pin the overlay digests + sync ArgoCD.
//
// Compiling (rather than executing) is the deterministic acceptance bar a
// load-only test can prove without live DB / build-server / ArgoCD infra; the
// development path is dry-runnable through the no-op capability backends once
// the runner is wired (see automations.memql header).
func TestDeployEngineClusterCompiles(t *testing.T) {
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := memoryNodes.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry is empty after LoadUnifiedConcepts")
	}
	loader := automations.NewLoader(automations.LoaderOptions{Registry: registry})

	source := readDeploymentAutomations(t)
	var sliceSrc string
	for _, s := range memql.ExtractAutomationSlices(source) {
		if s.Name == "deployEngineCluster" {
			sliceSrc = s.Source
			break
		}
	}
	if sliceSrc == "" {
		t.Fatal("deployEngineCluster slice not found in dsl/deployment/automations.memql")
	}

	auto, err := loader.CompileSource(sliceSrc, "i10:deployEngineCluster")
	if err != nil {
		t.Fatalf("deployEngineCluster failed to compile: %v", err)
	}

	// Trigger: deploy.requested (the concept scoping rides the trigger's
	// concept filter, not the topic string).
	if auto.Trigger == nil || !strings.Contains(auto.Trigger.Event, "deploy.requested") {
		t.Fatalf("expected a deploy.requested trigger, got %+v", auto.Trigger)
	}

	// The forEach + switch steps carry synthesized IDs (the authored step name
	// is dropped when a struct-form step lowers to a raw loop/switch statement),
	// so classify the compiled steps structurally and assert BOTH the per-phase
	// shape AND the EXECUTION ORDER. Execution order is the compiled slice order
	// (the scheduler runs steps sequentially); the deploy pipeline must come out
	// authorize/record -> clone -> build -> import -> place -> gate -> outcome ->
	// finalize despite the compiler's dependency topo-sort.
	idx := func(pred func(*automations.Step) bool, what string) int {
		for i, s := range auto.Steps {
			if pred(s) {
				return i
			}
		}
		t.Fatalf("no step matched %s", what)
		return -1
	}
	isFn := func(name string) func(*automations.Step) bool {
		return func(s *automations.Step) bool { return s.Function != nil && s.Function.Name == name }
	}
	isAction := func(prefix string) func(*automations.Step) bool {
		return func(s *automations.Step) bool { return s.Action != nil && strings.HasPrefix(s.Action.Ref, prefix) }
	}
	isForEachDo := func(prefix string) func(*automations.Step) bool {
		return func(s *automations.Step) bool {
			return s.ForEach != nil && len(s.ForEach.Do) == 1 && s.ForEach.Do[0].Action != nil &&
				strings.HasPrefix(s.ForEach.Do[0].Action.Ref, prefix)
		}
	}
	isSwitchExpr := func(sub string) func(*automations.Step) bool {
		return func(s *automations.Step) bool { return s.Switch != nil && strings.Contains(s.Switch.Expression, sub) }
	}

	iAuthorize := idx(isFn("deploymentForwardAllowed"), "authorize logic")
	iRecord := idx(isFn("createDeployment"), "createDeployment write")
	iClone := idx(isAction("cloneRepoAtVersion"), "clone action")
	iBuild := idx(isForEachDo("buildEngineImage"), "build forEach")
	iImport := idx(isForEachDo("importImageToK3d"), "importDevImages forEach")
	iPlace := idx(isSwitchExpr("environment"), "place env switch")
	iGate := idx(isAction("runDeployGate"), "gate action")
	iOutcome := idx(isFn("deployOutcomeLabel"), "outcome logic")
	iFinalize := idx(isSwitchExpr("outcome"), "finalize switch")

	order := []struct {
		name string
		i    int
	}{
		{"authorize", iAuthorize}, {"record", iRecord}, {"clone", iClone},
		{"build", iBuild}, {"importDevImages", iImport}, {"place", iPlace},
		{"gate", iGate}, {"outcome", iOutcome}, {"finalize", iFinalize},
	}
	for k := 1; k < len(order); k++ {
		if order[k].i <= order[k-1].i {
			t.Errorf("deploy pipeline out of order: %s (idx %d) must run after %s (idx %d)",
				order[k].name, order[k].i, order[k-1].name, order[k-1].i)
		}
	}

	// build fans buildEngineImage over the engine node set, read from the event.
	build := auto.Steps[iBuild]
	if !strings.Contains(build.ForEach.Source, "engineNodeTypes") {
		t.Errorf("build must iterate the event engineNodeTypes set, got source %q", build.ForEach.Source)
	}

	// importDevImages is the per-node k3d import, gated to the development env.
	imp := auto.Steps[iImport]
	if !strings.Contains(imp.ForEach.Filter, "development") {
		t.Errorf("importDevImages must be filtered to the development env, got filter %q", imp.ForEach.Filter)
	}

	// place is the single env switch: development no-ops; staging/production
	// pin the overlay digests + sync ArgoCD.
	place := auto.Steps[iPlace]
	if _, ok := place.Switch.Cases["development"]; !ok {
		t.Errorf("place switch must carry a development case, got cases %v", place.Switch.Cases)
	}
	if place.Switch.Default == nil {
		t.Fatal("place switch must carry a default (staging|production) case")
	}
	defRefs := actionRefs(place.Switch.Default.Steps)
	if !hasPrefix(defRefs, "pinOverlayDigests") || !hasPrefix(defRefs, "argoSync") {
		t.Errorf("place default must pin overlay digests + sync ArgoCD, got action refs %v", defRefs)
	}

	// finalize forks on the outcome label: success records succeeded + notifies;
	// failure rolls the overlay back, records failed, + notifies.
	finalize := auto.Steps[iFinalize]
	succ, ok := finalize.Switch.Cases["succeeded"]
	if !ok {
		t.Fatalf("finalize must carry a succeeded case, got cases %v", finalize.Switch.Cases)
	}
	if !hasFunc(succ.Steps, "updateDeploymentStatus") {
		t.Error("finalize succeeded case must call updateDeploymentStatus")
	}
	if !hasPrefix(actionRefs(succ.Steps), "notifyDeploy") {
		t.Error("finalize succeeded case must notify the deploy outcome")
	}
	if finalize.Switch.Default == nil {
		t.Fatal("finalize must carry a default (failure) case")
	}
	failSteps := finalize.Switch.Default.Steps
	if !hasFunc(failSteps, "updateDeploymentStatus") {
		t.Error("finalize failure case must call updateDeploymentStatus")
	}
	failRefs := actionRefs(failSteps)
	if !hasPrefix(failRefs, "revertOverlay") || !hasPrefix(failRefs, "notifyDeploy") {
		t.Errorf("finalize failure case must revert the overlay + notify, got action refs %v", failRefs)
	}
}

func readDeploymentAutomations(t *testing.T) string {
	t.Helper()
	data, err := fs.ReadFile(memqldsl.Tree(), "deployment/automations.memql")
	if err != nil {
		t.Fatalf("read deployment/automations.memql: %v", err)
	}
	return string(data)
}

func actionRefs(steps []*automations.Step) []string {
	var refs []string
	for _, s := range steps {
		if s.Action != nil {
			refs = append(refs, s.Action.Ref)
		}
	}
	return refs
}

func hasFunc(steps []*automations.Step, name string) bool {
	for _, s := range steps {
		if s.Function != nil && s.Function.Name == name {
			return true
		}
	}
	return false
}

func hasPrefix(refs []string, prefix string) bool {
	for _, r := range refs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}
