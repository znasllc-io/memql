package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// up_reports_where_it_went_wrong_test.go -- memql#5029.
//
// THE DEFECT. `k3d.up` registered the ArgoCD Application, never read it back,
// and then ran a workload wait that is DELIBERATELY non-fatal (see
// wait_for_workloads: `make up` on a developer's machine must leave a cluster
// to inspect rather than abort inside one). So a source ArgoCD cannot fetch, a
// path that is not in the repo and an AppProject that refuses the source all
// produced the same transcript: every step green, the namespace empty,
// workloadsReady=false, the words "Bootstrap complete" printed over it, cap_ok,
// exit 0 -- and the only line saying anything was wrong arrived from the
// install graph one assertion later as `result.workloadsReady did not satisfy
// resultTrue`, which names a JSON path and points at the workloads, which were
// never the fault. Four CI runs were read as three different flakes.
//
// Three properties close it, and each is asserted below:
//
//  1. a source ArgoCD refuses fails HERE, with ArgoCD's own message;
//  2. a comparison still in flight does NOT fail -- a slow fetch is the same
//     class as a slow image pull, and believing a first-poll ComparisonError
//     would manufacture exactly the intermittency this issue is about;
//  3. the summary banner never claims completion over a false verdict, and the
//     reason travels in the result envelope so the executor can print it.

// appFakeKubectl answers `get application` from $FAKE_APP_CONDITIONS and
// $FAKE_APP_SYNC. Both are read fresh on every call, and $FAKE_APP_CLEAR_AFTER
// makes the conditions clear after N reads -- which is how a transient
// ComparisonError is expressed.
const appFakeKubectl = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_KUBECTL_LOG"
case "$*" in
  *"get application"*)
    reads=0
    [ -f "$FAKE_APP_READS" ] && reads="$(cat "$FAKE_APP_READS")"
    reads=$((reads + 1))
    printf '%s' "$reads" > "$FAKE_APP_READS"
    conditions="$FAKE_APP_CONDITIONS"
    if [ -n "${FAKE_APP_CLEAR_AFTER:-}" ] && [ "$reads" -gt "$FAKE_APP_CLEAR_AFTER" ]; then
      conditions=""
    fi
    case "$*" in
      *ComparisonError*) printf '%s' "$conditions" ;;
      *'status.sync.status'*) printf '%s' "${FAKE_APP_SYNC:-}" ;;
      *'status.health.status'*) printf 'Healthy' ;;
      *) printf '%s' "$conditions" ;;
    esac
    exit 0 ;;
