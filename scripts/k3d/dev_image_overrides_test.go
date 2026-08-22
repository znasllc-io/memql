package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// filter_node_image_overrides drops the override of every node THIS RUN built
// and keeps everything else -- the database operand, and any node the run did
// not touch. Under --image-source=checkout a rebuilt node must resolve to the
// overlay's own :local image, and a node nobody rebuilt must keep pointing at
// the released image that is actually in the cluster.
//
// ITS INPUT IS ONE ENTRY PER LINE -- kubectl's `{range ...}{@}{"\n"}{end}`
// rendering, which every kubectl version produces identically. It used to be
// the bare array node, whose rendering is version-dependent (JSON on 1.36, Go's
// `[a b]` on older ones); the tests below pin the line form and pin that the
// old form is REFUSED rather than mis-split.
func runFilter(t *testing.T, input string, nodes ...string) string {
	t.Helper()
	root := repoRoot(t)
	harness := filepath.Join(t.TempDir(), "harness.sh")
	body := "set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"filter_node_image_overrides '" + input + "' " + strings.Join(nodes, " ") + "\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", harness).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

const operandOverride = "memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1"

func TestFilterNodeImageOverridesKeepsTheOperandOnly(t *testing.T) {
	in := "memql-bff=ghcr.io/znasllc-io/memql-bff:v0.17.0\n" +
		operandOverride + "\n" +
		"memql-agent=ghcr.io/znasllc-io/memql-agent:v0.17.0\n"
	if got, want := runFilter(t, in, "bff", "agent"), `["`+operandOverride+`"]`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

// The operand is matched on the image BASENAME, so a registry-qualified
// override name is protected too -- that is the form an install writes.
func TestFilterNodeImageOverridesKeepsARegistryQualifiedOperand(t *testing.T) {
	const qualified = "ghcr.io/znasllc-io/memql-db=ghcr.io/znasllc-io/memql-db:16.15-timescaledb-2.29.1"
	in := "ghcr.io/znasllc-io/memql-bff=ghcr.io/znasllc-io/memql-bff:v0.17.0\n" + qualified + "\n"
	if got, want := runFilter(t, in, "bff"), `["`+qualified+`"]`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
	}
}

func TestFilterNodeImageOverridesIsIdempotent(t *testing.T) {
	if got, want := runFilter(t, operandOverride+"\n", "bff"), `["`+operandOverride+`"]`; got != want {
		t.Errorf("filter changed an already-filtered list: %s, want %s", got, want)
	}
	if got := runFilter(t, "", "bff"); got != "[]" {
		t.Errorf("filter of no list = %s", got)
	}
	if got := runFilter(t, "\n", "bff"); got != "[]" {
		t.Errorf("filter of a blank line = %s", got)
	}
}

// THE CRITICAL ONE, at the unit. A node the run did NOT build keeps its
// override: its :local image was never imported, so dropping the override
// points the Deployment at an image that does not exist in the cluster, and
// with imagePullPolicy IfNotPresent that is ImagePullBackOff.
func TestFilterNodeImageOverridesKeepsNodesItDidNotBuild(t *testing.T) {
	const bff = "memql-bff=ghcr.io/znasllc-io/memql-bff:v0.17.0"
	const agent = "memql-agent=ghcr.io/znasllc-io/memql-agent:v0.17.0"
	in := bff + "\n" + agent + "\n" + operandOverride + "\n"
	if got, want := runFilter(t, in, "bff"), `["`+agent+`","`+operandOverride+`"]`; got != want {
		t.Errorf("filter = %s, want %s", got, want)
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

// fakeKubectlApplication answers every read point_application_at_local_images
// makes, each one separately -- which is the point. A fake that served one
// canned answer to every `get application` handed the image list to the
// sync-status read too, so the patch -> annotate -> Synced path could never
// complete and was never exercised.
//
// THE OVERRIDE LIST IS RENDERED THE WAY `-o jsonpath={range ...}{@}{"\n"}{end}`
// RENDERS IT: one entry per line, no brackets, no quotes. Encoding the real
// rendering here is what makes these tests evidence about kubectl rather than
// about a shape we invented.
const fakeKubectlApplication = `#!/usr/bin/env bash
case "$*" in
  *"get application"*"-o name"*)
    [ "${FAKE_APP_EXISTS:-1}" = "1" ] || exit 1
    printf 'application.argoproj.io/memql-local\n'
    exit 0 ;;
  *"get application"*kustomize.images*)
    [ "${FAKE_IMAGES_READ_FAILS:-0}" = "1" ] && exit 1
    [ -n "${FAKE_IMAGES:-}" ] && printf '%s\n' "$FAKE_IMAGES"
    exit 0 ;;
  *"get application"*status.sync.status*)
    printf '%s' "${FAKE_SYNC_STATUS:-Synced}"
    exit 0 ;;
  *"patch application"*)
    printf '%s\n' "$*" >> "$FAKE_PATCH_LOG"
    exit 0 ;;
  *"rollout restart"*)
    printf '%s\n' "$*" >> "$FAKE_RESTART_LOG"
    exit 0 ;;
esac
exit 0
`

// pointAppFake drives fakeKubectlApplication.
type pointAppFake struct {
	appExists bool
	images    string   // newline-separated, as kubectl's range form renders
	nodes     []string // the nodes this run built (default: bff)
	readFails bool     // the override-list read itself fails
	syncod    string   // .status.sync.status (default "Synced")
}

// runPointApplication sources dev.sh and calls point_application_at_local_images
// against the fake, returning the combined output, the exit code, and every
// `kubectl patch` argv the run issued (empty when it never patched).
func runPointApplication(t *testing.T, f pointAppFake) (string, int, []string) {
	t.Helper()
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(fakeKubectlApplication), 0o755); err != nil {
		t.Fatal(err)
	}
	built := f.nodes
	if len(built) == 0 {
		built = []string{"bff"}
	}
	patchLog := filepath.Join(tmp, "patches")
	harness := filepath.Join(tmp, "harness.sh")
	body := "set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"APP_NAME=memql-local\n" +
		"IMAGE_SOURCE=checkout\n" +
		// checkout_facts runs before the first image is built, so these are
		// known long before the patch -- which is the point of emitting them
		// there rather than in main()'s tail.
		"CHECKOUT_COMMIT=abc1234def5678\nCHECKOUT_REF=branch:main\n" +
		// A regression that reaches the sync wait must FAIL, not hang: against a
		// fake that never answers "Synced" the real 300s budget would stall the
		// package for five minutes and report a timeout naming nothing.
		"function sleep() { :; }\n" +
		"point_application_at_local_images " + strings.Join(built, " ") + "\n" +
		"echo \"OVERRIDES_PATCHED=$OVERRIDES_PATCHED\"\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	exists, readFails := "0", "0"
	if f.appExists {
		exists = "1"
	}
	if f.readFails {
		readFails = "1"
	}
	cmd := exec.Command("bash", harness)
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_APP_EXISTS="+exists,
		"FAKE_IMAGES="+f.images,
		"FAKE_IMAGES_READ_FAILS="+readFails,
		"FAKE_SYNC_STATUS="+f.syncod,
		"FAKE_PATCH_LOG="+patchLog,
		"FAKE_RESTART_LOG="+filepath.Join(tmp, "restarts"),
		"MEMQL_K3D_SYNC_TIMEOUT=1",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	var patches []string
	if raw, rerr := os.ReadFile(patchLog); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line != "" {
				patches = append(patches, line)
			}
		}
	}
	return string(out), code, patches
}

