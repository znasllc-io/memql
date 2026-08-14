package k3d

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// status_argocd_test.go -- memql#3817.
//
// WHAT HAPPENED. This machine's ArgoCD Application pointed at
// `feat/edge-deploy`. That branch merged and was deleted, and from that moment
// ArgoCD could not resolve its target:
//
//	ComparisonError: Failed to load target state: ... unable to resolve
//	'feat/edge-deploy' to a commit SHA
//
// `syncPolicy.automated` delivered nothing for over four hours. Every pod was
// Running, every readiness probe passed, `health.status` was `Healthy`, and a
// merged fix could not be observed on the cluster because nothing had reached
// it. The honest-looking conclusion from outside is "the fix did not work".
//
// WHY `make status` DID NOT CATCH IT, WHICH IS THE INTERESTING PART. It was
// already PRINTING the answer. check_argocd ran `kubectl get application` with
// a custom-columns format including SYNC, and wrote `SYNC: Unknown` to stderr
// on every run -- then `return 0` unconditionally, set no result field, and let
// main() reach cap_ok. So the envelope said ok:true over a cluster that had
// stopped reconciling, with the evidence on screen.
//
// That is a stricter version of the pattern this repo keeps finding: not a
// check that measured the wrong thing, but a DISPLAY mistaken for a check. It
// looks like coverage in a way an assertion-free print never earns.
//
// health.status: Healthy alongside sync.status: Unknown is the specific trap.
// Health describes the workloads that ARE running, and they were fine. It says
// nothing about whether they are what the repository asks for.

