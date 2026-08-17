package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// up_wait_narration_test.go -- znasllc-io/memql#4073.
//
// The workload wait was one blocking `kubectl wait --timeout=$deadline`:
// correct, and silent for its whole life. On a fresh install that silence is
// the operator's entire experience -- a new k3d cluster is a new containerd,
// so EVERY image is pulled from GHCR over their connection. Measured on a real
// machine: 40 pulls, ~27 cumulative minutes, and the database operand's pull
// unable to even start until ArgoCD and the CNPG operator were themselves
// pulled and admitted. The cluster went healthy ~30 seconds AFTER the wait
// expired, and the install reported workloadsReady=false on a machine that
// was fine -- with nothing in the log saying what the ten minutes were spent
// on.
//
// Two fixes, both asserted here:
//
//   - the wait NARRATES: every ~30s it prints who is not Ready and the
//     container state's own reason, so a slow pull reads as a slow pull while
//     it happens and a timeout's last report is its diagnosis;
//   - the deadline is a PARAMETER (`--workload-timeout`), so the installer can
//     pass a fresh-pull budget (900s) while `make dev` -- images already
//     imported -- keeps the 300s default. CI already knew this number was
//     load-bearing (install-cluster-e2e.yml exports 720s); the installer had
//     no way to say it.
//
// Same seam as the rest of this file's package: up.sh is sourced and
// wait_for_workloads driven against the fake kubectl, which now answers the
// pods-json probe with one pod mid-pull.

// TestWaitNarratesWhatItIsWaitingOn is the memql#4073 headline: during the
// wait, the not-Ready pod and its reason are in the log -- not only after the
// verdict.
func TestWaitNarratesWhatItIsWaitingOn(t *testing.T) {
	deployments := "bff 1\nidentity 1\n"
	ready, _, log := runWaitForWorkloads(t, deployments, "identity")

	if ready != "false" {
		t.Fatalf("WORKLOADS_READY = %q, want false (identity never becomes Available)\noutput:\n%s", ready, log)
	}
	if !strings.Contains(log, "still waiting") {
		t.Fatalf("the wait never narrated progress -- no 'still waiting' line. The operator is\n"+
			"back to staring at a silent spinner for the length of the budget, which is the\n"+
			"exact experience memql#4073 exists to end.\noutput:\n%s", log)
	}
	if !strings.Contains(log, "identity-abc") || !strings.Contains(log, "ContainerCreating") {
		t.Fatalf("the narration does not name the pod and its container-state reason. Naming is\n"+
			"the point: 'ContainerCreating: pulling image' and 'startup probe failing' are\n"+
			"different problems with the same spinner.\noutput:\n%s", log)
	}
}

// TestWaitBudgetParamOutranksTheEnv pins the precedence: WORKLOAD_TIMEOUT (what
// main resolves from --workload-timeout) beats MEMQL_K3D_WORKLOAD_TIMEOUT. The
// harness exports the env at 1s; main-style resolution is simulated by setting
// the variable the param lands in.
func TestWaitBudgetParamOutranksTheEnv(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// Reuse the harness via a tiny wrapper: WORKLOAD_TIMEOUT=2 with the env at
	// 1s must print a 2s budget. (2s keeps the failing wait to one tick.)
	deployments := "bff 1\n"
	ready, _, log := runWaitForWorkloadsEnv(t, deployments, "bff", []string{"WORKLOAD_TIMEOUT=2"})
	if ready != "false" {
		t.Fatalf("WORKLOADS_READY = %q, want false\noutput:\n%s", ready, log)
	}
	if !strings.Contains(log, "up to 2s") {
		t.Fatalf("the param's budget (2s) did not outrank the env's (1s) -- the installer's\n"+
			"fresh-pull budget would be silently ignored.\noutput:\n%s", log)
	}
}

// TestUpDeclaresTheWorkloadTimeoutParam: an undeclared flag is an immediate
// exit 2 at run time, so the declaration IS the feature reaching the wire.
func TestUpDeclaresTheWorkloadTimeoutParam(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	out, err := exec.Command("bash", root+"/scripts/k3d/up.sh", "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "workload-timeout") {
		t.Fatalf("k3d.up does not declare workload-timeout -- the install plan passes it, so\n"+
			"every clusterUp would exit 2 with 'unknown flag'.\nspec:\n%s", out)
	}
}

// runWaitForWorkloadsEnv is runWaitForWorkloads with extra environment -- the
// seam for simulating main()'s param resolution, which sets WORKLOAD_TIMEOUT
// before the function runs.
func runWaitForWorkloadsEnv(t *testing.T, deployments, unready string, extra []string) (ready string, kubectlCalls string, log string) {
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
	cmd.Env = append(append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_KUBECTL_LOG="+calls,
		"FAKE_DEPLOYMENTS="+deployments,
		"FAKE_UNREADY="+unready,
		"MEMQL_K3D_WORKLOAD_TIMEOUT=1s",
	), extra...)
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
	b, _ := os.ReadFile(calls)
	t.Logf("output:\n%s", log)
	return ready, string(b), log
}
