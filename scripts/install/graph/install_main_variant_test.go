package graph

import (
	"reflect"
	"testing"
)

// install_main_variant_test.go -- znasllc-io/memql#4430.
//
// The from-source lane needs an install graph that differs from install.json in
// three places and NOWHERE ELSE. Two documents describing one install is how the
// sixteen shared steps drift apart -- a dependency fixed in one and not the
// other, a receipt added to one and not the other -- and the drift is invisible
// because each document is internally valid.
//
// So the variant is not reviewed as a document. It is DERIVED here from
// install.json, and the committed file must equal that derivation exactly. A
// change to install.json flows into the expectation automatically; a change to
// install-main.json that is not one of the three deltas fails.
//
// The three deltas, and why each one exists, are argued at InstallFromMain.

// applyMainDeltas is the ONE statement of what the from-source lane changes.
func applyMainDeltas(t *testing.T, g *Graph) *Graph {
	t.Helper()
	out := *g
	out.Name = "install-main"
	out.Description = "Install a local MemQL cluster from a main checkout, building its node images " +
		"from that checkout instead of pulling a published release."

	steps := make([]Step, 0, len(g.Steps)+1)
	for _, s := range g.Steps {
		switch s.ID {
		case "clusterUp":
			// Delta 1: it proves what it can prove when no image exists yet.
			s.Verify = Verify{Kind: VerifyResultTrue, Field: "result.argocdReady"}
			s.Description = "Create the k3d cluster, install ArgoCD, seed secrets and reconcile the local " +
				"overlay against the checkout's own :local image names."
			steps = append(steps, s)
			// Delta 2: the build, immediately after it.
			steps = append(steps, Step{
				ID:             "buildImages",
				Script:         "k3d.dev",
				TimeoutSeconds: 2700,
				Description: "Build the node images from the checkout, import them into k3d, " +
					"and roll the cluster onto them.",
				Params:      map[string]string{"image-source": "checkout"},
				DependsOn:   []string{"clusterUp"},
				Elevation:   ElevationNone,
				ContainedBy: "clusterUp",
				Verify:      Verify{Kind: VerifyResultEquals, Field: "result.imageSource", Value: "checkout"},
			})
		case "seedBootstrap":
			// Delta 3: the tail waits for the images.
			s.DependsOn = []string{"clusterUp", "buildImages"}
			steps = append(steps, s)
		default:
			steps = append(steps, s)
		}
	}
	out.Steps = steps
	return &out
}

func TestInstallFromMainIsInstallPlusExactlyTheThreeDeltas(t *testing.T) {
	want := applyMainDeltas(t, mustLoadEmbedded(t, Install))
	got := mustLoadEmbedded(t, InstallFromMain)

	if got.Name != want.Name {
		t.Errorf("name = %q, want %q", got.Name, want.Name)
	}
	if got.Kind != want.Kind {
		t.Errorf("kind = %q, want %q -- the from-source lane runs forward and records a receipt "+
			"exactly as install.json does", got.Kind, want.Kind)
	}
	if got.Description != want.Description {
		t.Errorf("description = %q, want %q", got.Description, want.Description)
	}

	if len(got.Steps) != len(want.Steps) {
		t.Fatalf("step count = %d, want %d\n got: %v\nwant: %v",
			len(got.Steps), len(want.Steps), got.IDs(), want.IDs())
	}
	for i := range want.Steps {
		w, g := want.Steps[i], got.Steps[i]
		if !reflect.DeepEqual(w, g) {
			t.Errorf("step %d differs from the derivation.\n got: %+v\nwant: %+v\n\n"+
				"install-main.json is install.json plus three deltas and nothing else. If this is a "+
				"deliberate FOURTH difference, argue it at InstallFromMain and add it to "+
				"applyMainDeltas; if install.json changed, this file needed no edit at all.", i, g, w)
		}
	}
}