type argocdStatusResult struct {
	OK     bool `json:"ok"`
	Result struct {
		ArgoCDSync           string `json:"argocdSync"`
		ArgoCDHealth         string `json:"argocdHealth"`
		ArgoCDTargetRevision string `json:"argocdTargetRevision"`
		ArgoCDReconciling    *bool  `json:"argocdReconciling"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type argocdScenario struct {
	sync       string
	health     string
	target     string
	conditions string // the jsonpath'd condition types, e.g. "ComparisonError"
}

// runStatusArgoCD drives the real status.sh with an ArgoCD Application present.
func runStatusArgoCD(t *testing.T, sc argocdScenario) (argocdStatusResult, string, int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "k3d", "status.sh")
	tmp := t.TempDir()
	for name, body := range map[string]string{"k3d": fakeStatusK3d, "kubectl": fakeStatusKubectl} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = root
	cmd.Env = []string{
		"PATH=" + tmp + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + tmp,
		"MEMQL_K3D_NAMESPACE=memql",
		"MEMQL_K3D_CLUSTER=memql",
		"FAKE_IDENTITY_PODS=",
		"FAKE_ARGOCD=1",
		"FAKE_ARGOCD_SYNC=" + sc.sync,
		"FAKE_ARGOCD_HEALTH=" + sc.health,
		"FAKE_ARGOCD_TARGET=" + sc.target,
		"FAKE_ARGOCD_CONDITIONS=" + sc.conditions,
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running status.sh: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Logf("exit=%d\nstdout: %s\nstderr:\n%s", code, stdout.String(), stderr.String())

	var res argocdStatusResult
	if line := strings.TrimSpace(stdout.String()); line != "" {
		if jerr := json.Unmarshal([]byte(lastJSONObject(line)), &res); jerr != nil {
			t.Fatalf("stdout is not a JSON envelope: %v\nstdout: %s", jerr, line)
		}
	}
	return res, stderr.String(), code
}

// TestStatusFailsWhenArgoCDCannotReconcile is the regression, and it uses the
// exact condition that was live: a ComparisonError from a targetRevision that
// no longer resolves.
func TestStatusFailsWhenArgoCDCannotReconcile(t *testing.T) {
	res, stderr, code := runStatusArgoCD(t, argocdScenario{
		sync:       "Unknown",
		health:     "Healthy", // the trap: healthy workloads, stale manifests
		target:     "feat/edge-deploy",
		conditions: "ComparisonError",
	})

	if code == 0 {
		t.Errorf("status exited 0 over an Application that cannot reconcile.\n"+
			"That is the memql#3817 defect exactly: the cluster runs a frozen manifest "+
			"set, every pod is Running and health is Healthy, and the litmus reports "+
			"success.\nstderr:\n%s", stderr)
	}
	if res.OK {
		t.Error("the envelope reports ok:true over a cluster that has stopped reconciling")
	}
	// Case-insensitive: the message shouts NOT RECONCILING, and the assertion
	// is about what it says rather than how it is typeset.
	if res.Error == nil || !strings.Contains(strings.ToLower(res.Error.Message), "reconcil") {
		t.Errorf("the failure message does not say the cluster is not reconciling: %+v", res.Error)
	}
	// The message has to name the branch, because "cannot reconcile" without
	// the targetRevision leaves an operator with nothing to act on -- and the
	// remedy is entirely determined by which revision it is stuck on.
	if res.Error != nil && !strings.Contains(res.Error.Message, "feat/edge-deploy") {
		t.Errorf("the failure message does not name the unresolvable targetRevision, which "+
			"is the one fact that determines the remedy: %+v", res.Error)
	}
	if res.Result.ArgoCDReconciling == nil || *res.Result.ArgoCDReconciling {
		t.Errorf("argocdReconciling = %v, want false", res.Result.ArgoCDReconciling)
	}
}

// TestStatusPassesWhenArgoCDIsSynced is the other side. Without it, a check
// that failed unconditionally would satisfy the test above.
func TestStatusPassesWhenArgoCDIsSynced(t *testing.T) {
	res, stderr, code := runStatusArgoCD(t, argocdScenario{
		sync: "Synced", health: "Healthy", target: "main", conditions: "",
	})

	if code != 0 {
		t.Fatalf("status exited %d over a healthy, synced Application; want 0.\nstderr:\n%s",
			code, stderr)
	}
	if !res.OK {
		t.Error("envelope ok=false for a synced cluster")
	}
	if res.Result.ArgoCDSync != "Synced" {
		t.Errorf("argocdSync = %q, want %q", res.Result.ArgoCDSync, "Synced")
	}
	if res.Result.ArgoCDReconciling == nil || !*res.Result.ArgoCDReconciling {
		t.Errorf("argocdReconciling = %v, want true", res.Result.ArgoCDReconciling)
	}
}

// TestStatusReportsATrackedBranchWithoutFailing pins the deliberate case.
//
// Pointing the Application at a feature branch to test an overlay change is
// legitimate and common -- it is how the wedge came about in the first place.
// So it must not fail. But it must not be INVISIBLE either: the whole incident
// is that "this cluster is tracking a branch" was a fact nobody could see
// without going and asking for it.
//
// So: reported as a result field, warned about on stderr, exit 0.
func TestStatusReportsATrackedBranchWithoutFailing(t *testing.T) {
	res, stderr, code := runStatusArgoCD(t, argocdScenario{
		sync: "Synced", health: "Healthy", target: "feat/some-experiment", conditions: "",
	})

	if code != 0 {
		t.Fatalf("status exited %d over a cluster deliberately tracking a branch; want 0. "+
			"Testing an overlay change from a branch is legitimate.\nstderr:\n%s", code, stderr)
	}
	if res.Result.ArgoCDTargetRevision != "feat/some-experiment" {
		t.Errorf("argocdTargetRevision = %q, want the tracked branch reported so the fact is "+
			"visible rather than latent", res.Result.ArgoCDTargetRevision)
	}
	if !strings.Contains(stderr, "feat/some-experiment") {
		t.Errorf("the operator is not told the cluster tracks a branch rather than main.\n"+
			"That fact outliving the branch is what produced memql#3817.\nstderr:\n%s", stderr)
	}
}

// TestStatusFlagsOutOfSyncWithoutFailing keeps the failure reserved for
// "cannot reconcile".
//
// OutOfSync means ArgoCD has compared successfully and found drift -- it knows
// what to do and auto-sync will do it. Failing the litmus on a state that
// resolves itself in seconds would make `make status` flaky, and a litmus
// people learn to re-run is a litmus they stop reading.
func TestStatusFlagsOutOfSyncWithoutFailing(t *testing.T) {
	res, stderr, code := runStatusArgoCD(t, argocdScenario{
		sync: "OutOfSync", health: "Healthy", target: "main", conditions: "",
	})

	if code != 0 {
		t.Errorf("status exited %d over an OutOfSync Application; want 0. OutOfSync means the "+
			"comparison SUCCEEDED and found drift, which auto-sync resolves -- unlike a "+
			"ComparisonError, where nothing will happen at all.\nstderr:\n%s", code, stderr)
	}
	if res.Result.ArgoCDSync != "OutOfSync" {
		t.Errorf("argocdSync = %q, want %q reported", res.Result.ArgoCDSync, "OutOfSync")
	}
	if res.Result.ArgoCDReconciling == nil || !*res.Result.ArgoCDReconciling {
		t.Error("argocdReconciling should be true for OutOfSync: ArgoCD is comparing fine, " +
			"it has simply found work to do")
	}
}
