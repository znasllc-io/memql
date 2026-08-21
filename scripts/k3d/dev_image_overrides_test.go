package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// filter_node_image_overrides keeps the database operand's override and drops
// every node override, because under --image-source=checkout the nodes must
// resolve to the overlay's own :local images while the operand must not roll.
func runFilter(t *testing.T, input string) string {
	t.Helper()
	root := repoRoot(t)
	harness := filepath.Join(t.TempDir(), "harness.sh")
	body := "set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"filter_node_image_overrides '" + input + "'\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestFilterNodeImageOverridesKeepsTheOperandOnly(t *testing.T) {
	in := `["memql-bff=ghcr.io/znasllc-io/memql-bff:v0.17.0","memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1","memql-agent=ghcr.io/znasllc-io/memql-agent:v0.17.0"]`
	if got, want := runFilter(t, in), `["memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1"]`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

func TestFilterNodeImageOverridesIsIdempotent(t *testing.T) {
	in := `["memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1"]`
	if got := runFilter(t, in); got != in {
		t.Errorf("filter changed an already-filtered list: %s", got)
	}
	if got := runFilter(t, "[]"); got != "[]" {
		t.Errorf("filter of an empty list = %s", got)
	}
	if got := runFilter(t, ""); got != "[]" {
		t.Errorf("filter of no list = %s", got)
	}
}

func TestDevPrintSpecDeclaresTheRebuildFlags(t *testing.T) {
	out, err := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "k3d", "dev.sh"), "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec: %v\n%s", err, out)
	}
	for _, flag := range []string{`"name":"repo-root"`, `"name":"app-name"`, `"name":"image-source"`} {
		if !strings.Contains(string(out), flag) {
			t.Errorf("--print-spec lacks %s:\n%s", flag, out)
		}
	}
}

// --------------------------------------------------------------------------
// the Application must EXIST before its overrides are read
// --------------------------------------------------------------------------

// fakeKubectlApplication answers the two reads point_application_at_local_images
// makes. $FAKE_APP_EXISTS=0 makes the existence probe fail the way kubectl does
// for an Application that is not there; $FAKE_IMAGES is the override list the
// jsonpath read returns.
const fakeKubectlApplication = `#!/usr/bin/env bash
case "$*" in
  *"get application"*"-o name"*)
    [ "${FAKE_APP_EXISTS:-1}" = "1" ] || exit 1
    printf 'application.argoproj.io/memql-local\n'
    exit 0 ;;
  *"get application"*)
    printf '%s' "${FAKE_IMAGES:-[]}"
    exit 0 ;;
esac
exit 0
`

// runPointApplication sources dev.sh and calls point_application_at_local_images
// against a fake kubectl, returning the combined output and the exit code.
func runPointApplication(t *testing.T, appExists bool, images string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(fakeKubectlApplication), 0o755); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(tmp, "harness.sh")
	body := "set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"APP_NAME=memql-local\n" +
		// A regression that reaches the sync wait must FAIL, not hang: against a
		// fake that never answers "Synced" the real 300s budget would stall the
		// package for five minutes and report a timeout naming nothing.
		"function sleep() { :; }\n" +
		"point_application_at_local_images\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	exists := "0"
	if appExists {
		exists = "1"
	}
	cmd := exec.Command("bash", harness)
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_APP_EXISTS="+exists,
		"FAKE_IMAGES="+images,
		"MEMQL_K3D_SYNC_TIMEOUT=1",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return string(out), code
}

// THE SILENT FAILURE THIS CLOSES. `kubectl get application` on an Application
// that does not exist and one that carries no image overrides both render as an
// empty string, so a typo'd --app-name used to log "no overrides -- the
// overlay's :local images already apply", return 0, and let the run report
// imageSource=checkout. That satisfies the graph's verify while the pods stay on
// the released images the rebuild existed to replace.
func TestPointApplicationRefusesAMissingApplication(t *testing.T) {
	out, code := runPointApplication(t, false, `["memql-bff=ghcr.io/x/memql-bff:v1"]`)
	if code != 4 {
		t.Errorf("exit = %d, want 4 (prerequisite missing):\n%s", code, out)
	}
	for _, want := range []string{"memql-local", "argocd", "--app-name"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q -- an operator has to guess which name was wrong:\n%s", want, out)
		}
	}
}

// The other direction, so the probe is not simply refusing everything: an
// Application that EXISTS and carries no overrides is the ordinary `make up`
// cluster, and it must pass straight through.
func TestPointApplicationAcceptsAnApplicationWithNoOverrides(t *testing.T) {
	out, code := runPointApplication(t, true, "[]")
	if code != 0 {
		t.Errorf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "No image overrides") {
		t.Errorf("output does not report the no-override case:\n%s", out)
	}
}