// The ordering the whole lane turns on, asserted through TopoOrder rather than
// through the dependsOn list -- an executor runs WAVES, and it is the wave a
// step lands in, not the edge it declares, that decides what it runs beside.
func TestInstallFromMainRunsBuildImagesAfterClusterUpAndBeforeSeedBootstrap(t *testing.T) {
	g := mustLoadEmbedded(t, InstallFromMain)
	waves, err := g.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}

	waveOf := map[string]int{}
	for i, w := range waves {
		for _, id := range w {
			waveOf[id] = i
		}
	}
	for _, id := range []string{"clusterUp", "buildImages", "seedBootstrap"} {
		if _, ok := waveOf[id]; !ok {
			t.Fatalf("step %q is not in any wave", id)
		}
	}

	if waveOf["buildImages"] <= waveOf["clusterUp"] {
		t.Errorf("buildImages is in wave %d and clusterUp in wave %d -- the build imports into a k3d "+
			"cluster, so it cannot start before that cluster exists",
			waveOf["buildImages"], waveOf["clusterUp"])
	}
	if waveOf["seedBootstrap"] <= waveOf["buildImages"] {
		t.Errorf("seedBootstrap is in wave %d and buildImages in wave %d -- equal or earlier means the "+
			"two run CONCURRENTLY, which writes bootstrap values and restarts nodes that are still in "+
			"ImagePullBackOff because nothing has built their images yet",
			waveOf["seedBootstrap"], waveOf["buildImages"])
	}
}

// The build step is the rebuild flow's step, not a second way to do the same
// thing. Tying them here means the capability contract that document already
// satisfies is INHERITED rather than re-asserted -- and that a change to how
// MemQL builds node images from a checkout cannot land in one and not the other.
func TestInstallFromMainBuildsWithTheSameCapabilityContractAsRebuild(t *testing.T) {
	build := mustLoadEmbedded(t, InstallFromMain).Step("buildImages")
	if build == nil {
		t.Fatal("install-main has no buildImages step")
	}
	rebuild := mustLoadEmbedded(t, Rebuild).Step("rebuildFromCheckout")
	if rebuild == nil {
		t.Fatal("rebuild.json has no rebuildFromCheckout step")
	}

	if build.Script != rebuild.Script {
		t.Errorf("buildImages runs %q and rebuildFromCheckout runs %q -- there is one way to build node "+
			"images from a checkout, and a second would drift", build.Script, rebuild.Script)
	}
	if !reflect.DeepEqual(build.Params, rebuild.Params) {
		t.Errorf("buildImages pins %v and rebuildFromCheckout pins %v -- `--image-source=checkout` is "+
			"policy the DOCUMENT states so no caller can turn it into \"build, but keep running released "+
			"images\", and both documents must state it identically", build.Params, rebuild.Params)
	}
	if !reflect.DeepEqual(build.Verify, rebuild.Verify) {
		t.Errorf("buildImages proves %+v and rebuildFromCheckout proves %+v -- same capability, same "+
			"evidence that it did its job", build.Verify, rebuild.Verify)
	}
}

// clusterUp's proof obligation is the delta most likely to be "tidied up" by
// somebody making the two documents look alike, so it gets its own name and its
// own reason.
func TestInstallFromMainDoesNotAskClusterUpToProveWorkloadsItCannotHaveYet(t *testing.T) {
	main := mustLoadEmbedded(t, InstallFromMain).Step("clusterUp")
	release := mustLoadEmbedded(t, Install).Step("clusterUp")

	if release.Verify.Field != "result.workloadsReady" {
		t.Fatalf("install.json's clusterUp now proves %q -- this test compares against it and needs "+
			"updating deliberately", release.Verify.Field)
	}
	if main.Verify.Field != "result.argocdReady" {
		t.Errorf("install-main's clusterUp proves %q, want result.argocdReady -- on this lane the node "+
			"images do not exist until buildImages runs, so requiring workloadsReady here burns the "+
			"full workload budget and then fails every from-source install on a cluster that is fine",
			main.Verify.Field)
	}
}
