package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// up_retired_node_types_test.go -- memql#5061.
//
// THE DEFECT. The `upgrade` leg of install-cluster-e2e installs the last
// RELEASE and then moves the cluster to the revision under test. It therefore
// runs NEW scripts against an OLD checkout's manifests deliberately -- that is
// what an upgrade is -- and the two halves parted company the moment a node
// type was retired. `47e81134a` removed `voice`; v0.19.1's overlay still
// declares the `voice` and `voice-agent` Deployments, current
// `seed-secrets.sh` no longer creates `livekit-secrets`, and the code that
// used to scale them to 0 went in the same removal. So every branch went red
// at the FIRST step:
//
//	NOT READY: not every workload became Available within 900s:
//	  not ready: voice-...        (CreateContainerConfigError: secret "livekit-secrets" not found)
//	  not ready: voice-agent-...  (CreateContainerConfigError: secret "livekit-secrets" not found)
//
// -- before the upgrade the lane exists to test was attempted, on branches
// whose diff touched neither deploy/k8s/ nor the install flow. A lane that is
// always red is a lane nobody reads, and `upgrade` is the only coverage the
// release-to-release path has.
//
// IT IS THE GENERAL SHAPE, not a voice-specific accident: removing a node type
// removes the seeding the previous release still needs, and the next
// retirement does it again.
//
// THE PROPERTY. The wait covers what the REVISION UNDER TEST is about. A
// Deployment running an engine node type this tree no longer builds belongs to
// the release being upgraded from, and is skipped -- named, not silently.
//
// AND THE OPPOSITE PROPERTY, which is the one that keeps this from becoming
// the false green memql#3570 and memql#3585 were about: the rule keys on the
// IMAGE (`memql-<nodeType>`), so a Deployment that is merely unfamiliar --
// `redis`, `postgres`, `livekit` -- stays in the wait, and so does every node
// type this tree does build.

// retiredFakeKubectl answers `get deployments` for both column shapes the
// script asks for, from $FAKE_DEPLOYMENTS: one `name replicas image` triple per
// line. A `wait` naming a deployment in $FAKE_UNREADY fails.
const retiredFakeKubectl = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_KUBECTL_LOG"
case "$*" in
  *"get deployments"*)
    while read -r name replicas image; do
      [ -n "$name" ] || continue
      case "$*" in
        *IMAGES*)   printf '%s   %s\n' "$name" "$image" ;;
        *REPLICAS*) printf '%s   %s\n' "$name" "$replicas" ;;
        *)          printf 'deployment.apps/%s\n' "$name" ;;
      esac
    done <<< "$FAKE_DEPLOYMENTS"
    exit 0 ;;
  *"get pods"*"-o json"*)
    printf '{"items":[]}\n'
    exit 0 ;;
  *wait*)
    for unready in $FAKE_UNREADY; do
      case "$*" in
        *"/$unready"*)
          printf 'error: timed out waiting for the condition on deployments/%s\n' "$unready" >&2
          exit 1 ;;
      esac
    done
    exit 0 ;;
