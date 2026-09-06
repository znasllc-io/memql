// Build-speed C3 (#1508, epic #1505): static guard over the engine image-build
// workflow (.github/workflows/build-engine-images.yml). It builds the
// product-agnostic engine images (memql-identity, memql-bff, ...) and pushes them
// to ACR via OIDC; the built engine digests pin directly into a release's
// {engine, bundle, client} deploy overlay (no release lockfile). These string
// assertions keep the workflow's invariants from silently regressing; the real
// build is exercised by a workflow_dispatch on main.
package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func engineBuildWorkflow(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/release/ -> repo root is two directories up.
	p := filepath.Join(filepath.Dir(thisFile), "..", "..", ".github", "workflows", "build-engine-images.yml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read build-engine-images.yml: %v", err)
	}
	return string(raw)
}

func dispatchOnReleaseWorkflow(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "..", "..", ".github", "workflows", "dispatch-engine-images-on-release.yml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read dispatch-engine-images-on-release.yml: %v", err)
	}
	return string(raw)
}

// THE TWO TAG CONVENTIONS MEET HERE, AND NOWHERE ELSE (memql#4061).
//
// Git tags carry the `v` and image tags do not. `clone-stack.sh` checks out
// `v0.19.0`; the extension's `imageTagFor()` strips the prefix, so an install
// pins `ghcr.io/znasllc-io/memql-bff:0.19.0`. Every image that resolves today
// is named that way.
//
// `build-engine-images.yml` uses its `version` input VERBATIM as the tag it
// pushes (asserted below, because that is what makes the stripping this test
// guards necessary rather than decorative). So the release-published dispatch
// is the one place a ref becomes a version, and forwarding `tag_name`
// unchanged builds `memql-bff:v0.19.0` -- a green build, an immutable tag
// burned at a name nothing pulls, and every pod of that release in
// ImagePullBackOff.
//
// It is worth a test rather than a comment because the failure is invisible
// from the workflow run: nothing goes red, and the mismatch only surfaces later
// as a cluster that will not start.
func TestReleaseDispatchStripsTheTagPrefix(t *testing.T) {
	wf := dispatchOnReleaseWorkflow(t)

	if !strings.Contains(wf, "${TAG#v}") {
		t.Error("the release dispatch must strip the leading `v` from the release tag " +
			"before forwarding it as build-engine-images' `version` input " +
			"(git tags carry the v, image tags do not)")
	}
	if strings.Contains(wf, `-f version="$TAG"`) {
		t.Error("the release dispatch forwards the raw release tag as `version`; " +
			"that builds memql-<node>:vX.Y.Z, which no install pulls -- forward the " +
			"stripped value instead")
	}
}

func TestEngineBuildWorkflowUsesTheVersionInputVerbatim(t *testing.T) {
	// The other half of the contract above. If this ever stops being true --
	// if the build learns to normalize the value itself -- then the stripping
	// in the dispatch is no longer what protects the tag, and both places need
	// re-reading together rather than one being "cleaned up" alone.
	wf := engineBuildWorkflow(t)
	if !strings.Contains(wf, "memql-${{ matrix.node }}:${{ inputs.version }}") {
		t.Error("build-engine-images no longer tags images with the raw `version` input; " +
			"re-check TestReleaseDispatchStripsTheTagPrefix, which exists because it did")
	}
}

func TestEngineBuildWorkflowInvariants(t *testing.T) {
	wf := engineBuildWorkflow(t)

	// Builds the engine images the engine-image build owns.
	for _, node := range []string{"identity", "bff", "edge"} {
		if !strings.Contains(wf, "node: "+node) {
			t.Errorf("workflow must build the %q engine node", node)
		}
	}
	// Pushes to the shared ACR.
	if !strings.Contains(wf, "acrmemql") {
		t.Error("workflow must target the acrmemql registry")
	}
	if !strings.Contains(wf, "push: true") {
		t.Error("workflow must push the images (push: true)")
	}
	// OIDC auth (no long-lived secret) -- needs the id-token permission +
	// azure/login.
	if !strings.Contains(wf, "id-token: write") {
		t.Error("workflow must request id-token: write for OIDC")
	}
	if !strings.Contains(wf, "azure/login@") {
		t.Error("workflow must authenticate via azure/login (OIDC)")
	}
	// The OIDC federated-credential subject is ref:refs/heads/main, so the
	// build must run on main -- workflow_dispatch (runs on the default branch).
	if !strings.Contains(wf, "workflow_dispatch:") {
		t.Error("workflow must be workflow_dispatch (the OIDC subject is ref:refs/heads/main)")
	}
	// Release tags stay immutable (mirror the operator's ensure_tag_immutable).
	if !strings.Contains(wf, "Immutability guard") {
		t.Error("workflow must guard against overwriting an existing release tag")
	}
	// The workbench is the one node with a runtime stage of its own -- it runs
	// somebody else's build command, so it needs a Node toolchain the shared
	// distroless runtime does not carry.
	if !strings.Contains(wf, "workbench-runtime") {
		t.Error("the workbench build must target the workbench-runtime stage")
	}
	// EVERY entry names a target. An empty one resolves to the Dockerfile's
	// last stage, which is how released images and locally built ones came to
	// be built from different bases with nothing saying so.
	if strings.Contains(wf, `target: ""`) {
		t.Error("an engine-image matrix entry leaves `target` empty; it must name its runtime stage")
	}
}
