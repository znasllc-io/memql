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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// Execute the workflow's real guard against a repository whose main has moved
// beyond the release, rather than testing a second implementation of the rule.
func TestEngineBuildUsesExactReleaseSource(t *testing.T) {
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string            `yaml:"name"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(engineBuildWorkflow(t)), &wf); err != nil {
		t.Fatal(err)
	}
	var inputs, verify string
	checkoutIndex, verifyIndex, loginIndex := -1, -1, -1
	for n, step := range wf.Jobs["build"].Steps {
		switch step.Name {
		case "Check release inputs":
			inputs = step.Run
		case "Checkout":
			checkoutIndex = n
			if step.With["ref"] != "${{ inputs.source_sha }}" {
				t.Fatal("checkout must use the exact release SHA")
			}
		case "Verify release source":
			verify, verifyIndex = step.Run, n
		case "Azure login (OIDC)":
			loginIndex = n
		}
	}
	if inputs == "" || verify == "" || checkoutIndex < 0 || verifyIndex <= checkoutIndex || loginIndex <= verifyIndex {
		t.Fatal("release validation must precede registry authentication")
	}
	bridge := dispatchOnReleaseWorkflow(t)
	for _, want := range []string{"SOURCE_SHA: ${{ github.sha }}", `-f source_sha="$SOURCE_SHA"`, "--ref main"} {
		if !strings.Contains(bridge, want) {
			t.Fatalf("release bridge must forward the event source: missing %s", want)
		}
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "VERSION"), []byte("1.2.3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "VERSION")
	git("commit", "-m", "release")
	release := git("rev-parse", "HEAD")
	git("tag", "v1.2.3")
	git("tag", "-a", "v1.2.4", "-m", "wrong version")
	git("commit", "--allow-empty", "-m", "main advanced")
	later := git("rev-parse", "HEAD")
	git("tag", "v1.2.5")
	git("remote", "add", "origin", repo)
	run := func(script, head, ref, version, sha string, ok bool) {
		t.Helper()
		git("checkout", "--detach", head)
		output := filepath.Join(t.TempDir(), "output")
		cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "WORKFLOW_REF="+ref, "VERSION="+version, "SOURCE_SHA="+sha, "GITHUB_OUTPUT="+output)
		out, err := cmd.CombinedOutput()
		if (err == nil) != ok {
			t.Fatalf("guard(%s,%s,%s) success=%v want %v: %s", ref, version, sha, err == nil, ok, out)
		}
		stamp, _ := os.ReadFile(output)
		if !ok && len(stamp) != 0 {
			t.Fatalf("refusal exported a build stamp: %s", stamp)
		}
		if ok && script == verify && string(stamp) != "sha="+release+"\n" {
			t.Fatalf("wrong source stamp: %s", stamp)
		}
	}
	run(inputs, release, "refs/heads/main", "1.2.3", release, true)
	run(inputs, release, "refs/heads/feature", "1.2.3", release, false)
	run(inputs, release, "refs/heads/main", "v1.2.3", release, false)
	run(inputs, release, "refs/heads/main", "1.2.3", "main", false)
	run(inputs, release, "refs/heads/main", "1.2.3;echo unsafe", release, false)
	run(verify, release, "refs/heads/main", "1.2.3", release, true)
	run(verify, later, "refs/heads/main", "1.2.3", release, false)   // moving main is never the release checkout
	run(verify, later, "refs/heads/main", "1.2.3", later, false)     // tag differs from candidate
	run(verify, release, "refs/heads/main", "1.2.4", release, false) // VERSION differs, including an annotated tag
	run(verify, release, "refs/heads/main", "9.9.9", release, false) // missing tag
	git("tag", "-d", "v1.2.3")
	git("tag", "-a", "v1.2.3", release, "-m", "annotated release")
	run(verify, release, "refs/heads/main", "1.2.3", release, true)
}