// THE SILENT FAILURE THIS CLOSES. `kubectl get application` on an Application
// that does not exist and one that carries no image overrides both render as an
// empty string, so a typo'd --app-name used to log "no overrides -- the
// overlay's :local images already apply", return 0, and let the run report
// imageSource=checkout. That satisfies the graph's verify while the pods stay on
// the released images the rebuild existed to replace.
func TestPointApplicationRefusesAMissingApplication(t *testing.T) {
	out, code, patches := runPointApplication(t, pointAppFake{images: "memql-bff=ghcr.io/x/memql-bff:v1"})
	if code != 4 {
		t.Errorf("exit = %d, want 4 (prerequisite missing):\n%s", code, out)
	}
	for _, want := range []string{"memql-local", "argocd", "--app-name"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q -- an operator has to guess which name was wrong:\n%s", want, out)
		}
	}
	if len(patches) != 0 {
		t.Errorf("patched an Application that does not exist: %v", patches)
	}
}

// The other direction, so the probe is not simply refusing everything: an
// Application that EXISTS and carries no overrides is the ordinary `make up`
// cluster, and it must pass straight through.
func TestPointApplicationAcceptsAnApplicationWithNoOverrides(t *testing.T) {
	out, code, patches := runPointApplication(t, pointAppFake{appExists: true})
	if code != 0 {
		t.Errorf("exit = %d, want 0:\n%s", code, out)
	}
	if !strings.Contains(out, "No image overrides") {
		t.Errorf("output does not report the no-override case:\n%s", out)
	}
	if len(patches) != 0 {
		t.Errorf("patched an Application that had nothing to patch: %v", patches)
	}
}