esac
exit 0
`

// runUpFunc sources up.sh and runs one snippet against the fake kubectl,
// returning the combined output and the exit code.
func runUpFunc(t *testing.T, snippet string, env ...string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "kubectl"), []byte(appFakeKubectl), 0o755); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	harness := filepath.Join(tmp, "harness.sh")
	body := "#!/usr/bin/env bash\nset -uo pipefail\nsource \"" +
		filepath.Join(root, "scripts", "k3d", "up.sh") + "\"\n" + snippet + "\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	cmd := exec.Command("bash", harness)
	cmd.Dir = root
	cmd.Env = append(append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_KUBECTL_LOG="+filepath.Join(tmp, "kubectl.log"),
		"FAKE_APP_READS="+filepath.Join(tmp, "reads"),
	), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	t.Logf("exit=%d output:\n%s", code, out)
	return string(out), code
}

// PROPERTY 1. A source ArgoCD refuses is not a slow install and never becomes
// one. It fails here, with ArgoCD's own sentence, rather than as an empty
// namespace two steps later.
func TestAnUnreadableApplicationSourceFailsWhereItWentWrong(t *testing.T) {
	const argoSays = "rpc error: code = Unknown desc = authentication required"
	out, code := runUpFunc(t,
		"APP_NAME=memql-local\nARGOCD_NAMESPACE=argocd\nREPO_URL=https://example.test/x.git\n"+
			"TARGET_REVISION=abc123\nOVERLAY_PATH=deploy/k8s/overlays/local\nAPP_PROJECT=memql\n"+
			"APP_COMPARE_TIMEOUT=30\nwait_for_app_comparison\n",
		"FAKE_APP_CONDITIONS="+argoSays, "FAKE_APP_SYNC=")

	if code == 0 {
		t.Fatalf("wait_for_app_comparison exited 0 on an Application ArgoCD refuses -- which is the whole\n"+
			"defect: exit 0 here is what produced `Bootstrap complete` over an empty namespace\noutput:\n%s", out)
	}
	if !strings.Contains(out, argoSays) {
		t.Errorf("the failure does not carry ArgoCD's own message %q -- a diagnosis the operator cannot\n"+
			"read is the same as the JSON path this replaces\noutput:\n%s", argoSays, out)
	}
	for _, want := range []string{"https://example.test/x.git", "abc123", "deploy/k8s/overlays/local", "memql"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not name %q, so the operator cannot see WHICH source was refused\noutput:\n%s", want, out)
		}
	}
}

// PROPERTY 2, and it is the one that makes property 1 safe to ship. ArgoCD
// publishes a ComparisonError while its first fetch is in flight and clears it
// moments later. Believing a single poll would fail healthy installs at random
// -- manufacturing the intermittency this issue exists to remove.
func TestATransientComparisonErrorIsNotAFailure(t *testing.T) {
	out, code := runUpFunc(t,
		"APP_NAME=memql-local\nARGOCD_NAMESPACE=argocd\nREPO_URL=r\nTARGET_REVISION=v\n"+
			"OVERLAY_PATH=p\nAPP_PROJECT=memql\nAPP_COMPARE_TIMEOUT=30\nwait_for_app_comparison\n",
		"FAKE_APP_CONDITIONS=still fetching", "FAKE_APP_CLEAR_AFTER=1", "FAKE_APP_SYNC=Synced")

	if code != 0 {
		t.Fatalf("a ComparisonError that cleared on the next poll failed the bring-up (exit %d)\noutput:\n%s", code, out)
	}
	if !strings.Contains(out, "Synced") {
		t.Errorf("the success line does not report the sync status it observed\noutput:\n%s", out)
	}
}

// A comparison that never arrives is a slow fetch: recorded, never fatal. The
// same decision wait_for_workloads already makes, for the same reason.
func TestAComparisonThatNeverArrivesIsNotFatal(t *testing.T) {
	out, code := runUpFunc(t,
		"APP_NAME=memql-local\nARGOCD_NAMESPACE=argocd\nREPO_URL=r\nTARGET_REVISION=v\n"+
			"OVERLAY_PATH=p\nAPP_PROJECT=memql\nAPP_COMPARE_TIMEOUT=1\nwait_for_app_comparison\n"+
			"printf 'REACHED_THE_END\\n'\n",
		"FAKE_APP_CONDITIONS=", "FAKE_APP_SYNC=")

	if code != 0 {
		t.Fatalf("a comparison still in flight aborted the bring-up (exit %d) -- a slow fetch is the same\n"+
			"class as a slow image pull, and this script does not abort a developer's cluster over slow\noutput:\n%s", code, out)
	}
	if !strings.Contains(out, "REACHED_THE_END") {
		t.Errorf("execution did not continue past the wait\noutput:\n%s", out)
	}
}

// PROPERTY 3a. The banner is the line every reader of that transcript saw last,
// and it said the install worked.
func TestTheSummaryDoesNotClaimCompletionOverAFalseVerdict(t *testing.T) {
	out, _ := runUpFunc(t,
		"NAMESPACE=memql\nAPP_NAME=memql-local\nARGOCD_NAMESPACE=argocd\nCLUSTER_NAME=memql\n"+
			"ARGOCD_VERSION=v2\nTARGET_REVISION=v\nOVERLAY_PATH=p\n"+
			"WORKLOADS_READY=false\nWORKLOADS_REASON='ArgoCD applied no Deployments to memql at all'\n"+
			"print_summary\n")

	if strings.Contains(out, "Bootstrap complete") {
		t.Errorf("the summary still says \"Bootstrap complete\" with workloadsReady=false. That sentence,\n"+
			"printed over an empty namespace, is what sent two investigations at the workloads\noutput:\n%s", out)
	}
	if !strings.Contains(out, "NOT READY") {
		t.Errorf("the summary does not say the workloads are not ready\noutput:\n%s", out)
	}
	if !strings.Contains(out, "ArgoCD applied no Deployments") {
		t.Errorf("the summary does not carry the recorded reason\noutput:\n%s", out)
	}
}

// The other direction, so the fix cannot be "never say it".
func TestTheSummaryStillReportsCompletionWhenTheWorkloadsAreReady(t *testing.T) {
	out, _ := runUpFunc(t,
		"NAMESPACE=memql\nAPP_NAME=memql-local\nARGOCD_NAMESPACE=argocd\nCLUSTER_NAME=memql\n"+
			"ARGOCD_VERSION=v2\nTARGET_REVISION=v\nOVERLAY_PATH=p\n"+
			"WORKLOADS_READY=true\nWORKLOADS_REASON=\nprint_summary\n")

	if !strings.Contains(out, "Bootstrap complete") {
		t.Errorf("a healthy bring-up no longer reports completion\noutput:\n%s", out)
	}
	if strings.Contains(out, "NOT READY") {
		t.Errorf("a healthy bring-up warns that the workloads are not ready\noutput:\n%s", out)
	}
}

// The summary is also the operator's list of entry points, and it advertised
// the Portal for a while after epic memql#4984 deleted it.
func TestTheSummaryDoesNotAdvertiseARetiredSurface(t *testing.T) {
	out, _ := runUpFunc(t,
		"NAMESPACE=memql\nAPP_NAME=memql-local\nARGOCD_NAMESPACE=argocd\nCLUSTER_NAME=memql\n"+
			"ARGOCD_VERSION=v2\nTARGET_REVISION=v\nOVERLAY_PATH=p\nWORKLOADS_READY=true\n"+
			"WORKLOADS_REASON=\nprint_summary\n")

	if strings.Contains(strings.ToLower(out), "portal") {
		t.Errorf("the summary still names the Portal, retired in epic memql#4984\noutput:\n%s", out)
	}
	if !strings.Contains(out, "os.memql.localhost") {
		t.Errorf("the summary does not name MemQL OS, which is the console now\noutput:\n%s", out)
	}
}

// PROPERTY 3b. The reason has to LEAVE the script, or the executor is still
// printing a predicate name. wait_for_workloads records one on every path that
// sets the verdict false.
func TestTheNotReadyVerdictCarriesAReason(t *testing.T) {
	out, _ := runUpFunc(t,
		"NAMESPACE=memql\nAPP_NAME=memql-local\nARGOCD_NAMESPACE=argocd\n"+
			"MEMQL_K3D_WORKLOAD_TIMEOUT=1\nwait_for_workloads\n"+
			"printf 'REASON=%s\\n' \"$WORKLOADS_REASON\"\n",
		"FAKE_APP_CONDITIONS=", "FAKE_APP_SYNC=OutOfSync")

	var reason string
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "REASON="); ok {
			reason = v
		}
	}
	if strings.TrimSpace(reason) == "" {
		t.Fatalf("an empty namespace set workloadsReady=false and recorded NO reason, so the executor\n"+
			"falls back to naming the JSON path -- which is the defect\noutput:\n%s", out)
	}
	if !strings.Contains(reason, "no Deployments") {
		t.Errorf("the reason does not say what was actually wrong: %q", reason)
	}
	// It must also carry the Application's own state, or the operator has a
	// symptom with no next step.
	if !strings.Contains(reason, "memql-local") {
		t.Errorf("the reason does not name the Application whose sync produced nothing: %q", reason)
	}
}