// --------------------------------------------------------------------------
// the pods must actually name the locally built images
// --------------------------------------------------------------------------

func runEveryImageIs(t *testing.T, images, want string) bool {
	t.Helper()
	root := repoRoot(t)
	harness := filepath.Join(t.TempDir(), "harness.sh")
	body := "set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"if every_image_is '" + images + "' '" + want + "'; then echo YES; else echo NO; fi\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out)) == "YES"
}

// The predicate wait_for_local_images polls on. An EMPTY list must not read as
// "every image matches": the jsonpath read is `|| true`-guarded, so a kubectl
// that failed hands back "" -- and a vacuous truth there would end the wait on
// the strength of a read that never happened.
func TestEveryImageIsRequiresEveryContainerToMatch(t *testing.T) {
	const want = "memql-bff:local"
	cases := []struct {
		images string
		match  bool
	}{
		{"memql-bff:local", true},
		{"memql-bff:local memql-bff:local", true},
		{"ghcr.io/znasllc-io/memql-bff:v0.17.0", false},
		{"memql-bff:local ghcr.io/znasllc-io/memql-bff:v0.17.0", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := runEveryImageIs(t, tc.images, want); got != tc.match {
			t.Errorf("every_image_is(%q, %q) = %v, want %v", tc.images, want, got, tc.match)
		}
	}
}

// fakeKubectlDeployment answers the two reads wait_for_local_images makes:
// the existence check, and the jsonpath read of the container images.
const fakeKubectlDeployment = `#!/usr/bin/env bash
case "$*" in
  *"get deployment"*jsonpath*)
    printf '%s' "${FAKE_DEPLOY_IMAGES:-}"
    exit 0 ;;
  *"get deployment"*)
    [ "${FAKE_DEPLOY_EXISTS:-1}" = "1" ] || exit 1
    printf 'deployment.apps/bff\n'
    exit 0 ;;
esac
exit 0
`

// runWaitForLocalImages sources dev.sh and calls wait_for_local_images for the
// bff node against a fake kubectl reporting the given container images.
func runWaitForLocalImages(t *testing.T, deployExists bool, images string) (string, int) {
	t.Helper()
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(fakeKubectlDeployment), 0o755); err != nil {
		t.Fatal(err)
	}
	harness := filepath.Join(tmp, "harness.sh")
	body := "set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"NAMESPACE=memql\n" +
		"function sleep() { :; }\n" +
		"wait_for_local_images bff\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	exists := "0"
	if deployExists {
		exists = "1"
	}
	cmd := exec.Command("bash", harness)
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_DEPLOY_EXISTS="+exists,
		"FAKE_DEPLOY_IMAGES="+images,
		"MEMQL_K3D_SYNC_TIMEOUT=1",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return string(out), code
}

// The success direction: the pods name what was built, so the wait ends.
func TestWaitForLocalImagesPassesWhenThePodsNameTheLocalImage(t *testing.T) {
	out, code := runWaitForLocalImages(t, true, "memql-bff:local")
	if code != 0 {
		t.Errorf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "bff names memql-bff:local") {
		t.Errorf("output does not confirm the image ref:\n%s", out)
	}
}

// THE REGRESSION THIS GUARDS. A `Synced` read can be ArgoCD's PREVIOUS answer,
// taken before the refresh landed -- so a run could report success with the
// Deployment still pinned to the released image the rebuild replaced. The wait
// must fail, naming the deployment and what it actually saw.
func TestWaitForLocalImagesFailsWhileThePodsStillNameTheReleasedImage(t *testing.T) {
	out, code := runWaitForLocalImages(t, true, "ghcr.io/znasllc-io/memql-bff:v0.17.0")
	if code != 5 {
		t.Errorf("exit = %d, want 5 (operation failed):\n%s", code, out)
	}
	for _, want := range []string{"bff", "ghcr.io/znasllc-io/memql-bff:v0.17.0", "memql-bff:local"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not mention %q:\n%s", want, out)
		}
	}
}

// A node whose Deployment the cluster does not carry is skipped, not failed --
// the image was still imported, and `make dev NODE=<x>` on a cluster that does
// not run x is an ordinary thing to do.
func TestWaitForLocalImagesSkipsAnAbsentDeployment(t *testing.T) {
	out, code := runWaitForLocalImages(t, false, "")
	if code != 0 {
		t.Errorf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "nothing to wait for") {
		t.Errorf("output does not report the skip:\n%s", out)
	}
}