// THE WHOLE POINT OF THE CAPABILITY, end to end through the function: a
// wizard-installed Application pinned to released images is patched to exactly
// the operand-only list, annotated, and waited to Synced.
func TestPointApplicationPatchesAReleasedApplicationToTheOperandOnly(t *testing.T) {
	images := "memql-bff=ghcr.io/znasllc-io/memql-bff:v0.17.0\n" +
		"memql-agent=ghcr.io/znasllc-io/memql-agent:v0.17.0\n" +
		operandOverride
	out, code, patches := runPointApplication(t, pointAppFake{
		appExists: true, images: images, nodes: []string{"bff", "agent"},
	})
	if code != 0 {
		t.Errorf("exit = %d, want 0:\n%s", code, out)
	}
	if len(patches) != 1 {
		t.Fatalf("want exactly one patch, got %d: %v\n%s", len(patches), patches, out)
	}
	want := `{"spec":{"source":{"kustomize":{"images":["` + operandOverride + `"]}}}}`
	if !strings.Contains(patches[0], want) {
		t.Errorf("patch payload = %s\nwant it to carry %s", patches[0], want)
	}
	for _, gone := range []string{"memql-bff", "memql-agent"} {
		if strings.Contains(patches[0], gone) {
			t.Errorf("the patch still carries the %s node override: %s", gone, patches[0])
		}
	}
	if !strings.Contains(out, "OVERRIDES_PATCHED=true") {
		t.Errorf("the run did not record that it patched:\n%s", out)
	}
	if !strings.Contains(out, "is Synced") {
		t.Errorf("the run did not reach the Synced gate -- the patch -> annotate -> Synced path was not exercised:\n%s", out)
	}
}

// THE DESTRUCTIVE FAILURE THIS EXISTS FOR. An older kubectl renders a bare
// string array as Go's `[a b]` rather than JSON. The previous reader
// comma-split that text, so the whole rendering became ONE entry, nothing
// matched the operand, the filtered list came out empty -- and the patch would
// have removed the DATABASE OPERAND override along with the nodes (memql#4063),
// reporting exit 0. An unrecognised shape must never become a patch.
func TestPointApplicationRefusesTheGoArrayRenderingRatherThanMisSplittingIt(t *testing.T) {
	goArray := "[memql-bff=ghcr.io/x/memql-bff:v1 " + operandOverride + "]"
	out, code, patches := runPointApplication(t, pointAppFake{appExists: true, images: goArray})
	if code != 5 {
		t.Errorf("exit = %d, want 5 (operation failed):\n%s", code, out)
	}
	if len(patches) != 0 {
		t.Fatalf("PATCHED on an unparseable list -- this is the destructive case: %v\n%s", patches, out)
	}
	if !strings.Contains(out, goArray) {
		t.Errorf("the refusal does not quote the text it could not parse:\n%s", out)
	}
}

// --------------------------------------------------------------------------
// a PARTIAL rebuild keeps the overrides it did not build (memql#4245, CRITICAL)
// --------------------------------------------------------------------------

// allAppNodes is DEFAULT_APP_NODES: what a wizard install writes an override
// for, and what a bare `make dev` rebuilds.
var allAppNodes = []string{"identity", "bff", "voice", "mcp", "cognition", "agent", "planner", "workbench", "edge"}

// releasedOverrides is the override list `k3d.up --image-registry/--image-tag`
// leaves on a wizard-installed Application: one per node type, plus the operand.
func releasedOverrides() string {
	lines := make([]string, 0, len(allAppNodes)+1)
	for _, n := range allAppNodes {
		lines = append(lines, "memql-"+n+"=ghcr.io/znasllc-io/memql-"+n+":v0.17.0")
	}
	return strings.Join(append(lines, operandOverride), "\n")
}