esac
exit 0
`

// runWaitWithImages sources up.sh and runs wait_for_workloads against a fake
// kubectl that knows each Deployment's image, which is what the retirement rule
// reads.
func runWaitWithImages(t *testing.T, deployments, unready string) (ready, kubectlCalls, log string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(retiredFakeKubectl), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	calls := filepath.Join(tmp, "kubectl.log")

	harness := filepath.Join(tmp, "harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -uo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "up.sh") + "\"\n" +
		"NAMESPACE=memql\n" +
		"wait_for_workloads\n" +
		"printf 'WORKLOADS_READY=%s\\n' \"$WORKLOADS_READY\"\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cmd := exec.Command("bash", harness)
	cmd.Dir = root
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_KUBECTL_LOG="+calls,
		"FAKE_DEPLOYMENTS="+deployments,
		"FAKE_UNREADY="+unready,
		"MEMQL_K3D_WORKLOAD_TIMEOUT=3s",
	)
	out, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); !ok && err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	log = string(out)
	for _, l := range strings.Split(log, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "WORKLOADS_READY="); ok {
			ready = v
		}
	}
	b, readErr := os.ReadFile(calls)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read kubectl log: %v", readErr)
	}
	kubectlCalls = string(b)
	t.Logf("output:\n%s\nkubectl:\n%s", log, kubectlCalls)
	return ready, kubectlCalls, log
}

// The observed failure, reproduced: v0.19.1's two voice Deployments, scaled up,
// never Available, on a tree that no longer builds a `voice` node type.
func TestWaitForWorkloadsSkipsARetiredNodeType(t *testing.T) {
	deployments := "bff 2 acrmemql.azurecr.io/memql-bff:0.9.9\n" +
		"identity 2 acrmemql.azurecr.io/memql-identity:0.9.9\n" +
		"voice 1 acrmemql.azurecr.io/memql-voice:0.9.9\n" +
		"voice-agent 1 acrmemql.azurecr.io/memql-voice:0.9.9\n"
	ready, calls, log := runWaitWithImages(t, deployments, "voice voice-agent")

	if ready != "true" {
		t.Fatalf("WORKLOADS_READY = %q, want true -- every node type this tree BUILDS was\n"+
			"Available; the two that were not run `memql-voice`, which it does not\noutput:\n%s",
			ready, log)
	}
	for _, gone := range []string{"deployment.apps/voice", "deployment.apps/voice-agent"} {
		for _, line := range strings.Split(calls, "\n") {
			if strings.Contains(line, "wait") && strings.Contains(line, gone) {
				t.Errorf("the wait was asked for %s, a node type this tree no longer builds:\n  %s", gone, line)
			}
		}
	}
}

// SAY WHAT IS BEING LEFT OUT. A wait that narrows itself and never explains is
// how a false green starts -- the operator reads a pass and never learns that
// two workloads were excluded from it.
func TestWaitForWorkloadsNamesWhatItSkipped(t *testing.T) {
	deployments := "bff 2 acrmemql.azurecr.io/memql-bff:0.9.9\n" +
		"voice 1 acrmemql.azurecr.io/memql-voice:0.9.9\n"
	_, _, log := runWaitWithImages(t, deployments, "voice")

	if !strings.Contains(log, "voice") || !strings.Contains(log, "no longer builds") {
		t.Fatalf("the run never said it had stopped waiting for `voice`, or why.\n"+
			"An unexplained narrowing is indistinguishable from a pass:\noutput:\n%s", log)
	}
}

// THE FALSE-GREEN GUARD, and the reason the rule reads the image rather than
// the name. `redis` and `postgres` are not node types in ANY release, so
// "not a node type this tree builds" would exclude them -- dropping real
// infrastructure out of the wait, which is memql#3570 exactly.
func TestWaitForWorkloadsStillWaitsForInfrastructureItNeverBuilt(t *testing.T) {
	deployments := "bff 2 acrmemql.azurecr.io/memql-bff:0.9.9\n" +
		"redis 1 redis:7-alpine\n"
	ready, _, log := runWaitWithImages(t, deployments, "redis")

	if ready != "false" {
		t.Fatalf("WORKLOADS_READY = %q, want false -- `redis` is not an engine node type at all,\n"+
			"so it is infrastructure this cluster needs and its failure is real\noutput:\n%s", ready, log)
	}
}

// And a node type this tree DOES build stays in the wait even when it is
// broken. That is the whole verdict.
func TestWaitForWorkloadsStillWaitsForANodeTypeThisTreeBuilds(t *testing.T) {
	deployments := "bff 2 acrmemql.azurecr.io/memql-bff:0.9.9\n" +
		"edge 1 acrmemql.azurecr.io/memql-edge:0.9.9\n"
	ready, _, log := runWaitWithImages(t, deployments, "edge")

	if ready != "false" {
		t.Fatalf("WORKLOADS_READY = %q, want false -- `edge` is in ENGINE_NODE_TYPES, so this\n"+
			"tree builds it and its failure is the revision under test's own\noutput:\n%s", ready, log)
	}
}

// A namespace holding ONLY retired node types is not a healthy one, and the
// reason it reports has to say retirement rather than "scaled to 0" -- which is
// a different fact and would send the reader to the wrong overlay.
func TestWaitForWorkloadsDoesNotReadAnAllRetiredNamespaceAsScaledDown(t *testing.T) {
	deployments := "voice 1 acrmemql.azurecr.io/memql-voice:0.9.9\n"
	ready, _, log := runWaitWithImages(t, deployments, "voice")

	if ready != "false" {
		t.Fatalf("WORKLOADS_READY = %q, want false -- nothing under test is running\noutput:\n%s", ready, log)
	}
	if strings.Contains(log, "scaled to 0") {
		t.Errorf("the reason blamed replica counts for a namespace that holds only retired\n"+
			"node types; nothing here is scaled to 0:\noutput:\n%s", log)
	}
}
