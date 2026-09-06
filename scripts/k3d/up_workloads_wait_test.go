package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// up_workloads_wait_test.go -- znasllc-io/memql#3585.
//
// `k3d.up` waits for the MemQL Deployments to become Available and reports the
// verdict as `workloadsReady`, which the install graph verifies (memql#3570).
// The wait was asked to wait for EVERY Deployment in the namespace, including
// ones an overlay deliberately holds at zero replicas. So on any cluster with a
// module switched off the wait could not succeed and clusterUp failed on a
// cluster that was otherwise completely healthy.
//
// The property asserted here: the wait covers what is MEANT to be running --
// Deployments at zero replicas are not waited for. Asking the cluster what is
// scaled up is a fact rather than an assumption about what ran first.
//
// up.sh cannot be driven end-to-end from a test -- it creates clusters and
// installs ArgoCD -- so it is sourced and the wait is exercised against a fake
// kubectl, the same seam bringup_front_door_tls_test.go uses.

// upFakeKubectl answers `get deployments` from $FAKE_DEPLOYMENTS (one
// "name replicas" pair per line) and records every `wait` it is asked to
// perform. A wait naming a deployment listed in $FAKE_UNREADY fails, which is
// what a Deployment held at zero replicas does.
const upFakeKubectl = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_KUBECTL_LOG"
case "$*" in
  *"get deployments"*|*"get deploy "*)
    while read -r name replicas; do
      [ -n "$name" ] || continue
      case "$*" in
        *custom-columns*) printf '%s   %s\n' "$name" "$replicas" ;;
        *) printf 'deployment.apps/%s\n' "$name" ;;
      esac
    done <<< "$FAKE_DEPLOYMENTS"
    exit 0 ;;
  *"get pods"*"-o json"*)
    # One pod not Ready, mid image pull -- the state a fresh install spends
    # most of its wait in, and the one the narration exists to surface.
    printf '{"items":[{"metadata":{"name":"identity-abc"},"status":{"phase":"Pending","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"state":{"waiting":{"reason":"ContainerCreating","message":"pulling image memql-identity"}}}]}}]}\n'
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

// runWaitForWorkloads sources up.sh, calls wait_for_workloads against the fake
// kubectl, and reports WORKLOADS_READY, everything kubectl was asked to do, and
// the combined log.
func runWaitForWorkloads(t *testing.T, deployments, unready string) (ready string, kubectlCalls string, log string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(upFakeKubectl), 0o755); err != nil {
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
		"MEMQL_K3D_WORKLOAD_TIMEOUT=1s",
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

// THE REGRESSION, memql#3585. An overlay holds some Deployments at zero
// replicas (cloud-entry holds `mcp` there today). Waiting for them meant
// clusterUp could not pass on a cluster where every other node was Available.
func TestWaitForWorkloadsIgnoresDeploymentsScaledToZero(t *testing.T) {
	deployments := "bff 1\nidentity 1\nmcp 0\nedge 0\n"
	ready, calls, log := runWaitForWorkloads(t, deployments, "mcp edge")

	if ready != "true" {
		t.Fatalf("WORKLOADS_READY = %q, want true -- every Deployment that is meant to be\n"+
			"running was Available; the two that were not are deliberately at zero replicas\noutput:\n%s",
			ready, log)
	}
	for _, gated := range []string{"deployment.apps/mcp", "deployment.apps/edge"} {
		for _, line := range strings.Split(calls, "\n") {
			if strings.Contains(line, "wait") && strings.Contains(line, gated) {
				t.Errorf("the wait was asked to wait for %s, which is scaled to 0:\n  %s", gated, line)
			}
		}
	}
}

// The other direction, so the fix cannot be "stop waiting". A node that is meant
// to be running and is not must still fail the verdict -- that is what
// workloadsReady was added for (memql#3570).
func TestWaitForWorkloadsStillFailsWhenSomethingScaledUpIsNotReady(t *testing.T) {
	deployments := "bff 1\nidentity 1\nmcp 0\n"
	ready, _, log := runWaitForWorkloads(t, deployments, "identity")

	if ready != "false" {
		t.Fatalf("WORKLOADS_READY = %q, want false -- identity is scaled to 1 and never became\n"+
			"Available, which is exactly the case this verdict exists to catch\noutput:\n%s", ready, log)
	}
}

// A namespace ArgoCD has not populated yet is not a healthy one.
func TestWaitForWorkloadsReportsAnEmptyNamespaceAsNotReady(t *testing.T) {
	ready, _, log := runWaitForWorkloads(t, "", "")
	if ready != "false" {
		t.Fatalf("WORKLOADS_READY = %q, want false with no Deployments at all\noutput:\n%s", ready, log)
	}
}

// A whole namespace deliberately at zero is not "everything is fine". Nothing
// creates that state today; asserting it keeps the replicas filter from being
// read as "an empty wait set passes".
func TestWaitForWorkloadsReportsAllZeroReplicasAsNotReady(t *testing.T) {
	ready, _, log := runWaitForWorkloads(t, "mcp 0\nedge 0\n", "")
	if ready != "false" {
		t.Fatalf("WORKLOADS_READY = %q, want false -- no workload is running at all\noutput:\n%s", ready, log)
	}
}