// THE CRITICAL DEFECT. `--node=bff --image-source=checkout` built and imported
// ONE image and removed ALL NINE overrides, leaving eight Deployments naming
// memql-<node>:local images that were never imported -- ImagePullBackOff under
// imagePullPolicy IfNotPresent -- while the run exited 0 with
// imageSource=checkout, the verify passed, and the toast said the cluster runs
// your checkout. The image wait could not catch it either: it only waits on the
// nodes that WERE built.
//
// The Nodes field on the rebuild screen hints "For example: bff, agent", so
// this is the invited path, not an exotic one.
func TestPointApplicationKeepsTheOverridesOfNodesItDidNotBuild(t *testing.T) {
	out, code, patches := runPointApplication(t, pointAppFake{
		appExists: true, images: releasedOverrides(), nodes: []string{"bff"},
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if len(patches) != 1 {
		t.Fatalf("want exactly one patch, got %d: %v\n%s", len(patches), patches, out)
	}
	if strings.Contains(patches[0], "memql-bff=") {
		t.Errorf("the rebuilt node kept its released override -- it must move to :local: %s", patches[0])
	}
	// Every node NOT rebuilt keeps its released override, and so does the operand.
	for _, n := range allAppNodes {
		if n == "bff" {
			continue
		}
		if !strings.Contains(patches[0], "memql-"+n+"=") {
			t.Errorf("the patch dropped the %s override although this run never built %s -- "+
				"its :local image was never imported, so the Deployment lands in ImagePullBackOff: %s",
				n, n, patches[0])
		}
	}
	if !strings.Contains(patches[0], operandOverride) {
		t.Errorf("the patch dropped the database operand override: %s", patches[0])
	}
}

// The whole-cluster rebuild is the other end of the same rule: build all nine
// and every node override goes, leaving exactly the operand.
func TestPointApplicationDropsEveryOverrideWhenEveryNodeWasBuilt(t *testing.T) {
	out, code, patches := runPointApplication(t, pointAppFake{
		appExists: true, images: releasedOverrides(), nodes: allAppNodes,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if len(patches) != 1 {
		t.Fatalf("want exactly one patch, got %d: %v\n%s", len(patches), patches, out)
	}
	want := `{"spec":{"source":{"kustomize":{"images":["` + operandOverride + `"]}}}}`
	if !strings.Contains(patches[0], want) {
		t.Errorf("patch payload = %s\nwant it to carry exactly %s", patches[0], want)
	}
}

// Nothing this run built is overridden, so there is nothing to patch -- and
// patching anyway would rewrite the list for no reason.
func TestPointApplicationPatchesNothingWhenTheBuiltNodeHasNoOverride(t *testing.T) {
	images := "memql-bff=ghcr.io/znasllc-io/memql-bff:v0.17.0\n" + operandOverride
	out, code, patches := runPointApplication(t, pointAppFake{
		appExists: true, images: images, nodes: []string{"agent"},
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if len(patches) != 0 {
		t.Errorf("patched although no node this run built is overridden: %v\n%s", patches, out)
	}
	if !strings.Contains(out, "nothing to patch") {
		t.Errorf("output does not say there was nothing to patch:\n%s", out)
	}
}

// A FAILED read is not an empty list. `|| true` on the read turned a kubectl
// that errored into "no overrides -- the overlay's :local images already
// apply", i.e. a success envelope over a cluster nobody looked at.
func TestPointApplicationRefusesAFailedOverrideRead(t *testing.T) {
	out, code, patches := runPointApplication(t, pointAppFake{appExists: true, readFails: true})
	if code != 5 {
		t.Errorf("exit = %d, want 5:\n%s", code, out)
	}
	if !strings.Contains(out, "could not read the image overrides") {
		t.Errorf("the failure does not say the read failed:\n%s", out)
	}
	if len(patches) != 0 {
		t.Errorf("patched after a failed read: %v", patches)
	}
}

// A run that PATCHED and then failed must still say which lane the Application
// is in (memql#4245). dev.sh set its result fields only in main()'s tail, so a
// failure between the patch and there emitted `result:{}` -- and the editor,
// finding no rebuild entry, read the lane off the older clusterUp entry and
// showed a released version for a cluster whose Application was already patched
// and whose pods were converging onto :local. The envelope a failure carries is
// the only record of a crossing that already happened.
func TestPointApplicationRecordsTheLaneEvenWhenTheSyncWaitFails(t *testing.T) {
	out, code, patches := runPointApplication(t, pointAppFake{
		appExists: true, images: releasedOverrides(), nodes: allAppNodes,
		syncod: "OutOfSync",
	})
	if code != 5 {
		t.Fatalf("exit = %d, want 5 (the sync never converged):\n%s", code, out)
	}
	if len(patches) != 1 {
		t.Fatalf("want the patch to have happened, got %d: %v", len(patches), patches)
	}
	// All five, not just the lane: a rebuild entry that says `checkout` with an
	// empty commit/ref/nodes renders as "checkout " and "...from the checkout
	// at ." -- a row that names a lane it cannot describe.
	for _, want := range []string{
		`"imageSource":"checkout"`,
		`"overridesPatched":true`,
		`"commit":"abc1234def5678"`,
		`"ref":"branch:main"`,
		`"nodes":"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure envelope does not carry %s -- the Application is patched and "+
				"nothing records it:\n%s", want, out)
		}
	}
	// Emitted once each: cap_result_set appends, so main()'s tail skips all five.
	for _, key := range []string{`"imageSource"`, `"overridesPatched"`, `"commit"`, `"ref"`, `"nodes"`} {
		if n := strings.Count(out, key); n != 1 {
			t.Errorf("%s appears %d times in the envelope, want 1:\n%s", key, n, out)
		}
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

// --------------------------------------------------------------------------
// the built nodes the patch did not move must still be restarted
// --------------------------------------------------------------------------

// runPatchThenRestart runs the two steps main() runs in the patched branch --
// point_application_at_local_images, then restart_nodes_the_patch_did_not_move
// -- and reports which Deployments were patched and which were restarted.
func runPatchThenRestart(t *testing.T, images string, built ...string) (out string, patches, restarts []string) {
	t.Helper()
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(fakeKubectlApplication), 0o755); err != nil {
		t.Fatal(err)
	}
	patchLog, restartLog := filepath.Join(tmp, "patches"), filepath.Join(tmp, "restarts")
	harness := filepath.Join(tmp, "harness.sh")
	body := "set -euo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"APP_NAME=memql-local\nNAMESPACE=memql\nIMAGE_SOURCE=checkout\n" +
		"function sleep() { :; }\n" +
		"point_application_at_local_images " + strings.Join(built, " ") + "\n" +
		"restart_nodes_the_patch_did_not_move " + strings.Join(built, " ") + "\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", harness)
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_APP_EXISTS=1",
		"FAKE_IMAGES="+images,
		"FAKE_IMAGES_READ_FAILS=0",
		"FAKE_SYNC_STATUS=Synced",
		"FAKE_PATCH_LOG="+patchLog,
		"FAKE_RESTART_LOG="+restartLog,
		"MEMQL_K3D_SYNC_TIMEOUT=1",
	)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, b)
	}
	read := func(path string) []string {
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}
	return string(b), read(patchLog), read(restartLog)
}

// THE MIXED CASE. Rebuild bff alone, and its override is dropped -- so on the
// NEXT rebuild of bff and agent, only agent still has an override to drop.
// ArgoCD rolls a Deployment when its image REF changes, and bff's ref is
// already memql-bff:local, so the sync rolls nothing for bff and the image
// wait passes on its first poll against a ref that was already right. Without
// an explicit restart the bff pod keeps serving the PREVIOUS rebuild's image,
// and the run reports success.
func TestPatchedRunRestartsTheBuiltNodesItDidNotMove(t *testing.T) {
	images := "memql-agent=ghcr.io/znasllc-io/memql-agent:v0.17.0\n" + operandOverride
	out, patches, restarts := runPatchThenRestart(t, images, "bff", "agent")

	if len(patches) != 1 {
		t.Fatalf("want one patch, got %d: %v\n%s", len(patches), patches, out)
	}
	if strings.Contains(patches[0], "memql-agent=") {
		t.Errorf("agent was built and overridden, so its override must go: %s", patches[0])
	}
	if !strings.Contains(patches[0], operandOverride) {
		t.Errorf("the patch dropped the database operand override: %s", patches[0])
	}

	// agent's ref changed, so the sync rolls it -- restarting it too would be a
	// second, redundant roll.
	for _, r := range restarts {
		if strings.Contains(r, "deployment/agent") {
			t.Errorf("agent's override was dropped, so ArgoCD rolls it; restarting is redundant: %v", restarts)
		}
	}
	// bff's ref did not change, so nothing else will ever roll it.
	found := false
	for _, r := range restarts {
		if strings.Contains(r, "deployment/bff") {
			found = true
		}
	}
	if !found {
		t.Errorf("bff was rebuilt but had no override to drop, so its image REF did not change and "+
			"the sync rolls nothing -- without a restart the pod keeps running the previous "+
			"rebuild's image. restarts=%v\n%s", restarts, out)
	}
}

// The whole-cluster case must not gain redundant restarts: every built node's
// override was dropped, so ArgoCD's sync rolls all of them.
func TestPatchedRunRestartsNothingWhenEveryBuiltNodeMoved(t *testing.T) {
	_, patches, restarts := runPatchThenRestart(t, releasedOverrides(), allAppNodes...)
	if len(patches) != 1 {
		t.Fatalf("want one patch, got %d: %v", len(patches), patches)
	}
	if len(restarts) != 0 {
		t.Errorf("every built node's ref changed, so the sync rolls them all; these restarts are "+
			"redundant rolls: %v", restarts)
	}
}
